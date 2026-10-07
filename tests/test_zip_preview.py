import io
import json
import re
import struct
import zipfile
from types import SimpleNamespace

import pytest

from api.models import File
from api.zip_preview import FILE_LIMIT, ZipPreviewError, open_remote_zip, read_member


@pytest.fixture
def remote(monkeypatch):
    state = SimpleNamespace(data=b"", ranges=[], status=206, etag='"v1"', content_range=None)

    class Response:
        def __init__(self, start, end):
            self.status_code = state.status
            self.headers = {
                "Content-Range": state.content_range or f"bytes {start}-{end}/{len(state.data)}",
                "ETag": state.etag,
            }
            self.data = state.data[start : end + 1]

        def __enter__(self):
            return self

        def __exit__(self, *args):
            pass

        def iter_content(self, size):
            for offset in range(0, len(self.data), size):
                yield self.data[offset : offset + size]

    def get(self, url, headers, **kwargs):
        start, end = map(int, re.findall(r"\d+", headers["Range"]))
        state.ranges.append((start, end))
        return Response(start, end)

    monkeypatch.setattr("requests.Session.get", get)
    return state


def make_zip(entries, compression=zipfile.ZIP_DEFLATED):
    data = io.BytesIO()
    with zipfile.ZipFile(data, "w", compression=compression) as archive:
        for name, content in entries:
            archive.writestr(name, content)
    return data.getvalue()


def test_reads_selected_file_without_fetching_large_sibling(remote):
    remote.data = make_zip([("large.bin", b"x" * 2_000_000), ("目录/hello.txt", b"hello")], zipfile.ZIP_STORED)
    with open_remote_zip("https://example.test/archive.zip") as (archive, source):
        assert archive.namelist() == ["large.bin", "目录/hello.txt"]
        assert read_member(archive, source, archive.infolist()[1]) == b"hello"
    assert sum(end - start + 1 for start, end in remote.ranges) < 150_000


def test_deflate_and_duplicate_names_use_entry_identity(remote):
    with pytest.warns(UserWarning):
        remote.data = make_zip([("same.txt", b"first"), ("same.txt", b"second")])
    with open_remote_zip("https://example.test/a") as (archive, source):
        assert read_member(archive, source, archive.infolist()[0]) == b"first"
        assert read_member(archive, source, archive.infolist()[1]) == b"second"


@pytest.mark.parametrize("status,content_range", [(200, None), (403, None), (206, "bytes 2-3/10")])
def test_rejects_unsupported_or_invalid_ranges(remote, status, content_range):
    remote.data = make_zip([])
    remote.status, remote.content_range = status, content_range
    with pytest.raises(ZipPreviewError):
        with open_remote_zip("https://example.test/a"):
            pass


def test_rejects_changed_archive(remote):
    remote.data = make_zip([("a", b"abc")])
    with open_remote_zip("https://example.test/a") as (archive, source):
        remote.etag = '"v2"'
        with pytest.raises(ZipPreviewError, match="已变化"):
            read_member(archive, source, archive.infolist()[0])


@pytest.mark.parametrize(
    "attribute,value,message",
    [("flag_bits", 1, "加密"), ("file_size", FILE_LIMIT + 1, "32 MiB"), ("compress_type", 99, "压缩方式")],
)
def test_member_limits(remote, attribute, value, message):
    remote.data = make_zip([("a", b"abc")])
    with open_remote_zip("https://example.test/a") as (archive, source):
        entry = archive.infolist()[0]
        setattr(entry, attribute, value)
        with pytest.raises(ZipPreviewError, match=message):
            read_member(archive, source, entry)


def test_split_archive_rejected(remote):
    data = bytearray(make_zip([]))
    struct.pack_into("<H", data, 4, 1)
    remote.data = bytes(data)
    with pytest.raises(ZipPreviewError, match="分卷"):
        with open_remote_zip("https://example.test/a"):
            pass


@pytest.fixture
def browser(tmp_path, monkeypatch, remote):
    from app import create_app

    config = tmp_path / "config.json"
    config.write_text(json.dumps({"CLIENT_ID": "id", "CLIENT_SECRET": "secret", "SECRET_KEY": "test"}))
    client = SimpleNamespace(
        get_file_info_single=lambda *a, **kw: File({"fileId": 1, "filename": "test.zip", "type": 0, "size": 200}),
        get_final_download_url=lambda *a, **kw: ("https://example.test/archive.zip", "api"),
    )
    monkeypatch.setattr("routes.zip_browser.get_client", lambda: client)
    app = create_app(str(config))
    app.config["TESTING"] = True
    return app.test_client()


