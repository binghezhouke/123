"""Web password submission validates encrypted data before saving a sidecar."""

import io

import pyzipper
import pytest

from api.exceptions import FileUploadError
from test_zip_preview import browser as browser, remote as remote


def prepare(browser, remote, monkeypatch):
    from routes.zip_browser import get_client

    output = io.BytesIO()
    with pyzipper.AESZipFile(output, "w", compression=pyzipper.ZIP_DEFLATED,
                            encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(b"web fixture password")
        archive.writestr("hello.txt", b"validated encrypted contents")
    remote.data = output.getvalue()
    saves = []
    client = get_client()
    monkeypatch.setattr(client, "save_archive_password",
                        lambda file_id, password, **kwargs: saves.append((file_id, password)) or {"fileID": 9},
                        raising=False)
    listing = browser.get("/file/1/zip")
    assert listing.status_code == 200
    assert b'name="save_password" value="1" checked' in listing.data
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    return csrf, saves, client


def test_web_password_validates_then_saves_sibling(browser, remote, monkeypatch):
    csrf, saves, _ = prepare(browser, remote, monkeypatch)
    response = browser.post("/file/1/zip", data={
        "action": "password", "password": "web fixture password",
        "save_password": "1", "csrf_token": csrf,
    }, follow_redirects=True)
    assert response.status_code == 200
    assert saves == [(1, "web fixture password")]
    assert "已保存同级密码文件 test.zip.pwd".encode() in response.data
    assert b"web fixture password" not in response.data
    assert "no-store" in response.headers["Cache-Control"]
    assert browser.get("/file/1/zip/member/0").data == b"validated encrypted contents"


@pytest.mark.parametrize("failure", ["password", "csrf", "authentication"])
def test_web_invalid_submission_never_uploads(browser, remote, monkeypatch, failure):
    csrf, saves, _ = prepare(browser, remote, monkeypatch)
    if failure == "authentication":
        data = bytearray(remote.data)
        central = data.index(b"PK\x01\x02")
        data[central - 1] ^= 1
        remote.data = bytes(data)
        remote.etag = '"changed"'
        browser.application.extensions["archive_cache"].entries.clear()
    response = browser.post("/file/1/zip", data={
        "action": "password",
        "password": "wrong" if failure == "password" else "web fixture password",
        "csrf_token": "wrong" if failure == "csrf" else csrf,
        "save_password": "1",
    })
    assert response.status_code in (400, 401)
    assert saves == []


def test_existing_web_password_can_be_saved_separately(browser, remote, monkeypatch):
    csrf, saves, _ = prepare(browser, remote, monkeypatch)
    assert browser.post("/file/1/zip", data={
        "action": "password", "password": "web fixture password", "csrf_token": csrf,
    }).status_code == 302
    assert saves == []
    response = browser.post("/file/1/zip", data={"action": "save_password", "csrf_token": csrf})
    assert response.status_code == 302
    assert saves == [(1, "web fixture password")]
    other_browser = browser.application.test_client()
    other_browser.get("/file/1/zip")
    with other_browser.session_transaction() as state:
        other_csrf = state["archive_csrf"]
    assert other_browser.post("/file/1/zip", data={
        "action": "save_password", "csrf_token": other_csrf,
    }).status_code == 400
    assert len(saves) == 1


def test_upload_failure_keeps_web_password_and_shows_retry(browser, remote, monkeypatch):
    csrf, _, client = prepare(browser, remote, monkeypatch)

    def fail(*_, **kwargs):
        raise FileUploadError("upstream failed")

    monkeypatch.setattr(client, "save_archive_password", fail)
    response = browser.post("/file/1/zip", data={
        "action": "password", "password": "web fixture password",
        "save_password": "1", "csrf_token": csrf,
    }, follow_redirects=True)
    assert response.status_code == 200
    assert "保存到网盘失败".encode() in response.data
    assert "保存密码到网盘".encode() in response.data
    assert browser.get("/file/1/zip/member/0").data == b"validated encrypted contents"


@pytest.mark.parametrize("solid,header", [(False, False), (False, True), (True, True)])
@pytest.mark.parametrize("correct", [True, False])
def test_web_7z_password_verified_before_save(browser, remote, monkeypatch, solid, header, correct):
    import py7zr
    from api.models import File
    from routes.zip_browser import get_client

    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w", password="fixture-only", header_encryption=header) as archive:
        archive.writestr(b"hello encrypted contents", "a.txt")
        if solid:
            archive.writestr(b"second encrypted contents", "b.txt")
    remote.data = data.getvalue()
    client = get_client()
    monkeypatch.setattr(client, "get_file_info_single", lambda *a, **kw: File(
        {"fileId": 1, "filename": "test.7z", "type": 0, "size": len(remote.data)}))
    saves = []
    monkeypatch.setattr(client, "save_archive_password", lambda *a, **kw: saves.append((a, kw)), raising=False)
    listing = browser.get("/file/1/zip")
    assert listing.status_code in (200, 401)
    assert b"test.7z.pwd" in listing.data
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    response = browser.post("/file/1/zip", data={
        "action": "password", "password": "fixture-only" if correct else "wrong",
        "save_password": "1", "csrf_token": csrf,
    })
    assert response.status_code == (302 if correct else 401)
    assert len(saves) == int(correct)
    if correct:
        assert saves[0][1] == {"archive_kind": ".7z"}


@pytest.mark.parametrize("name", ["rar5-psw.rar", "rar5-hpsw.rar"])
@pytest.mark.parametrize("correct", [True, False])
def test_web_rar_password_verified_before_save(browser, remote, monkeypatch, name, correct):
    import shutil
    from pathlib import Path
    from api.models import File
    from routes.zip_browser import get_client

    if not shutil.which("unrar"):
        pytest.skip("requires unrar")
    remote.data = (Path(__file__).parent / "fixtures/rar" / name).read_bytes()
    client = get_client()
    monkeypatch.setattr(client, "get_file_info_single", lambda *a, **kw: File(
        {"fileId": 1, "filename": name, "type": 0, "size": len(remote.data)}))
    saves = []
    monkeypatch.setattr(client, "save_archive_password", lambda *a, **kw: saves.append(a), raising=False)
    browser.get("/file/1/zip")
    with browser.session_transaction() as state:
        csrf = state["archive_csrf"]
    response = browser.post("/file/1/zip", data={
        "action": "password", "password": "password" if correct else "wrong",
        "save_password": "1", "csrf_token": csrf,
    })
    assert response.status_code == (302 if correct else 401)
    assert len(saves) == int(correct)
