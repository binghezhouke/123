"""Per-application, bounded archive-link/index cache with single-flight loading.

Only immutable HTTP index bytes are shared. Parsers, cursors and HTTP sessions
are created separately per request, so concurrent extraction is safe.
"""

import logging
from collections import OrderedDict
from concurrent.futures import Future
from dataclasses import dataclass
from threading import Lock
from time import monotonic

from .archive_preview import open_archive
from .zip_preview import ChangedArchive, ExpiredArchiveLink, ZipPreviewError


logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class CachedArchive:
    url: object
    snapshot: object


class ArchiveCache:
    def __init__(self, ttl=300, capacity=16, max_bytes=64 * 1024 * 1024):
        self.ttl, self.capacity, self.max_bytes = ttl, capacity, max_bytes
        self.entries = OrderedDict()
        self.pending = {}
        self.lock = Lock()
        self.byte_size = 0

    def _remove(self, key):
        previous = self.entries.pop(key, None)
        if previous:
            self.byte_size -= previous[1].snapshot.byte_size

    def invalidate(self, key, expected=None):
        with self.lock:
            old = self.entries.get(key)
            if expected is None or (old and old[1] is expected):
                self._remove(key)
            # In-flight callers can finish, but must not repopulate after refresh.
            if expected is None:
                self.pending.pop(key, None)

    def get(self, key, loader):
        with self.lock:
            old = self.entries.get(key)
            if old and monotonic() - old[0] < self.ttl:
                self.entries.move_to_end(key)
                logger.info("压缩包直链与索引缓存命中")
                return old[1]
            self._remove(key)
            future = self.pending.get(key)
            owner = future is None
            if owner:
                future = Future()
                self.pending[key] = future
        if not owner:
            logger.info("等待同一压缩包的索引加载")
            return future.result()
        try:
            logger.info("加载压缩包直链与索引")
            value = loader()
            with self.lock:
                if self.pending.get(key) is future:
                    del self.pending[key]
                    if value.snapshot.byte_size <= self.max_bytes:
                        self._remove(key)
                        self.entries[key] = (monotonic(), value)
                        self.byte_size += value.snapshot.byte_size
                        while len(self.entries) > self.capacity or self.byte_size > self.max_bytes:
                            self._remove(next(iter(self.entries)))
            future.set_result(value)
            return value
        except BaseException as exc:
            with self.lock:
                if self.pending.get(key) is future:
                    del self.pending[key]
            future.set_exception(exc)
            raise

    def run(self, key, kind, get_url, operation, refresh=False, resolve_part=None, password=None):
        if refresh:
            self.invalidate(key)

        def load():
            result = get_url()
            if not result:
                raise ZipPreviewError("获取压缩包下载地址失败，请重试")
            with open_archive(result[0], kind, record=True, resolve_part=resolve_part, password=password) as (
                _,
                source,
            ):
                return CachedArchive(result[0], source.snapshot())

        cached = self.get(key, load)
        for attempt in range(2):
            try:
                # Without a validator, never mix an old index with newly fetched bytes.
                snapshot = cached.snapshot if cached.snapshot.validator else None
                with open_archive(
                    cached.url,
                    kind,
                    snapshot=snapshot,
                    record=snapshot is None,
                    resolve_part=resolve_part,
                    password=password,
                ) as (archive, source):
                    source.index_version = (snapshot or source.snapshot()).version
                    return operation(archive, source)
            except ChangedArchive:
                self.invalidate(key, cached)
                raise
            except ExpiredArchiveLink:
                logger.info("压缩包直链失效，刷新缓存")
                self.invalidate(key, cached)
                if attempt:
                    raise
                updated = self.get(key, load)
                if (
                    updated.snapshot.size != cached.snapshot.size
                    or updated.snapshot.validator != cached.snapshot.validator
                    or updated.snapshot.version != cached.snapshot.version
                ):
                    raise ChangedArchive("压缩包已变化，请刷新目录后重新打开")
                cached = updated
