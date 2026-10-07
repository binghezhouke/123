"""Favorites are local; rendering must work without cloud access and escape filenames."""
import pytest
import json
import re
from pathlib import Path
from threading import Thread
from werkzeug.serving import make_server

import test_file_browser
from test_file_browser import BrowserClient, make_file
import test_zip_preview

app = test_file_browser.app
archive_browser = test_zip_preview.browser
remote = test_zip_preview.remote
make_zip = test_zip_preview.make_zip


def launch_chromium(playwright):
    executable = Path(playwright.chromium.executable_path)
    if not executable.is_file():
        installed = sorted(Path.home().glob(".cache/ms-playwright/chromium-*/chrome-linux64/chrome"))
        if not installed:
            pytest.skip("Playwright Chromium is not installed")
        executable = installed[-1]
    return playwright.chromium.launch(headless=True, executable_path=str(executable))


def test_favorites_page_does_not_fetch_cloud_metadata(app, monkeypatch):
    import routes.main

    def fail():
        raise AssertionError('favorites must not call the cloud API')

    monkeypatch.setattr(routes.main, 'get_client', fail)
    response = app.test_client().get('/favorites?color=red')
    assert response.status_code == 200
    assert b'id="favorite-results"' in response.data
    assert b'js/favorites.js' in response.data


@pytest.mark.parametrize("path", ["/", "/search?q=test"])
def test_favorites_markers_keep_file_identity_and_escaped_name_in_both_views(app, monkeypatch, path):
    import routes.main

    client = BrowserClient()
    client.list_files = lambda **kw: ([make_file(7, '\"<script>test</script>.txt')], None)
    monkeypatch.setattr(routes.main, 'get_client', lambda: client)
    response = app.test_client().get(path)
    html = response.get_data(as_text=True)
    assert html.count('data-favorite-key="file:7"') == 2
    assert html.count('data-favorite-url="/file/7"') == 2
    assert '<script>test</script>' not in html
    assert '&#34;&lt;script&gt;test&lt;/script&gt;.txt' in html
    if path == "/":
        assert 'css/browser_position.css' in html


def test_archive_favorite_checks_version_and_member_path(archive_browser, remote):
    remote.data = make_zip([("folder/first.txt", b"first"), ("folder/second.txt", b"second")])
    listing = archive_browser.get("/file/1/zip?path=folder/")
    assert listing.status_code == 200
    html = listing.get_data(as_text=True)
    match = re.search(r'data-favorite-key="zip:1:([a-f0-9]+):0"[^>]*data-favorite-member-path="folder/first.txt"', html)
    assert match
    version = match.group(1)

    opened = archive_browser.get("/file/1/zip/member/0", query_string={"v": version, "expected_path": "folder/first.txt"})
    assert opened.status_code == 200
    assert opened.data == b"first"

    stale = archive_browser.get("/file/1/zip/member/0", query_string={"v": version, "expected_path": "folder/second.txt"})
    assert stale.status_code == 400
    assert "文件已变化".encode() in stale.data
    assert b'href="/file/1/zip"' in stale.data


@pytest.fixture
def live_server(app):
    position_state = {"root_requests": 0, "cursors": []}

    def page(rows, load_url=None):
        row_html = "".join(
            f'<div class="file-item" data-file-id="{item_id}"><a href="/away">item {item_id}</a></div>'
            for item_id in rows
        )
        button = f'<a data-load-more href="{load_url}">Load more</a>' if load_url else ''
        return f'''<!doctype html><html><body><div data-file-page hidden></div>
            <main class="finder-main" style="height:200px;overflow:auto"><div id="fileContainer">{row_html}<div style="height:1200px"></div></div>
            <div id="fileListView" style="display:none">{row_html}</div><div>{button}</div></main>
            <script defer src="/static/js/browser_position.js"></script>
            <script defer src="/static/js/file_pagination.js"></script></body></html>'''

    def position_start():
        position_state["root_requests"] += 1
        cursor = f'fresh-{position_state["root_requests"]}'
        return page([1], f'/__position_cursor?cursor={cursor}')

    def position_cursor():
        cursor = __import__("flask").request.args.get("cursor", "")
        position_state["cursors"].append(cursor)
        return page([2])

    app.add_url_rule("/__position_fixture", "test_position_start", position_start)
    app.add_url_rule("/__position_cursor", "test_position_cursor", position_cursor)
    app.add_url_rule("/away", "test_position_away", lambda: "away")
    server = make_server("127.0.0.1", 0, app)
    app.extensions["test_position_state"] = position_state
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_favorites_import_export_validation_and_cross_tab_sync(app, live_server, tmp_path):
    pytest.importorskip("playwright.sync_api", reason="browser UI test requires the Playwright environment")
    from playwright.sync_api import expect, sync_playwright

    with sync_playwright() as playwright:
        browser = launch_chromium(playwright)
        context = browser.new_context(accept_downloads=True)
        first = context.new_page()
        first.route("https://**/*", lambda route: route.abort())
        first.goto(f"{live_server}/favorites", wait_until="domcontentloaded")
        first.evaluate("""() => localStorage.setItem('cloudarchive.favorites.v1', JSON.stringify({
            'file:1': {name: 'Existing.txt', url: '/file/1', color: 'blue'}
        }))""")
        first.reload(wait_until="domcontentloaded")
        second = context.new_page()
        second.route("https://**/*", lambda route: route.abort())
        second.goto(f"{live_server}/favorites", wait_until="domcontentloaded")

        backup = {
            "version": 1,
            "favorites": [
                {"identity": "file:1", "name": "Replacement.txt", "url": "/file/1", "color": "red"},
                {"identity": "file:2", "name": "New.txt", "url": "/file/2", "color": "green"},
                {"identity": "__proto__", "name": "Invalid", "url": "/file/3", "color": "red"},
                {"identity": "file:3", "name": "Credential URL", "url": "/file/3?token=secret", "color": "red"},
            ],
        }
        first.locator("#favorite-import-file").set_input_files({
            "name": "backup.json", "mimeType": "application/json", "buffer": json.dumps(backup).encode(),
        })
        expect(first.locator("#favorite-transfer-message")).to_have_text("导入完成：新增 1 个，重复 1 个，无效 2 个。")
        expect(second.locator("#favorite-count")).to_have_text("2 个收藏")
        stored = first.evaluate("JSON.parse(localStorage.getItem('cloudarchive.favorites.v1'))")
        assert set(stored) == {"file:1", "file:2"}
        assert stored["file:1"]["color"] == "blue"

        with first.expect_download() as download_info:
            first.locator("#favorite-export").click()
        download = download_info.value
        export_path = tmp_path / "favorites.json"
        download.save_as(export_path)
        exported = json.loads(export_path.read_text())
        assert exported["version"] == 1
        assert {item["identity"] for item in exported["favorites"]} == {"file:1", "file:2"}
        assert "secret" not in export_path.read_text()

        first.locator("#favorite-import-file").set_input_files({
            "name": "broken.json", "mimeType": "application/json", "buffer": b"{broken",
        })
        expect(first.locator("#favorite-transfer-message")).to_contain_text("不是有效 JSON")
        assert first.evaluate("Object.keys(JSON.parse(localStorage.getItem('cloudarchive.favorites.v1'))).length") == 2

        first.locator("#favorite-import-file").set_input_files({
            "name": "legacy.json", "mimeType": "application/json",
            "buffer": json.dumps({"file:4": {"name": "Legacy.txt", "url": "/file/4", "color": "yellow"}}).encode(),
        })
        expect(first.locator("#favorite-transfer-message")).to_contain_text("新增 1 个")
        assert first.evaluate("JSON.parse(localStorage.getItem('cloudarchive.favorites.v1'))['file:4'].name") == "Legacy.txt"
        context.close()
        browser.close()


