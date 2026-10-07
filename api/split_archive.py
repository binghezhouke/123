"""A seekable, read-only 7z stream over numbered files in one cloud directory."""

import hashlib
import io
import re
import struct
import time
from bisect import bisect_right
from dataclasses import dataclass, field
from threading import Lock

from .zip_preview import ChangedArchive, INDEX_LIMIT, RangeReader, RangeSnapshot, ZipPreviewError

SPLIT_7Z = re.compile(r"^(.*\.7z)\.(\d{3,})$", re.IGNORECASE)
MAX_PARTS = 1000


@dataclass(frozen=True)
class Part:
    file_id: int
    name: str
    size: int
    etag: str


@dataclass
class VolumeSet:
    parts: tuple
    urls: dict = field(default_factory=dict)
    lock: object = field(default_factory=Lock)

    def url(self, index, resolve):
        # URL resolution is cheap relative to content reads; serialize first requests
        # so neighbouring prefetch requests do not request the same link twice.
        with self.lock:
            if index not in self.urls:
                result = resolve(self.parts[index].file_id)
                if not result:
                    raise ZipPreviewError(f"无法获取分卷下载链接：{self.parts[index].name}")
                self.urls[index] = result[0]
            return self.urls[index]

    @property
    def identity(self):
        return hashlib.sha256(repr(self.parts).encode()).hexdigest()


def discover_volumes(client, file):
    match = SPLIT_7Z.fullmatch(file.filename)
    if not match:
        raise ZipPreviewError("不是 7z 分卷文件")
    stem = match[1]
    found, cursor, seen = {}, None, set()
    for _ in range(1000):
        rows, following = client.list_files(
            parent_id=file.get("parentFileId", 0), limit=100, last_file_id=cursor, use_cache=False
        )
        for row in rows:
            candidate = SPLIT_7Z.fullmatch(row.filename)
            if row.is_folder or not candidate or candidate[1] != stem:
                continue
            number = int(candidate[2])
            if number < 1 or number > MAX_PARTS:
                raise ZipPreviewError("分卷编号无效或超过 1000 卷上限")
            if number in found:
                raise ZipPreviewError(f"存在重复分卷编号：{number:03}")
            if row.size <= 0:
                raise ZipPreviewError(f"分卷大小无效：{row.filename}")
            found[number] = Part(row.file_id, row.filename, row.size, row.get("etag") or "")
        if following in (None, -1):
            break
        if following in seen:
            raise ZipPreviewError("目录分页游标重复，无法确认分卷完整性")
        seen.add(following)
        cursor = following
    else:
        raise ZipPreviewError("目录过大，无法确认分卷完整性")
    if not found:
        raise ZipPreviewError("同目录未找到分卷")
    for number in range(1, max(found) + 1):
        if number not in found:
            raise ZipPreviewError(f"缺少分卷：{stem}.{number:03}")
    return VolumeSet(tuple(found[n] for n in sorted(found)))


@dataclass(frozen=True)
class SplitSnapshot(RangeSnapshot):
    part_validators: tuple = ()
    identity: str = ""

    @property
    def version(self):
        return hashlib.sha256((self.identity + super().version).encode()).hexdigest()[:24]


class SplitRangeReader(io.RawIOBase):
    def __init__(self, volumes, resolve, snapshot=None, record=False):
        self.volumes, self.resolve, self.record = volumes, resolve, record
        self.position = 0
        self.offsets = [0]
        for part in volumes.parts:
            self.offsets.append(self.offsets[-1] + part.size)
        self.size = self.offsets[-1]
        self.remaining = INDEX_LIMIT * 2
        self.deadline = time.monotonic() + 60
        self.read_ahead = 0
        self.readers = {}
        self.blocks = snapshot.blocks if snapshot else ()
        self.buffer_start, self.buffer = 0, b""
        self.recorded = []
        self.validators = dict(snapshot.part_validators) if snapshot else {}
        if snapshot and (snapshot.identity != volumes.identity or snapshot.size != self.size):
            raise ChangedArchive("分卷列表已变化，请刷新目录")

    def close(self):
        for reader in self.readers.values():
            reader.close()
        super().close()

    def readable(self):
        return True

    def seekable(self):
        return True

    def tell(self):
        return self.position

    def seek(self, offset, whence=0):
        if whence not in (0, 1, 2):
            raise OSError("偏移无效")
        position = offset + (self.position if whence == 1 else self.size if whence == 2 else 0)
        if position < 0:
            raise OSError("偏移无效")
        self.position = position
        return position

    def snapshot(self):
        usable = bool(self.validators) and all(self.validators.values())
        validator = hashlib.sha256(repr(sorted(self.validators.items())).encode()).hexdigest() if usable else None
        return SplitSnapshot(
            self.size, validator, tuple(self.recorded), tuple(sorted(self.validators.items())), self.volumes.identity
        )

    def read(self, size=-1):
        size = self.size - self.position if size is None or size < 0 else min(size, self.size - self.position)
        if size <= 0:
            return b""
        if size > INDEX_LIMIT or size > self.remaining or time.monotonic() > self.deadline:
            raise ZipPreviewError("分卷读取超过大小或时间限制")
        start = self.position
        for offset, data in ((self.buffer_start, self.buffer), *self.blocks):
            if offset <= start and start + size <= offset + len(data):
                self.position += size
                return data[start - offset : start - offset + size]
        requested = size
        size = min(max(size, self.read_ahead), self.size - start, INDEX_LIMIT)
        if size > self.remaining:
            raise ZipPreviewError("分卷读取超过大小限制")
        chunks = []
        while size:
            index = bisect_right(self.offsets, self.position) - 1
            part = self.volumes.parts[index]
            relative = self.position - self.offsets[index]
            count = min(size, part.size - relative)
            if index not in self.readers:
                expected = RangeSnapshot(part.size, self.validators.get(index), ())
                self.readers[index] = RangeReader(self.volumes.url(index, self.resolve), snapshot=expected)
            reader = self.readers[index]
            reader.deadline = self.deadline
            reader.remaining = self.remaining
            data = reader._fetch(relative, relative + count - 1)
            self.validators[index] = reader.validator
            self.remaining -= len(data)
            chunks.append(data)
            self.position += len(data)
            size -= len(data)
        result = b"".join(chunks)
        if self.record:
            self.recorded.append((start, result))
        self.buffer_start, self.buffer = start, result
        self.position = start + requested
        return result[:requested]

    def check_header(self):
        self.seek(0)
        header = self.read(32)
        if len(header) != 32 or header[:6] != b"7z\xbc\xaf\x27\x1c":
            raise ZipPreviewError("首卷不是有效的 7z 文件")
        offset, length = struct.unpack_from("<QQ", header, 12)
        if 32 + offset + length > self.size:
            raise ZipPreviewError("缺少末尾分卷或分卷数据被截断，请确认已上传全部分卷")
        self.seek(0)
