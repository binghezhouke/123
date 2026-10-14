"""Selected thumbnails and gallery neighbors overtake stalled speculative reads."""
from pathlib import Path
from urllib.parse import urlparse
import sys
from playwright.sync_api import sync_playwright

with sync_playwright() as pw:
    executable = Path(pw.chromium.executable_path)
    if not executable.is_file():
        executable = sorted(Path.home().glob('.cache/ms-playwright/chromium-*/chrome-linux64/chrome'))[-1]
    browser = pw.chromium.launch(headless=True, executable_path=str(executable))
    page = browser.new_page(viewport={'width': 1280, 'height': 900})
    page.set_default_timeout(8000)
    page.route('**/*', lambda route: route.continue_() if urlparse(route.request.url).hostname == '127.0.0.1' else route.abort())
    foreground = []
    def record(request):
        if ('/member?' in request.url or '/preview' in request.url) and 'direct=1' not in request.url and request.headers.get('x-priority') != 'background':
            foreground.append(request.url)
    page.on('request', record)
    page.goto(sys.argv[1] + sys.argv[2], wait_until='domcontentloaded')
    page.wait_for_function('() => CloudImageResources.backgroundActive === 2')
    page.locator('#fileContainer .file-item[data-gallery-image]').first.click()
    page.wait_for_function('() => document.querySelector("#preview-stage img")?.naturalWidth === 64')
    page.wait_for_function('() => document.querySelector("#zip-gallery-stage img")?.naturalWidth === 64')
    assert len(foreground) == 1, 'preview and gallery should share promoted original'

    page.wait_for_function('() => CloudImageResources.waiting.length > 0')
    assert page.evaluate('CloudImageResources.backgroundActive') == 4
    page.locator('#zip-gallery-next').click()
    page.wait_for_function('() => document.querySelector("#zip-gallery-stage img")?.naturalWidth === 65')
    assert len(foreground) == 2, 'gallery must promote its in-flight prefetched neighbor'

    page.locator('#zip-gallery-jump-input').fill('10')
    page.locator('#zip-gallery-jump-input').press('Enter')
    page.wait_for_function('() => document.querySelector("#zip-gallery-stage img")?.naturalWidth === 73')
    assert len(foreground) == 3, 'queued target must bypass background queue'
    assert page.evaluate('CloudImageResources.backgroundActive') <= 4
    print('PASS: selected thumbnail, running neighbor and queued jump all display while background responses remain blocked')
    browser.close()
