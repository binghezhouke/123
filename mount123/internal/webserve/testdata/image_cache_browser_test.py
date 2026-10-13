"""Actual archive UI: a displayed thumbnail must not redownload its original."""
from pathlib import Path
import sys
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright

base = sys.argv[1]
with sync_playwright() as pw:
    executable = Path(pw.chromium.executable_path)
    if not executable.is_file():
        executable = sorted(Path.home().glob('.cache/ms-playwright/chromium-*/chrome-linux64/chrome'))[-1]
    browser = pw.chromium.launch(headless=True, executable_path=str(executable))
    context = browser.new_context(viewport={'width': 1200, 'height': 850})
    # Avoid depending on external fonts/bootstrap. Keep actual local assets.
    context.route('**/*', lambda r: r.continue_() if urlparse(r.request.url).hostname == '127.0.0.1' else r.abort())
    page = context.new_page()
    requests = []
    successful_images = []
    page.on('request', lambda r: requests.append(r.url) if '/member?' in r.url or '/preview' in r.url else None)
    page.on('response', lambda r: successful_images.append(r.url) if r.status == 200 and ('/member?' in r.url or '/preview' in r.url) and r.headers.get('content-type', '').startswith('image/') else None)
    page.goto(base + (sys.argv[2] if len(sys.argv) > 2 else '/archive/1'), wait_until='networkidle')
    item = page.locator('#fileContainer .file-item[data-gallery-image]').first
    item.locator('.preview-prefetch-thumb').wait_for(state='visible')
    before = len(requests)
    # The ordinary-page fixture may probe a direct redirect before the CSP
    # makes it fall back to the local proxy. Only one image body may succeed.
    assert len(successful_images) == 1, f'one thumbnail read {len(successful_images)} original bodies'
    item.click()
    page.wait_for_function("() => { const i=document.querySelector('#preview-stage img'); return i && i.complete && i.naturalWidth === 64; }")
    after = len(requests)
    print(f'thumbnail requests={before}; after opening original={after}', flush=True)
    assert after == before, f'clicking cached thumbnail refetched original: {before} -> {after}'
    page.wait_for_function("() => { const i=document.querySelector('#zip-gallery-stage img'); return i && i.complete && i.naturalWidth === 64; }")
    page.locator('#zip-gallery-close').click()
    item.click()
    page.wait_for_function("() => { const i=document.querySelector('#zip-gallery-stage img'); return i && i.complete && i.naturalWidth === 64; }")
    assert len(requests) == before, 'reopening gallery refetched original'
    page.reload(wait_until='networkidle')
    item.locator('.preview-prefetch-thumb').wait_for(state='visible')
    item.click()
    page.wait_for_function("() => { const i=document.querySelector('#zip-gallery-stage img'); return i && i.complete && i.naturalWidth === 64; }")
    assert len(requests) == before, 'page reload lost persistent original'
    # The persistent cache uses a count bound and encoded blobs. Enforce the
    # bound with tiny synthetic entries, without downloading more images.
    page.evaluate("""async () => {
        const store = CloudImageResources.store;
        for (let n = 0; n < 202; n++) await store.put('synthetic-' + n, '', new Blob(['x'], {type:'image/png'}));
    }""")
    count = page.evaluate("""async () => {
        const db = await CloudImageResources.store.db;
        return await new Promise(resolve => { const r = db.transaction('images').objectStore('images').count(); r.onsuccess = () => resolve(r.result); });
    }""")
    assert count == 200, f'original disk cache is not bounded: {count}'
    assert page.evaluate("async () => (await CloudImageResources.store.get('synthetic-0', '')) === null")
    print('reopen + reload reuse passed; original disk cache capped at 200 encoded images', flush=True)
    browser.close()
