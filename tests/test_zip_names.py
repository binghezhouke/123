import io
import struct
import zipfile
import zlib

import pytest

import test_zip_preview
from api.zip_preview import open_remote_zip, read_member

remote = test_zip_preview.remote
browser = test_zip_preview.browser


def legacy_zip(name, raw_name, extra=b""):
    class LegacyInfo(zipfile.ZipInfo):
        def _encodeFilenameFlags(self):
            return raw_name, self.flag_bits & ~0x800

    info = LegacyInfo(name)
    info.extra = extra
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w") as archive:
        archive.writestr(info, b"hello")
    return buffer.getvalue()


@pytest.mark.parametrize("encoding", ["gbk", "utf-8"])
def test_legacy_chinese_zip_names_can_be_browsed_and_opened(browser, remote, encoding):
    remote.data = legacy_zip("相册/照片.jpg", "相册/照片.jpg".encode(encoding))
    root = browser.get("/file/1/zip")
    assert "相册".encode() in root.data
    listing = browser.get("/file/1/zip", query_string={"path": "相册/"})
    assert listing.status_code == 200
    assert "照片.jpg".encode() in listing.data
    result = browser.get("/file/1/zip/member/0?download=1")
    assert result.status_code == 200
    assert result.data == b"hello"
    assert "%E7%85%A7%E7%89%87.jpg" in result.headers["Content-Disposition"]


def test_unicode_path_extra_overrides_legacy_name(browser, remote):
    raw = b"legacy/photo.jpg"
    name = "旅行/海边.jpg"
    payload = b"\x01" + struct.pack("<I", zlib.crc32(raw)) + name.encode()
    extra = struct.pack("<HH", 0x7075, len(payload)) + payload
    remote.data = legacy_zip("legacy/photo.jpg", raw, extra)
    assert "旅行".encode() in browser.get("/file/1/zip").data
    assert "海边.jpg".encode() in browser.get("/file/1/zip", query_string={"path": "旅行/"}).data
    # Original local-header names must still match for extraction.
    assert browser.get("/file/1/zip/member/0").data == b"hello"


def test_standard_utf8_names_remain_readable(remote):
    remote.data = test_zip_preview.make_zip([("照片😀.txt", b"ok")])
    with open_remote_zip("https://example.test/a") as (archive, source):
        assert archive.infolist()[0].filename == "照片😀.txt"
        assert read_member(archive, source, archive.infolist()[0]) == b"ok"


@pytest.mark.parametrize("encoding", ["utf-16", "utf-32", "utf-8-sig", "gb18030"])
def test_zip_text_preview_handles_bom(browser, remote, encoding):
    data = "你好，世界".encode(encoding)
    remote.data = test_zip_preview.make_zip([("notes.txt", data)])
    preview = browser.get("/file/1/zip/member/0")
    assert preview.data.decode("utf-8") == "你好，世界"
    assert browser.get("/file/1/zip/member/0?download=1").data == data


@pytest.mark.parametrize("problem", ["crc", "version", "utf8"])
def test_invalid_unicode_extra_does_not_override_original_name(browser, remote, problem):
    raw = b"photo.jpg"
    payload = bytes([2 if problem == "version" else 1])
    payload += struct.pack("<I", 0 if problem == "crc" else zlib.crc32(raw))
    payload += b"\xff" if problem == "utf8" else "错误.jpg".encode()
    extra = struct.pack("<HH", 0x7075, len(payload)) + payload
    remote.data = legacy_zip("photo.jpg", raw, extra)
    assert b"photo.jpg" in browser.get("/file/1/zip").data
    assert browser.get("/file/1/zip/member/0").data == b"hello"


def test_cp437_name_and_raw_metadata_are_preserved(remote):
    from api.archive_names import member_name

    remote.data = legacy_zip("café.txt", "café.txt".encode("cp437"))
    with open_remote_zip("https://example.test/a") as (archive, source):
        entry = archive.infolist()[0]
        assert member_name(entry) == "café.txt"
        assert entry.filename == entry.orig_filename == "café.txt"
        assert read_member(archive, source, entry) == b"hello"


def test_legacy_name_in_encrypted_zip_still_extracts(browser, remote):
    import pyzipper

    raw = "照片.jpg".encode("gbk")
    placeholder = "x" * (len(raw) - 4) + ".jpg"
    data = io.BytesIO()
    with pyzipper.AESZipFile(data, "w", compression=pyzipper.ZIP_DEFLATED, encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(b"fixture-only")
        archive.writestr(placeholder, b"image-data")
    remote.data = data.getvalue().replace(placeholder.encode(), raw)
    assert "照片.jpg".encode() in browser.get("/file/1/zip").data
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    browser.post("/file/1/zip", data={"action": "password", "password": "fixture-only", "csrf_token": csrf})
    assert browser.get("/file/1/zip/member/0").data == b"image-data"
