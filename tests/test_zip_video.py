import io
import zipfile

import pyzipper
import pytest

import test_zip_preview
from api.zip_preview import FILE_LIMIT
from routes.zip_media import STREAM_CHUNK

remote = test_zip_preview.remote
browser = test_zip_preview.browser


@pytest.fixture
def movie(remote):
    data = bytes(range(256)) * 4096
    remote.data = test_zip_preview.make_zip([("旅行/movie.mp4", data), ("other.bin", b"other")], zipfile.ZIP_STORED)
    return data


def test_video_player_has_versioned_stream_url_and_parent(browser, remote, movie):
    listing = browser.get("/file/1/zip?path=旅行/")
    assert "播放视频".encode() in listing.data
    page = browser.get("/file/1/zip/member/0")
    assert page.status_code == 200
    assert b"<video" in page.data
    assert b"stream=1" in page.data
    assert b"v=" in page.data
    assert b'preload="metadata"' in page.data
    assert b"%E6%97%85%E8%A1%8C" in page.data
    assert sum(end - start + 1 for start, end in remote.ranges) < 150000


@pytest.mark.parametrize(
    "header,start,stop",
    [
        ("bytes=0-1", 0, 2),
        ("bytes=300000-300999", 300000, 301000),
        ("bytes=-128", -128, None),
        ("bytes=1000000-", 1000000, None),
        ("bytes=1048500-9999999", 1048500, None),
    ],
)
def test_video_range_is_relative_to_member(browser, remote, movie, header, start, stop):
    response = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": header})
    assert response.status_code == 206
    assert response.data == movie[start:stop]
    assert response.headers["Content-Type"].startswith("video/mp4")
    assert response.headers["Content-Length"] == str(len(movie[start:stop]))
    assert response.headers["Content-Range"] == f"bytes {start % len(movie)}-{(stop or len(movie)) - 1}/{len(movie)}"
    assert response.headers["Accept-Ranges"] == "bytes"


@pytest.mark.parametrize(
    "header", ["bytes=9999999-", "bytes=20-10", "bytes=-0", "garbage", "items=0-1", "bytes=0-1,3-4"]
)
def test_invalid_or_unsupported_ranges_return_416(browser, movie, header):
    response = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": header})
    assert response.status_code == 416
    assert response.headers["Content-Range"] == f"bytes */{len(movie)}"
    assert response.data == b""


def test_full_video_stream_and_head(browser, remote, movie):
    response = browser.head("/file/1/zip/member/0?stream=1")
    assert response.status_code == 200
    assert response.data == b""
    assert response.headers["Content-Length"] == str(len(movie))
    assert sum(end - start + 1 for start, end in remote.ranges) < 150000
    response = browser.get("/file/1/zip/member/0?stream=1")
    assert response.status_code == 200
    assert response.data == movie
    assert "Content-Range" not in response.headers
    response = browser.get("/file/1/zip/member/0?download=1", headers={"Range": "bytes=0-5"})
    assert response.data == movie[:6]
    assert response.headers["Content-Disposition"].startswith("attachment")


def test_large_video_stream_stays_bounded_and_closes_on_disconnect(browser, remote, monkeypatch):
    data = b"x" * (FILE_LIMIT + 1024)
    remote.data = test_zip_preview.make_zip([("big.mp4", data)], zipfile.ZIP_STORED)
    closed = []
    from api.zip_preview import RangeReader

    original_close = RangeReader.close

    def close(reader):
        closed.append(reader)
        original_close(reader)

    monkeypatch.setattr(RangeReader, "close", close)
    listing = browser.get("/file/1/zip")
    assert "播放视频".encode() in listing.data
    response = browser.get("/file/1/zip/member/0?stream=1", buffered=False)
    assert response.headers["Content-Length"] == str(len(data))
    assert len(next(response.response)) == STREAM_CHUNK
    transferred = sum(end - start + 1 for start, end in remote.ranges)
    assert transferred < STREAM_CHUNK + 150000
    before = len(closed)
    response.close()
    assert len(closed) > before
    assert closed[-1].closed
    assert sum(end - start + 1 for start, end in remote.ranges) == transferred


