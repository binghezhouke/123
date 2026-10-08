import io
import os
import re
from types import SimpleNamespace

import py7zr
import pytest

from api.archive_cache import ArchiveCache
from api.archive_preview import read_archive_member
from api.models import File
from api.split_archive import Part, VolumeSet, discover_volumes, SplitRangeReader
from api.zip_preview import ZipPreviewError


def file(number, size=100):
    return File(
        {
            "fileId": number,
            "filename": f"photos.7z.{number:03}",
            "size": size,
            "type": 0,
            "parentFileId": 42,
            "etag": str(number),
        }
    )


def test_discovery_paginates_and_sorts():
    calls = []

    def listing(**kw):
        calls.append(kw)
        return ([file(3), file(1)], 10) if kw["last_file_id"] is None else ([file(2)], -1)

    volumes = discover_volumes(SimpleNamespace(list_files=listing), file(1))
    assert [p.file_id for p in volumes.parts] == [1, 2, 3]
    assert calls[1]["last_file_id"] == 10
    assert calls[0]["parent_id"] == 42


@pytest.mark.parametrize(
    "rows,message",
    [([file(1), file(3)], "002"), ([file(2)], "001"), ([file(1), file(1)], "重复"), ([file(1, 0)], "大小无效")],
)
def test_missing_duplicate_and_invalid_parts(rows, message):
    with pytest.raises(ZipPreviewError, match=message):
        discover_volumes(SimpleNamespace(list_files=lambda **kw: (rows, -1)), file(1))


@pytest.fixture
def transport(monkeypatch):
    state = SimpleNamespace(payloads={}, calls=[], expired=set())

    class Response:
        def __init__(self, url, start, end):
            data = state.payloads[url]
            self.headers = {"Content-Range": f"bytes {start}-{end}/{len(data)}", "ETag": '"v1"'}
            self.status_code = 403 if url in state.expired else 206
            self.data = data[start : end + 1]

        def __enter__(self):
            return self

        def __exit__(self, *args):
            pass

        def iter_content(self, size):
            yield self.data

    def get(self, url, headers, **kwargs):
        start, end = map(int, re.findall(r"\d+", headers["Range"]))
        state.calls.append((url, start, end))
        return Response(url, start, end)

    monkeypatch.setattr("requests.Session.get", get)
    return state


def make_volumes(transport, payload, volume_size):
    parts = []
    for offset in range(0, len(payload), volume_size):
        index = len(parts) + 1
        content = payload[offset : offset + volume_size]
        transport.payloads[f"https://part/{index}"] = content
        parts.append(Part(index, f"a.7z.{index:03}", len(content), str(index)))
    return VolumeSet(tuple(parts))


def test_random_read_crosses_boundaries_and_resolves_lazily(transport):
    volumes = make_volumes(transport, b"abcdefghijklmnop", 4)
    urls = []

    def resolve(index):
        urls.append(index)
        return (f"https://part/{index}", "api")

    with SplitRangeReader(volumes, resolve) as source:
        source.seek(3)
        assert source.read(6) == b"defghi"
        source.seek(-2, 2)
        assert source.read() == b"op"
        source.seek(20)
        assert source.read() == b""
    assert urls == [1, 2, 3, 4]
    with SplitRangeReader(volumes, resolve) as source:
        source.read(1)
    assert urls == [1, 2, 3, 4]


@pytest.mark.parametrize("password", [None, "fixture-only"])
def test_real_split_7z_extracts_cross_volume_file_and_reuses_cache(transport, password):
    expected = os.urandom(600000)
    data = io.BytesIO()
    with py7zr.SevenZipFile(
        data,
        "w",
        filters=([{"id": py7zr.FILTER_COPY}] if password is None else None),
        password=password,
        header_encryption=bool(password),
    ) as archive:
        archive.writestr(expected, "photo.jpg")
    volumes = make_volumes(transport, data.getvalue(), 100000)
    links, indexes = [], []

    def resolve(index):
        links.append(index)
        return (f"https://part/{index}", "api")

    def load():
        indexes.append(1)
        return (volumes, "split")

    cache = ArchiveCache()
    names = cache.run(
        1, ".7z", load, lambda a, s: [e.filename for e in a.infolist()], resolve_part=resolve, password=password
    )
    assert names == ["photo.jpg"]
    # Listing touches the start and end, not every intervening volume.
    assert len(links) < len(volumes.parts)

    def read(a, s):
        return read_archive_member(a, s, a.infolist()[0])

    assert cache.run(1, ".7z", load, read, resolve_part=resolve, password=password) == expected
    assert cache.run(1, ".7z", load, read, resolve_part=resolve, password=password) == expected
    if password:
        from api.zip_password_validation import validate_archive_password
        assert cache.run(1, ".7z", load, validate_archive_password,
                         resolve_part=resolve, password=password) is None
    assert len(indexes) == 1
    assert len(links) == len(set(links))


def test_missing_last_volume_detected_from_7z_header(transport):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"hello", "hello.txt")
    volumes = make_volumes(transport, data.getvalue(), 40)
    volumes.parts = volumes.parts[:-1]
    with SplitRangeReader(volumes, lambda n: (f"https://part/{n}", "api")) as source:
        with pytest.raises(ZipPreviewError, match="末尾分卷"):
            source.check_header()


def test_expired_middle_volume_refreshes_group_once(transport):
    data = io.BytesIO()
    expected = os.urandom(600000)
    with py7zr.SevenZipFile(data, "w", filters=[{"id": py7zr.FILTER_COPY}]) as archive:
        archive.writestr(expected, "photo.jpg")
    original = make_volumes(transport, data.getvalue(), 100000)
    cache, loads = ArchiveCache(), []

    def load():
        loads.append(1)
        if len(loads) > 1:
            transport.expired.clear()
        return (VolumeSet(original.parts), "split")

    def resolve(index):
        return (f"https://part/{index}", "api")

    cache.run(1, ".7z", load, lambda a, s: None, resolve_part=resolve)
    transport.expired.add("https://part/4")
    actual = cache.run(1, ".7z", load, lambda a, s: read_archive_member(a, s, a.infolist()[0]), resolve_part=resolve)
    assert actual == expected
    assert len(loads) == 2
