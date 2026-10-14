"""Cloud and nested archive ancestors survive partial navigation and reloads."""
from pathlib import Path
from urllib.parse import urlparse, parse_qs
import sys
from playwright.sync_api import sync_playwright

base = sys.argv[1]
with sync_playwright() as pw:
    executable = Path(pw.chromium.executable_path)
    if not executable.is_file():
        executable = sorted(Path.home().glob('.cache/ms-playwright/chromium-*/chrome-linux64/chrome'))[-1]
    browser = pw.chromium.launch(headless=True, executable_path=str(executable))
    page = browser.new_page(viewport={"width": 1440, "height": 900})
    page.route('**/*', lambda route: route.continue_() if urlparse(route.request.url).hostname == '127.0.0.1' else route.abort())
    page.goto(base + '/browse?parent_id=7', wait_until='domcontentloaded')
    page.evaluate('window.breadcrumbTestLifetime = true')
    page.locator('#fileContainer a').filter(has_text='photos.zip').click()
    page.wait_for_url('**/archive/8')
    nav = page.locator('.toolbar-breadcrumbs')

    def assert_links(names, paths):
        assert nav.locator('a').all_text_contents()[1:] == names
        hrefs = nav.locator('a').evaluate_all('(items) => items.map(el => el.getAttribute("href"))')
        assert hrefs[:4] == ['/browse?parent_id=0', '/browse?parent_id=2', '/browse?parent_id=7', '/archive/8'], hrefs
        assert [parse_qs(urlparse(href).query)['path'][0] for href in hrefs[4:]] == paths

    assert_links(['Album', 'Raw', 'photos.zip'], [])
    page.locator('#fileContainer a').filter(has_text='内层 & 照片.zip').click()
    page.wait_for_function('() => new URL(location.href).searchParams.get("path") === "内层 & 照片.zip"')
    page.locator('#fileContainer a').filter(has_text='pics/').click()
    page.wait_for_function('() => new URL(location.href).searchParams.get("path") === "内层 & 照片.zip/pics"')
    assert_links(['Album', 'Raw', 'photos.zip', '内层 & 照片.zip', 'pics'], ['内层 & 照片.zip', '内层 & 照片.zip/pics'])
    assert page.evaluate('window.breadcrumbTestLifetime'), 'navigation reloaded the document'
    nav.locator('a[href="/browse?parent_id=7"]').click()
    page.wait_for_url('**/browse?parent_id=7')
    assert page.locator('#fileContainer a').filter(has_text='photos.zip').count() == 1
    assert page.evaluate('window.breadcrumbTestLifetime'), 'parent jump reloaded the document'
    page.go_back()
    page.wait_for_function('() => new URL(location.href).searchParams.get("path") === "内层 & 照片.zip/pics"')
    page.wait_for_function('() => document.querySelectorAll(".toolbar-breadcrumbs a").length === 6')
    page.reload(wait_until='domcontentloaded')
    assert_links(['Album', 'Raw', 'photos.zip', '内层 & 照片.zip', 'pics'], ['内层 & 照片.zip', '内层 & 照片.zip/pics'])
    print('PASS: full cloud/nested breadcrumbs, partial parent jump, Back and reload')
    browser.close()
