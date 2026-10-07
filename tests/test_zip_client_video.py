import io
import re
import struct
import zipfile
import zlib

import pyzipper
import pytest

import test_zip_preview
from api.zip_media import CLIENT_VIDEO_LIMIT, can_inflate_in_browser

remote = test_zip_preview.remote
browser = test_zip_preview.browser


def version(browser):
    response = browser.get("/file/1/zip")
    return re.search(rb"v=([0-9a-f]{24})", response.data)[1].decode()


def metadata(browser):
    return browser.get("/file/1/zip/member/0", query_string={"client_video": "1", "v": version(browser)})


def test_raw_deflate_endpoint_does_not_extract_on_server(browser, remote, monkeypatch):
    data = b"movie data" * 1000
    remote.data = test_zip_preview.make_zip([("movie.mp4", data)])
    monkeypatch.setattr("routes.zip_browser.read_archive_member", lambda *args: pytest.fail("server extraction called"))
    info = metadata(browser)
    assert info.status_code == 200
    payload = info.json
    assert payload["size"] == len(data)
    assert payload["crc32"] == zlib.crc32(data)
    assert payload["key"].startswith("1:") and payload["key"].endswith(":0")
    assert payload["raw_url"].startswith("/file/1/zip/member/0?")
    assert "https://" not in info.text
    raw = browser.get(payload["raw_url"], headers={"Range": "bytes=0-1"})
    assert raw.status_code == 200
    assert raw.headers["Content-Type"].startswith("application/octet-stream")
    assert "Content-Encoding" not in raw.headers
    assert raw.headers["Accept-Ranges"] == "none"
    assert raw.headers["Content-Length"] == str(payload["compressed_size"])
    assert zlib.decompress(raw.data, -15) == data
    assert browser.head(payload["raw_url"]).data == b""


def test_raw_payload_excludes_data_descriptor_and_following_member(browser, remote):
    class Unseekable(io.BytesIO):
        def seekable(self):
            return False

        def seek(self, *args):
            raise OSError("unseekable")

    buffer = Unseekable()
    data = b"example movie bytes" * 30
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("movie.mp4", data)
        archive.writestr("private.txt", b"not part of the movie")
    remote.data = buffer.getvalue()
    assert b"PK\x07\x08" in remote.data
    info = metadata(browser).json
    raw = browser.get(info["raw_url"]).data
    inflater = zlib.decompressobj(-15)
    assert inflater.decompress(raw) + inflater.flush() == data
    assert inflater.eof
    assert inflater.unused_data == b""


def test_raw_requires_current_index_version(browser, remote):
    remote.data = test_zip_preview.make_zip([("movie.mp4", b"video")])
    info = metadata(browser).json
    remote.etag = '"changed"'
    assert browser.get(info["raw_url"]).status_code == 400
    assert browser.get("/file/1/zip/member/0?raw_deflate=1").status_code == 400
    assert browser.get("/file/1/zip/member/0?raw_deflate=1&v=old").status_code == 400


def test_encrypted_deflate_is_rejected_even_with_password(browser, remote):
    data = io.BytesIO()
    with pyzipper.AESZipFile(data, "w", compression=zipfile.ZIP_DEFLATED, encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(b"fixture")
        archive.writestr("movie.mp4", b"video")
    remote.data = data.getvalue()
    browser.get("/file/1/zip")
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    browser.post("/file/1/zip", data={"action": "password", "password": "fixture", "csrf_token": csrf})
    assert metadata(browser).status_code == 400


def test_large_deflated_movie_between_32_and_64_mib_is_available(browser, remote, monkeypatch):
    data = b"a" * (33 * 1024 * 1024)
    remote.data = test_zip_preview.make_zip([("movie.mp4", data)])
    monkeypatch.setattr("routes.zip_browser.read_archive_member", lambda *a: pytest.fail("server extraction called"))
    listing = browser.get("/file/1/zip")
    assert b'class="btn btn-sm btn-primary zip-client-video"' in listing.data
    info = metadata(browser)
    assert info.status_code == 200
    assert info.json["size"] == len(data)
    assert info.json["size"] < CLIENT_VIDEO_LIMIT


def test_limits_apply_to_both_compressed_and_uncompressed_size():
    entry = zipfile.ZipInfo("video.mp4")
    entry.compress_type = zipfile.ZIP_DEFLATED
    entry.compress_size = 100
    entry.file_size = 100
    assert can_inflate_in_browser(entry, ".zip", ".mp4")
    for attribute in ("compress_size", "file_size"):
        setattr(entry, attribute, CLIENT_VIDEO_LIMIT + 1)
        assert not can_inflate_in_browser(entry, ".zip", ".mp4")
        setattr(entry, attribute, 100)
    assert not can_inflate_in_browser(entry, ".zip", ".webm")
    assert not can_inflate_in_browser(entry, ".7z", ".mp4")


def test_oversized_central_directory_entry_rejected_before_payload_read(browser, remote):
    data = bytearray(test_zip_preview.make_zip([("movie.mp4", b"video"), ("other.txt", b"ok")]))
    index = data.index(b"PK\x01\x02")
    struct.pack_into("<I", data, index + 24, CLIENT_VIDEO_LIMIT + 1)
    remote.data = bytes(data)
    assert metadata(browser).status_code == 400
