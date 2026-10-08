"""Batch HTTP endpoints use real encrypted ZIP reads before recording a save."""

import io
from types import SimpleNamespace
from threading import Thread

import pyzipper
import pytest

from api.exceptions import FileUploadError
from api.models import File
from test_file_browser import app as app, make_file
from test_zip_preview import remote as remote, make_zip


BASE = "/directory/8/zip-passwords"
PASSWORD = "共同密码 fixture "


@pytest.fixture
def batch(app, monkeypatch, remote):
    data = io.BytesIO()
    with pyzipper.AESZipFile(data, "w", compression=pyzipper.ZIP_DEFLATED,
                            encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(PASSWORD.encode())
        archive.writestr("image.jpg", b"sample authenticated contents")
    remote.data = data.getvalue()
    archive = File({"fileId": 1, "filename": "one.zip", "type": 0, "parentFileId": 8,
                    "size": len(remote.data), "etag": "v1"})
    saves, calls = [], []
    client = SimpleNamespace(
        get_file_info_single=lambda *a, **k: None,
        get_file_detail=lambda *a: archive,
        get_final_download_url=lambda *a, **kw: ("https://example.test/archive", "api"),
        save_archive_password=lambda *args, **kw: saves.append((args, kw)) or {"fileID": 9},
    )

    def listing(**kwargs):
        calls.append(kwargs)
        if kwargs.get("last_file_id") is None:
            return [archive, make_file(2, "skip.ZIP", 8), make_file(5, "nested", 8, 1)], 99
        return [make_file(3, "skip.ZIP.pwd", 8), make_file(4, "later.zip", 8),
                make_file(6, "plain.txt", 8), make_file(7, "subdir.zip", 8, 1)], -1

    client.list_files = listing
    monkeypatch.setattr("routes.batch_passwords.get_client", lambda: client)
    monkeypatch.setattr("routes.main.get_client", lambda: client)
    monkeypatch.setattr("api.archive_password_batch.sleep", lambda *_: None)
    web = app.test_client()
    assert web.get(BASE).status_code == 200
    with web.session_transaction() as state:
        csrf = state["archive_csrf"]
    return SimpleNamespace(web=web, csrf=csrf, saves=saves, calls=calls, client=client, archive=archive)


def test_directory_link_and_plan_scan_all_pages_without_subdirectories(batch):
    page = batch.web.get("/?parent_id=8")
    assert BASE.encode() in page.data
    batch.calls.clear()
    response = batch.web.post(BASE + "/plan", json={"csrf_token": batch.csrf})
    assert response.status_code == 200
    assert [(v["name"], v["status"]) for v in response.json["items"]] == [
        ("one.zip", "pending"), ("skip.ZIP", "skipped"), ("later.zip", "pending")]
    assert [c["last_file_id"] for c in batch.calls] == [None, 99]
    assert all(c["parent_id"] == 8 and c["use_cache"] is False for c in batch.calls)
    assert not batch.saves
    assert response.headers["Cache-Control"] == "no-store"


def test_password_validated_then_saved_and_available_to_same_browser(batch, monkeypatch):
    response = batch.web.post(BASE + "/file/1", json={"csrf_token": batch.csrf, "password": PASSWORD})
    assert response.json["status"] == "saved"
    assert batch.saves == [((1, PASSWORD), {"archive_kind": ".zip", "skip_existing": True,
                                          "expected_archive": batch.archive})]
    assert PASSWORD.encode() not in response.data
    with batch.web.session_transaction() as state:
        assert PASSWORD not in str(dict(state))
        owner = state["archive_session"]
    key = (1, ".zip", "v1", batch.archive.size, None)
    assert batch.web.application.extensions["archive_passwords"].get(owner, key) == PASSWORD
    monkeypatch.setattr("routes.zip_browser.get_client", lambda: batch.client)
    monkeypatch.setattr(batch.client, "get_file_info_single", lambda *a, **kw: batch.archive)
    opened = batch.web.get("/file/1/zip/member/0")
    assert opened.status_code == 200
    assert opened.data == b"sample authenticated contents"


@pytest.mark.parametrize("password", ["wrong", "", "x" * 1025, None, ["wrong"]])
def test_wrong_or_invalid_password_never_saves(batch, password):
    response = batch.web.post(BASE + "/file/1", json={"csrf_token": batch.csrf, "password": password})
    assert response.status_code == 400 or response.json["status"] == "failed"
    assert not batch.saves


def test_skip_validation_saves_without_resolving_or_reading_zip(batch, monkeypatch, remote):
    # Even an unavailable/corrupt ZIP must not be opened in direct-save mode.
    remote.data = b"not a readable zip"
    monkeypatch.setattr(batch.client, "get_final_download_url",
                        lambda *a, **kw: pytest.fail("must not resolve a download URL"))
    monkeypatch.setattr(batch.web.application.extensions["archive_cache"], "run",
                        lambda *a, **kw: pytest.fail("must not inspect ZIP contents"))
    response = batch.web.post(BASE + "/file/1", json={
        "csrf_token": batch.csrf, "password": PASSWORD, "skip_validation": True})
    assert response.json == {"status": "saved", "validated": False,
                             "message": "已直接保存同级 .pwd（未验证密码）"}
    assert batch.saves == [((1, PASSWORD), {"archive_kind": ".zip", "skip_existing": True,
                                          "expected_archive": batch.archive})]
    assert not remote.ranges
    assert PASSWORD.encode() not in response.data


@pytest.mark.parametrize("value", [None, "false", "true", 0, 1, []])
def test_skip_validation_requires_boolean(batch, remote, value):
    response = batch.web.post(BASE + "/file/1", json={
        "csrf_token": batch.csrf, "password": PASSWORD, "skip_validation": value})
    assert response.status_code == 400
    assert not batch.saves and not remote.ranges


def test_skip_validation_still_rejects_empty_password(batch, remote):
    response = batch.web.post(BASE + "/file/1", json={
        "csrf_token": batch.csrf, "password": "", "skip_validation": True})
    assert response.status_code == 400
    assert not batch.saves and not remote.ranges


@pytest.mark.parametrize("endpoint", ["/plan", "/file/1"])
def test_invalid_csrf_never_reads_or_saves(batch, endpoint):
    response = batch.web.post(BASE + endpoint, json={"password": PASSWORD, "csrf_token": "bad"})
    assert response.status_code == 400
    assert not batch.calls and not batch.saves


def test_unencrypted_zip_is_skipped(batch, remote):
    remote.data = make_zip([("readme.txt", b"no encryption")])
    response = batch.web.post(BASE + "/file/1", json={"password": PASSWORD, "csrf_token": batch.csrf})
    assert response.json["status"] == "skipped"
    assert not batch.saves


@pytest.mark.parametrize("skip_validation", [False, True])
@pytest.mark.parametrize("change", [{"parentFileId": 9}, {"filename": "other.rar"}, {"trashed": 1}, {"type": 1}])
def test_moved_deleted_or_non_zip_target_is_skipped(batch, monkeypatch, remote, change, skip_validation):
    altered = File({**batch.archive.to_dict(), **change})
    monkeypatch.setattr(batch.client, "get_file_detail", lambda *_: altered)
    response = batch.web.post(BASE + "/file/1", json={"password": PASSWORD, "csrf_token": batch.csrf,
                                                    "skip_validation": skip_validation})
    assert response.json["status"] == "skipped"
    assert not batch.saves and not remote.ranges


def test_save_failure_is_sanitized_and_next_request_can_succeed(batch, monkeypatch, caplog):
    original = batch.client.save_archive_password

    def fail(*args, **kwargs):
        raise FileUploadError("do not expose " + PASSWORD)

    monkeypatch.setattr(batch.client, "save_archive_password", fail)
    result = batch.web.post(BASE + "/file/1", json={"password": PASSWORD, "csrf_token": batch.csrf})
    assert result.json["status"] == "failed"
    assert PASSWORD not in result.get_data(as_text=True) + caplog.text
    monkeypatch.setattr(batch.client, "save_archive_password", original)
    assert batch.web.post(BASE + "/file/1", json={"password": PASSWORD, "csrf_token": batch.csrf}).json["status"] == "saved"


@pytest.mark.parametrize("skip_validation", [False, True])
def test_sidecar_appearing_during_validation_is_reported_skipped(batch, monkeypatch, skip_validation):
    monkeypatch.setattr(batch.client, "save_archive_password", lambda *a, **kw: {"skipped": True})
    response = batch.web.post(BASE + "/file/1", json={"password": PASSWORD, "csrf_token": batch.csrf,
                                                    "skip_validation": skip_validation})
    assert response.json["status"] == "skipped"


def test_duplicate_names_and_partial_scan_never_schedule_writes(batch, monkeypatch):
    duplicate = make_file(2, "one.zip", 8)
    monkeypatch.setattr(batch.client, "list_files", lambda **kw: ([batch.archive, duplicate], -1))
    response = batch.web.post(BASE + "/plan", json={"csrf_token": batch.csrf})
    assert all(item["status"] == "skipped" for item in response.json["items"])
    monkeypatch.setattr(batch.client, "list_files", lambda **kw: ([batch.archive], 42))
    assert batch.web.post(BASE + "/plan", json={"csrf_token": batch.csrf}).status_code == 502
    assert not batch.saves


@pytest.mark.parametrize("skip_validation", [False, True])
@pytest.mark.parametrize("stop_early", [False, True])
def test_browser_runs_serially_continues_failures_and_stops_after_current(batch, stop_early, skip_validation):
    from playwright.sync_api import sync_playwright
    from werkzeug.serving import make_server
    from test_favorites import launch_chromium

    server = make_server("127.0.0.1", 0, batch.web.application, threaded=True)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with sync_playwright() as playwright:
            browser = launch_chromium(playwright)
            page = browser.new_page()
            page.route("https://**/*", lambda route: route.abort())
            errors, requests_seen, held = [], [], []
            page.on("pageerror", lambda error: errors.append(str(error)))
            items = [{"name": "<img src=x>.zip", "status": "pending", "url": BASE + "/file/1"},
                     {"name": "existing.zip", "status": "skipped", "message": "已有密码文件"},
                     {"name": "later.zip", "status": "pending", "url": BASE + "/file/2"}]
            page.route("**/zip-passwords/plan", lambda route: route.fulfill(json={"items": items}))

            def apply_route(route):
                requests_seen.append(route.request.post_data_json)
                if len(requests_seen) == 1:
                    held.append(route)
                else:
                    route.fulfill(json={"status": "saved", "message": "已保存"})

            page.route("**/zip-passwords/file/*", apply_route)
            page.goto(f"http://127.0.0.1:{server.server_port}{BASE}", wait_until="domcontentloaded")
            page.locator("#batch-password").fill(PASSWORD)
            assert not page.locator("#batch-skip-validation").is_checked()
            if skip_validation:
                page.locator("#batch-skip-validation").check()
                assert page.locator("#batch-start").inner_text() == "直接生成密码文件"
            page.locator("#batch-start").click()
            pending_message = "正在生成密码文件…" if skip_validation else "正在验证并保存…"
            page.wait_for_function("text => document.querySelector('#batch-results li span:last-child')?.textContent === text",
                                   arg=pending_message)
            # The first request remains pending, so later targets must not start.
            page.wait_for_timeout(100)
            assert len(requests_seen) == 1 and held
            assert page.locator("#batch-password").input_value() == ""
            assert page.locator("#batch-skip-validation").is_disabled()
            if stop_early:
                page.locator("#batch-stop").click()
            held[0].fulfill(json={"status": "failed", "message": "密码错误"})
            page.wait_for_function("!document.querySelector('#batch-start').disabled")
            summary = page.locator("#batch-status").inner_text()
            assert ("已停止" if stop_early else "处理完成") in summary
            assert len(requests_seen) == (1 if stop_early else 2)
            assert all(item["password"] == PASSWORD for item in requests_seen)
            assert all(item["skip_validation"] is skip_validation for item in requests_seen)
            assert page.locator("#batch-skip-validation").is_enabled()
            assert "未处理 2" in summary if stop_early else "成功 1，跳过 1，失败 1" in summary
            assert page.locator("#batch-results img").count() == 0
            assert page.locator("#batch-password").input_value() == ""
            assert PASSWORD not in page.evaluate("JSON.stringify(localStorage) + JSON.stringify(sessionStorage) + document.cookie")
            assert not errors
            browser.close()
    finally:
        server.shutdown()
        thread.join(timeout=3)
