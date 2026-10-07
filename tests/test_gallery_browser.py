"""The ordinary directory viewer uses the same controls and stable progress."""
import base64
from threading import Thread

import pytest
from werkzeug.serving import make_server

import test_file_browser
from test_favorites import launch_chromium

app = test_file_browser.app


@pytest.mark.parametrize("mobile", [False, True])
def test_directory_gallery_reopens_and_resumes_after_reload(app, monkeypatch, mobile):
    sync_api = pytest.importorskip('playwright.sync_api')
    files = {1: test_file_browser.make_file(1, '2.png'), 2: test_file_browser.make_file(2, '10.png')}
    client = test_file_browser.BrowserClient(files)
    client.list_files = lambda **kwargs: (list(files.values()), -1)
    monkeypatch.setattr('routes.main.get_client', lambda: client)
    monkeypatch.setattr('routes.preview.get_client', lambda: client)
    png = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aD1sAAAAASUVORK5CYII=')
    monkeypatch.setattr('routes.preview.requests.get', lambda *args, **kwargs:
                        test_file_browser.FakeUpstream(png, headers={'Content-Length': str(len(png))}))
    server = make_server('127.0.0.1', 0, app, threaded=True)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with sync_api.sync_playwright() as playwright:
            browser = launch_chromium(playwright)
            context = browser.new_context(viewport={"width": 390 if mobile else 1100, "height": 844},
                                          is_mobile=mobile, has_touch=mobile)
            page = context.new_page()
            page.route('https://**/*', lambda route: route.abort())
            errors = []
            page.on('pageerror', lambda error: errors.append(str(error)))
            page.goto(f'http://127.0.0.1:{server.server_port}/?sort=name', wait_until='domcontentloaded')
            page.locator('#fileContainer [data-file-id="1"] a').click()
            page.wait_for_function('document.querySelector("#zip-gallery-stage img")?.naturalWidth > 0')
            assert page.locator('#zip-gallery-counter').inner_text() == '1 / 2'
            if mobile:
                cdp = context.new_cdp_session(page)
                box = page.locator('#zip-gallery-stage').bounding_box()
                y = box['y'] + box['height'] / 2
                def touch(kind, xs):
                    cdp.send('Input.dispatchTouchEvent', {'type': kind, 'touchPoints':
                             [{'x': x, 'y': y, 'id': i} for i, x in enumerate(xs)]})
                touch('touchStart', [150, 230])
                touch('touchMove', [120, 260])
                touch('touchMove', [80, 300])
                touch('touchEnd', [])
                page.wait_for_function('parseInt(document.querySelector("#zip-gallery-zoom-level").textContent)>100')
                touch('touchStart', [280])
                touch('touchMove', [190])
                touch('touchMove', [90])
                touch('touchEnd', [])
                assert page.locator('#zip-gallery-counter').inner_text() == '1 / 2'
                # The CDP-injected multitouch sequence suppresses Chromium's synthesized click.
                page.locator('#zip-gallery-zoom-reset').click()
                page.wait_for_function('document.querySelector("#zip-gallery-zoom-level").textContent==="100%"')

            page.locator('#zip-gallery-close').click()
            page.locator('#fileContainer [data-file-id="2"] a').click()
            page.wait_for_function('document.querySelector("#zip-gallery-stage img")?.naturalWidth > 0')
            assert page.locator('#zip-gallery-counter').inner_text() == '2 / 2'
            page.locator('#zip-gallery-jump-input').fill('1')
            page.locator('#zip-gallery-jump-input').press('ArrowLeft')
            assert page.locator('#zip-gallery-counter').inner_text() == '2 / 2'
            page.locator('#zip-gallery-close').click()
            page.reload(wait_until='domcontentloaded')
            page.locator('[data-gallery-resume]').click()
            page.wait_for_function('document.querySelector("#zip-gallery-stage img")?.naturalWidth > 0')
            assert page.locator('#zip-gallery-counter').inner_text() == '2 / 2'
            assert not errors
            browser.close()
    finally:
        server.shutdown()
        thread.join(timeout=3)