def test_recoloring_archive_favorite_keeps_member_path_and_uses_key_archive(app, live_server):
    pytest.importorskip("playwright.sync_api", reason="browser UI test requires the Playwright environment")
    from playwright.sync_api import sync_playwright

    version = "0123456789abcdef01234567"
    record = {"zip:1:" + version + ":0": {
        "name": "inside.txt", "url": "/file/2/zip?path=wrong/", "color": "blue", "memberPath": "folder/inside.txt",
    }}
    with sync_playwright() as playwright:
        browser = launch_chromium(playwright)
        page = browser.new_page()
        page.route("https://**/*", lambda route: route.abort())
        page.goto(f"{live_server}/favorites", wait_until="domcontentloaded")
        page.evaluate("value => localStorage.setItem('cloudarchive.favorites.v1', JSON.stringify(value))", record)
        page.reload(wait_until="domcontentloaded")
        page.locator("#favorite-results .favorite-marker").click()
        page.locator('#favorite-colors [data-color="red"]').click()

        saved = page.evaluate("JSON.parse(localStorage.getItem('cloudarchive.favorites.v1'))")
        assert saved["zip:1:" + version + ":0"]["memberPath"] == "folder/inside.txt"
        target = page.locator("#favorite-results .favorite-file-link").get_attribute("href")
        from urllib.parse import parse_qs, urlparse
        parsed = urlparse(target)
        assert parsed.path == "/file/1/zip/member/0"
        assert parse_qs(parsed.query) == {"v": [version], "expected_path": ["folder/inside.txt"]}
        browser.close()


def test_browser_position_replays_fresh_cursor_and_restores_row_and_scroll(app, live_server):
    pytest.importorskip("playwright.sync_api", reason="browser UI test requires the Playwright environment")
    from playwright.sync_api import sync_playwright

    with sync_playwright() as playwright:
        browser = launch_chromium(playwright)
        page = browser.new_page()
        page.set_default_timeout(5000)
        page.route("https://**/*", lambda route: route.abort())
        base = live_server
        page.goto(f"{base}/__position_fixture", wait_until="domcontentloaded")
        page.locator("[data-load-more]").click()
        page.locator('#fileContainer [data-file-id="2"]').wait_for()
        page.locator(".finder-main").evaluate("element => { element.scrollTop = 333; }")
        page.evaluate("document.querySelector('#fileContainer [data-file-id=\"2\"] a').click()")
        page.wait_for_url(f"{base}/away")

        # Simulate a saved session from the old version, whose cursor URLs are now expired.
        page.evaluate("""() => sessionStorage.setItem('cloudarchive.position:/__position_fixture', JSON.stringify({
            pages: [`${location.origin}/__position_cursor?cursor=expired-token`], selected: '2', top: 333, at: Date.now()
        }))""")
        page.goto(f"{base}/__position_fixture", wait_until="domcontentloaded")
        page.locator('#fileContainer [data-file-id="2"].browser-restored-selection').wait_for()
        page.wait_for_function("document.querySelector('.finder-main').scrollTop === 333")

        assert app.extensions["test_position_state"]["cursors"] == ["fresh-1", "fresh-2"]
        assert page.locator('#fileContainer [data-file-id="2"]').get_attribute("aria-current") == "true"
        selected_rows = page.locator('.file-item[data-file-id="2"].browser-restored-selection')
        assert selected_rows.count() == 2
        assert selected_rows.evaluate_all("rows => rows.every(row => row.getAttribute('aria-current') === 'true')")
        assert page.locator(".finder-main").evaluate("element => element.scrollTop") == 333
        browser.close()
