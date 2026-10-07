from concurrent.futures import ThreadPoolExecutor
from threading import Event

import pytest

import test_zip_preview
from api.archive_cache import ArchiveCache, CachedArchive
from api.archive_preview import read_archive_member
from api.zip_preview import ChangedArchive, RangeSnapshot

remote_fixture = test_zip_preview.remote


@pytest.fixture
def remote(remote_fixture):
    remote_fixture.data = test_zip_preview.make_zip([("a.txt", b"alpha"), ("b.txt", b"beta")])
    return remote_fixture


def test_cached_link_and_index_no_repeated_index_requests(remote):
    # Ensure the tail/index cache does not encompass the selected member.
    import zipfile

    remote.data = test_zip_preview.make_zip([("a.txt", b"alpha"), ("padding", b"x" * 200000)], zipfile.ZIP_STORED)
    cache = ArchiveCache()
    calls = []

    def url():
        calls.append(1)
        return ("https://example.test/a", "api")

    def read(archive, source):
        return read_archive_member(archive, source, archive.infolist()[0])

    cache.run(1, ".zip", url, lambda a, s: a.infolist())
    ranges = len(remote.ranges)
    assert cache.run(1, ".zip", url, read) == b"alpha"
    later = remote.ranges[ranges:]
    assert later and all(end < 100 for start, end in later)
    assert len(calls) == 1
    cache.run(1, ".zip", url, read, refresh=True)
    assert len(calls) == 2


def test_concurrent_misses_load_once():
    cache = ArchiveCache()
    entered, release = Event(), Event()
    calls = []
    value = CachedArchive("url", RangeSnapshot(10, "etag", ((0, b"bytes"),)))

    def loader():
        calls.append(1)
        entered.set()
        assert release.wait(5)
        return value

    with ThreadPoolExecutor(8) as pool:
        futures = [pool.submit(cache.get, "a", loader) for _ in range(8)]
        assert entered.wait(5)
        release.set()
        assert all(f.result() is value for f in futures)
    assert len(calls) == 1


def test_ttl_byte_limit_and_failed_load(monkeypatch):
    import api.archive_cache as module

    now = [100]
    monkeypatch.setattr(module, "monotonic", lambda: now[0])
    cache = ArchiveCache(ttl=10, capacity=3, max_bytes=6)
    value = CachedArchive("url", RangeSnapshot(10, "etag", ((0, b"1234"),)))
    cache.get(1, lambda: value)
    cache.get(2, lambda: value)
    assert list(cache.entries) == [2]
    assert cache.byte_size == 4
    now[0] += 11
    with pytest.raises(ValueError):
        cache.get(2, lambda: (_ for _ in ()).throw(ValueError("failed")))
    assert not cache.pending
    assert cache.get(2, lambda: value) is value


def test_expired_link_reloads_once_and_changed_content_rejected(remote):
    import zipfile

    remote.data = test_zip_preview.make_zip([("a", b"alpha"), ("pad", b"x" * 200000)], zipfile.ZIP_STORED)
    cache = ArchiveCache()
    urls = []

    def url():
        urls.append(1)
        remote.status = 206
        return ("https://example.test/a", "api")

    def read(a, s):
        return read_archive_member(a, s, a.infolist()[0])

    cache.run(1, ".zip", url, lambda a, s: None)
    remote.status = 403
    assert cache.run(1, ".zip", url, read) == b"alpha"
    assert len(urls) == 2
    remote.etag = '"new-version"'
    with pytest.raises(ChangedArchive):
        cache.run(1, ".zip", url, read)
    assert 1 not in cache.entries


def test_no_validator_reuses_link_but_reads_fresh_index(remote):
    remote.etag = None
    cache = ArchiveCache()
    calls = []

    def url():
        calls.append(1)
        return ("https://example.test/a", "api")

    cache.run(1, ".zip", url, lambda a, s: None)
    before = len(remote.ranges)
    cache.run(1, ".zip", url, lambda a, s: None)
    assert len(remote.ranges) > before
    assert len(calls) == 1


def test_refresh_during_load_prevents_stale_repopulation():
    cache = ArchiveCache()
    entered, release = Event(), Event()
    old = CachedArchive("old", RangeSnapshot(3, "old", ((0, b"old"),)))
    new = CachedArchive("new", RangeSnapshot(3, "new", ((0, b"new"),)))

    def loader():
        entered.set()
        assert release.wait(5)
        return old

    with ThreadPoolExecutor() as pool:
        task = pool.submit(cache.get, 1, loader)
        assert entered.wait(5)
        cache.invalidate(1)
        assert cache.get(1, lambda: new) is new
        release.set()
        assert task.result() is old
    assert cache.get(1, lambda: old) is new


def test_cache_replay_for_7z_and_rar(remote):
    import io
    from pathlib import Path
    import py7zr

    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"hello", "hello.txt")
    for kind, payload in [
        (".7z", data.getvalue()),
        (".rar", (Path(__file__).parent / "fixtures/rar/rar3-solid.rar").read_bytes()),
    ]:
        remote.data = payload
        cache = ArchiveCache()

        def url():
            return ("https://example.test/a", "api")

        names = cache.run(kind, kind, url, lambda a, s: [e.filename for e in a.infolist()])
        before = len(remote.ranges)
        assert cache.run(kind, kind, url, lambda a, s: [e.filename for e in a.infolist()]) == names
        assert len(remote.ranges) == before
        if kind == ".7z":
            assert cache.run(kind, kind, url, lambda a, s: read_archive_member(a, s, a.infolist()[0])) == b"hello"