def test_browse_folders_and_safe_html_preview(browser, remote):
    remote.data = make_zip([("目录/<script>.html", b"<script>alert(1)</script>"), ("empty/", b"")])
    root = browser.get("/file/1/zip")
    assert root.status_code == 200
    assert "目录/".encode() in root.data
    folder = browser.get("/file/1/zip", query_string={"path": "目录/"})
    assert b"&lt;script&gt;.html" in folder.data
    preview = browser.get("/file/1/zip/member/0")
    assert preview.status_code == 200
    assert preview.mimetype == "text/plain"
    assert "sandbox" in preview.headers["Content-Security-Policy"]
    assert preview.headers["X-Content-Type-Options"] == "nosniff"
    download = browser.get("/file/1/zip/member/0?download=1")
    assert download.headers["Content-Disposition"].startswith("attachment;")
    assert browser.get("/file/1/zip/member/9").status_code == 404
    assert browser.get("/file/1/zip?path=missing").status_code == 404
    assert browser.get("/file/1/zip?path=empty/").status_code == 200
    assert browser.post("/file/1/zip").status_code == 400


def test_no_range_shows_actionable_error(browser, remote):
    remote.data = make_zip([])
    remote.status = 200
    response = browser.get("/file/1/zip")
    assert response.status_code == 400
    assert "不支持分段读取".encode() in response.data


def test_zip64_directory_and_member(remote, monkeypatch):
    monkeypatch.setattr(zipfile, "ZIP64_LIMIT", 1)
    remote.data = make_zip([("hello.txt", b"hello zip64")])
    assert b"PK\x06\x06" in remote.data
    with open_remote_zip("https://example.test/a") as (archive, source):
        assert read_member(archive, source, archive.infolist()[0]) == b"hello zip64"


def test_corrupt_member_crc_is_rejected(remote):
    data = bytearray(make_zip([("a.txt", b"hello")], zipfile.ZIP_STORED))
    offset = data.index(b"hello")
    data[offset] = ord("x")
    remote.data = bytes(data)
    with open_remote_zip("https://example.test/a") as (archive, source):
        with pytest.raises(zipfile.BadZipFile, match="CRC"):
            read_member(archive, source, archive.infolist()[0])


def test_zip_uses_api_without_webdav(browser, remote, monkeypatch):
    remote.data = make_zip([])
    from routes.zip_browser import get_client

    client = get_client()

    def final_url(file_id, prefer_webdav=True):
        assert prefer_webdav is False
        return ("https://example.test/a", "api")

    monkeypatch.setattr(client, "get_final_download_url", final_url)
    assert browser.get("/file/1/zip").status_code == 200


def test_large_member_uses_bounded_request_count(remote):
    remote.data = make_zip([("photo.jpg", b"x" * 4_000_000)], zipfile.ZIP_STORED)
    with open_remote_zip("https://example.test/a") as (archive, source):
        before = len(remote.ranges)
        assert len(read_member(archive, source, archive.infolist()[0])) == 4_000_000
        assert len(remote.ranges) - before <= 4


def test_gallery_only_includes_supported_available_images(browser, remote):
    remote.data = make_zip([("photo.jpg", b"photo"), ("image.png", b"image"), ("note.txt", b"text")])
    response = browser.get("/file/1/zip")
    assert response.status_code == 200
    assert response.data.count(b'class="btn btn-sm btn-primary zip-image"') == 2
    assert b'<dialog id="zip-gallery"' in response.data
    assert b"zip_gallery.js" in response.data
    assert b'data-url="/file/1/zip/member/0?v=' in response.data


def test_old_index_version_cannot_open_a_different_member(browser, remote):
    remote.data = make_zip([("a.jpg", b"first")])
    response = browser.get("/file/1/zip")
    version = re.search(rb'data-url="/file/1/zip/member/0\?v=([a-f0-9]+)', response.data)[1].decode()
    remote.data = make_zip([("b.jpg", b"second")])
    remote.etag = '"v2"'
    assert browser.get("/file/1/zip?refresh=1").status_code == 200
    response = browser.get("/file/1/zip/member/0", query_string={"v": version})
    assert response.status_code == 400
    assert "目录已变化".encode() in response.data