@pytest.mark.parametrize("encrypted", [False, True])
def test_compressed_or_encrypted_video_is_not_streamed(browser, remote, encrypted):
    if encrypted:
        buffer = io.BytesIO()
        with pyzipper.AESZipFile(buffer, "w", compression=zipfile.ZIP_STORED, encryption=pyzipper.WZ_AES) as archive:
            archive.setpassword(b"fixture")
            archive.writestr("movie.mp4", b"hello")
        remote.data = buffer.getvalue()
    else:
        remote.data = test_zip_preview.make_zip([("movie.mp4", b"hello")])
    listing = browser.get("/file/1/zip")
    assert "播放视频".encode() not in listing.data
    response = browser.get("/file/1/zip/member/0?stream=1")
    assert response.status_code == 400
    assert "Store".encode() in response.data


def test_stream_rejects_old_index_version(browser, movie):
    response = browser.get("/file/1/zip/member/0?stream=1&v=old", headers={"Range": "bytes=0-1"})
    assert response.status_code == 400
    assert "目录已变化".encode() in response.data


def test_head_ignores_range_and_if_range_returns_full_representation(browser, movie):
    response = browser.head("/file/1/zip/member/0?stream=1", headers={"Range": "bytes=0-1"})
    assert response.status_code == 200
    assert response.headers["Content-Length"] == str(len(movie))
    response = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": "bytes=0-1", "If-Range": '"old"'})
    assert response.status_code == 200
    assert response.data == movie


def test_stream_url_expiry_refreshes_cached_link_before_headers(browser, remote, movie, monkeypatch):
    import requests
    from routes.zip_browser import get_client

    browser.get("/file/1/zip/member/0")
    urls = []

    def link(*args, **kwargs):
        urls.append(1)
        return ("https://example.test/refreshed", "api")

    monkeypatch.setattr(get_client(), "get_final_download_url", link)
    original = requests.Session.get
    expired = [False]
    with zipfile.ZipFile(io.BytesIO(remote.data)) as archive:
        info = archive.infolist()[0]
        offset = info.header_offset + 30 + len(info.filename.encode("utf-8"))
    payload_range = f"bytes={offset}-{offset + 19}"

    def get(session, url, **kwargs):
        result = original(session, url, **kwargs)
        if kwargs["headers"]["Range"] == payload_range and not expired[0]:
            result.status_code = 403
            expired[0] = True
        return result

    monkeypatch.setattr(requests.Session, "get", get)
    # Use an exact payload range, separate from local-header reads.
    response = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": "bytes=0-19"})
    assert response.status_code == 206
    assert response.data == movie[:20]
    assert expired[0]
    assert len(urls) == 1


def test_concurrent_streams_have_independent_positions(browser, movie):
    first = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": "bytes=0-599999"}, buffered=False)
    second = browser.get("/file/1/zip/member/0?stream=1", headers={"Range": "bytes=400000-999999"}, buffered=False)
    a, b = iter(first.response), iter(second.response)
    first_data = next(a)
    second_data = next(b)
    assert first_data + b"".join(a) == movie[:600000]
    assert second_data + b"".join(b) == movie[400000:1000000]
    first.close()
    second.close()


def test_changed_archive_mid_stream_closes_reader(browser, remote, movie, monkeypatch):
    from api.zip_preview import ChangedArchive, RangeReader

    closed = []
    original = RangeReader.close

    def close(reader):
        closed.append(reader)
        original(reader)

    monkeypatch.setattr(RangeReader, "close", close)
    response = browser.get("/file/1/zip/member/0?stream=1", buffered=False)
    chunks = iter(response.response)
    assert next(chunks) == movie[:STREAM_CHUNK]
    remote.etag = '"changed"'
    before = len(closed)
    with pytest.raises(ChangedArchive):
        next(chunks)
    assert len(closed) > before
    assert closed[-1].closed
    response.close()
