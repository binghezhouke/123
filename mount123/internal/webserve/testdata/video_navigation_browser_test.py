"""Open a real nested MP4 without consuming it through the HTML navigator."""
from pathlib import Path
from urllib.parse import urlparse
import sys
from playwright.sync_api import sync_playwright

base = sys.argv[1]
with sync_playwright() as pw:
    executable = Path(pw.chromium.executable_path)
    if not executable.is_file():
        executable = sorted(Path.home().glob('.cache/ms-playwright/chromium-*/chrome-linux64/chrome'))[-1]
    browser = pw.chromium.launch(headless=True, executable_path=str(executable))
    page = browser.new_page()
    page.route('**/*', lambda r: r.continue_() if urlparse(r.request.url).hostname == '127.0.0.1' else r.abort())
    requests, ranges = [], []
    page.on('request', lambda r: requests.append(r.resource_type) if '/member?' in r.url else None)
    page.on('response', lambda r: ranges.append(r.status) if '/member?' in r.url else None)
    page.goto(base + '/archive/1?path=inner.zip', wait_until='domcontentloaded')
    page.locator('#fileContainer .file-item').first.click()
    page.wait_for_function("() => document.querySelector('#preview-stage video')?.videoWidth === 64")
    page.evaluate("document.querySelector('#preview-stage video').muted = true")
    page.evaluate("document.querySelector('#preview-stage video').play()")
    page.wait_for_function("() => document.querySelector('#preview-stage video').currentTime > 0")
    page.locator('#preview-open').click()
    page.wait_for_url('**/member?*')
    page.wait_for_function("() => document.querySelector('video')?.videoWidth === 64")
    assert 'fetch' not in requests, f'video incorrectly consumed as HTML: {requests}'
    assert 'document' in requests, f'Open did not navigate natively: {requests}'
    assert 206 in ranges, f'no partial media response: {ranges}'
    print('PASS: nested deflated ZIP MP4 previews, plays, and opens natively with Range support')
    browser.close()