def test_7z_encrypted_header_has_friendly_route_response(browser, remote, monkeypatch):
    import py7zr
    from routes.zip_browser import get_client

    client = get_client()
    monkeypatch.setattr(
        client,
        "get_file_info_single",
        lambda *a, **kw: File({"fileId": 1, "filename": "encrypted.7z", "type": 0, "size": 200}),
    )
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w", password="fixture-only", header_encryption=True) as archive:
        archive.writestr(b"test", "a.txt")
    remote.data = data.getvalue()
    response = browser.get("/file/1/zip")
    assert response.status_code == 401
    assert "加密".encode() in response.data
    assert b'name="csrf_token"' in response.data


def test_archive_password_posts_require_session_csrf(browser, remote):
    remote.data = make_zip([])
    response = browser.get("/file/1/zip")
    assert response.status_code == 200
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    assert (
        browser.post(
            "/file/1/zip",
            data={
                "action": "password",
                "password": "secret",
                "csrf_token": "wrong",
            },
        ).status_code
        == 400
    )
    malformed = browser.post(
        "/file/1/zip",
        data={
            "action": "password",
            "password": "secret",
            "csrf_token": "密碼",
        },
    )
    assert malformed.status_code == 400
    assert malformed.headers["Cache-Control"] == "no-store"
    accepted = browser.post(
        "/file/1/zip",
        data={
            "action": "password",
            "password": "secret",
            "csrf_token": csrf,
            "path": "folder/",
        },
    )
    assert accepted.status_code == 302
    assert accepted.headers["Location"].endswith("/file/1/zip?path=folder/")
    with browser.session_transaction() as state:
        token = state["archive_session"]
    vault = browser.application.extensions["archive_passwords"]
    identity = (1, ".zip", "", 200, None)
    assert vault.get(token, identity) == "secret"
    cleared = browser.post(
        "/file/1/zip",
        data={
            "action": "clear",
            "csrf_token": csrf,
            "path": "folder/",
        },
    )
    assert cleared.status_code == 302
    assert vault.get(token, identity) is None


def test_encrypted_zip_password_is_session_scoped(browser, remote):
    import pyzipper

    data = io.BytesIO()
    with pyzipper.AESZipFile(data, "w", compression=pyzipper.ZIP_DEFLATED, encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(b"correct horse")
        archive.writestr("secret.txt", b"private contents")
    remote.data = data.getvalue()

    listing = browser.get("/file/1/zip")
    assert listing.status_code == 200
    assert listing.data.count(b'name="password"') == 1
    assert b'href="#archive-password-entry"' in listing.data
    csrf = re.search(rb'name="csrf_token" value="([^"]+)"', listing.data)[1].decode()
    cookie_header = listing.headers.get("Set-Cookie", "")
    assert "correct horse" not in cookie_header

    posted = browser.post(
        "/file/1/zip",
        data={
            "action": "password",
            "password": "correct horse",
            "csrf_token": csrf,
            "path": "",
        },
    )
    assert posted.status_code == 302
    unlocked = browser.get("/file/1/zip")
    assert unlocked.status_code == 200
    assert unlocked.data.count(b'name="password"') == 1
    assert "修改密码".encode() in unlocked.data
    member = browser.get("/file/1/zip/member/0")
    assert member.status_code == 200
    assert member.data == b"private contents"

    other_browser = browser.application.test_client()
    isolated = other_browser.get("/file/1/zip/member/0")
    assert isolated.status_code == 401


def test_split_7z_routes_discover_parts_and_extract(browser, remote, monkeypatch):
    import py7zr
    from routes.zip_browser import get_client

    client = get_client()
    # One small archive split across two cloud files; replace transport to map URLs.
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"hello split", "a.txt")
    raw = data.getvalue()
    payloads = [raw[:40], raw[40:]]
    files = [
        File({"fileId": i + 1, "filename": f"photos.7z.{i + 1:03}", "size": len(payload), "type": 0, "parentFileId": 0})
        for i, payload in enumerate(payloads)
    ]
    monkeypatch.setattr(client, "get_file_info_single", lambda file_id, **kw: files[file_id - 1])
    client.list_files = lambda **kw: (files, -1)
    client.get_final_download_url = lambda file_id, **kw: (f"https://part/{file_id}", "api")
    original_get = __import__("requests").Session.get

    def get(self, url, **kwargs):
        remote.data = payloads[int(url.rsplit("/", 1)[1]) - 1]
        return original_get(self, url, **kwargs)

    monkeypatch.setattr("requests.Session.get", get)
    assert browser.get("/file/1/zip").status_code == 200
    response = browser.get("/file/1/zip/member/0")
    assert response.status_code == 200
    assert response.data == b"hello split"
