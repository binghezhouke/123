"""Exercise status UI with actual Go routes/assets and deterministic samples."""
import copy
import datetime
import json
import os
from pathlib import Path
import sys

from playwright.sync_api import sync_playwright

base = sys.argv[1]
with sync_playwright() as pw:
    executable = Path(pw.chromium.executable_path)
    if not executable.is_file():
        executable = sorted(Path.home().glob('.cache/ms-playwright/chromium-*/chrome-linux64/chrome'))[-1]
    browser = pw.chromium.launch(headless=True, executable_path=str(executable))
    context = browser.new_context(viewport={'width': 1360, 'height': 1000})
    page = context.new_page()
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    # Begin with a real shared-service snapshot and serve only aggregate data.
    response = context.request.get(base + '/api/status')
    assert response.status == 200
    original = response.json()
    control = {'calls': 0, 'fail': False, 'restart': False}

    def respond(route):
        control['calls'] += 1
        if control['fail']:
            route.fulfill(status=503, content_type='application/json', body='{"error":"unavailable"}')
            return
        data = copy.deepcopy(original)
        n = control['calls']
        started = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)
        if control['restart']:
            started += datetime.timedelta(days=1)
            n = 1
        data['io']['started_at'] = started.isoformat()
        data['io']['collected_at'] = (started + datetime.timedelta(seconds=n * 5)).isoformat()
        data['io']['uptime_seconds'] = n * 5
        data['io']['downloaded_bytes'] = {'status': 'measured', 'events': n, 'bytes': n * 5242880}
        data['web'].update(response_bytes=n * 10485760, completed=n, active=1)
        data['io']['cache']['foreground'].update(status='measured', hit_bytes=n * 75, requested_bytes=n * 100)
        route.fulfill(status=200, content_type='application/json', body=json.dumps(data))

    page.route('**/api/status', respond)
    page.goto(base + '/status', wait_until='networkidle')
    page.wait_for_function("() => document.querySelector('#connection').textContent === '实时更新中'")
    assert page.locator('#api-table tr').count() == 5
    assert page.locator('#latency-table tr').count() == 15
    assert page.locator('.chart svg').count() == 3
    page.locator('#refresh').click()
    page.wait_for_function("() => document.querySelector('#download-rate').textContent === '1 MiB/s'")
    assert page.locator('#web-rate').inner_text() == '2 MiB/s'
    assert page.locator('#cache-hit').inner_text() == '75.0%'
    page.locator('#chart-traffic').focus()
    page.keyboard.press('ArrowLeft')
    assert '网盘下载' in page.locator('#chart-traffic .chart-readout').inner_text()

    page.locator('#pause').click()
    assert page.locator('#pause').get_attribute('aria-pressed') == 'true'
    count = control['calls']
    page.wait_for_timeout(5300)
    assert control['calls'] == count, 'paused page kept polling'
    page.locator('#pause').click()
    page.wait_for_function("() => document.querySelector('#download-rate').textContent === '—'")
    control['fail'] = True
    page.locator('#refresh').click()
    page.wait_for_function("() => !document.querySelector('#status-error').hidden")
    assert '503' in page.locator('#status-error').inner_text()
    assert page.locator('#download-rate').inner_text() == '—'
    control['fail'] = False
    page.locator('#refresh').click()
    page.wait_for_function("() => document.querySelector('#status-error').hidden")
    assert page.locator('#download-rate').inner_text() == '—'
    page.locator('#refresh').click()
    page.wait_for_function("() => document.querySelector('#download-rate').textContent === '1 MiB/s'")
    control['restart'] = True
    page.locator('#refresh').click()
    page.wait_for_function("() => document.querySelector('#download-rate').textContent === '—'")
    with page.expect_download() as downloaded:
        page.locator('#export').click()
    exported = json.loads(Path(downloaded.value.path()).read_text())
    assert len(exported['history']) == 1
    assert exported['snapshot']['schema_version'] == 1

    # Visibility lifecycle without relying on headless window-manager focus.
    page.evaluate("Object.defineProperty(document, 'hidden', {configurable: true, value: true}); document.dispatchEvent(new Event('visibilitychange'))")
    count = control['calls']
    page.wait_for_timeout(5300)
    assert control['calls'] == count, 'hidden page kept polling'
    assert '后台' in page.locator('#connection').inner_text()
    page.evaluate("Object.defineProperty(document, 'hidden', {configurable: true, value: false}); document.dispatchEvent(new Event('visibilitychange'))")

    page.set_viewport_size({'width': 390, 'height': 844})
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile page overflows'
    page.emulate_media(color_scheme='dark', reduced_motion='reduce')
    assert page.locator('#pause').is_visible()
    screenshot = os.environ.get('MOUNT123_STATUS_SCREENSHOT')
    if screenshot:
        page.set_viewport_size({'width': 1360, 'height': 1000})
        page.emulate_media(color_scheme='light')
        page.screenshot(path=screenshot, full_page=True)
    assert not errors, errors
    browser.close()
    print('Chromium: charts, rates, pause, hidden tab, errors, restart, export, mobile and dark mode passed')
