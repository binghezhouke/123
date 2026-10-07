# ruff: noqa: F811
"""Sort and filter observable HTML/JSON across upstream page boundaries."""
import html
import re

from test_file_browser import app, BrowserClient, make_file  # noqa: F401
from test_zip_preview import browser, remote, make_zip  # noqa: F401


def setup_pages(monkeypatch):
    client = BrowserClient()
    pages = {None: ([make_file(1, '10.jpg'), make_file(2, 'movie.mp4')], 2),
             2: ([make_file(3, '2.jpg'), make_file(4, '1.jpg')], -1)}
    def listing(**kwargs):
        client.list_calls.append(kwargs)
        return pages[kwargs.get('last_file_id')]
    client.list_files = listing
    monkeypatch.setattr('routes.main.get_client', lambda: client)
    return client


def more(response):
    return html.unescape(re.search(r'href="([^"]+)"\s+class="btn btn-primary" data-load-more',
                                  response.get_data(as_text=True)).group(1))


def test_sort_filter_cover_all_pages_and_keep_snapshot(app, monkeypatch):
    upstream = setup_pages(monkeypatch)
    web = app.test_client()
    first = web.get('/?sort=name&kind=image&limit=1')
    assert first.status_code == 200
    assert b'data-file-id="4"' in first.data
    assert b'data-file-id="1"' not in first.data
    second = web.get(more(first))
    assert b'data-file-id="3"' in second.data
    third = web.get(more(second))
    assert b'data-file-id="1"' in third.data
    assert b'data-load-more' not in third.data
    assert len(upstream.list_calls) == 2


def test_refresh_sorted_snapshot_starts_again(app, monkeypatch):
    setup_pages(monkeypatch)
    web = app.test_client()
    first = web.get('/?sort=name&kind=image&limit=1')
    refreshed = web.get(more(first) + '&refresh=1', headers={'X-Requested-With': 'XMLHttpRequest'})
    assert refreshed.status_code == 200
    assert b'data-file-id="4"' in refreshed.data


def test_directory_image_index_is_naturally_sorted(app, monkeypatch):
    setup_pages(monkeypatch)
    response = app.test_client().get('/api/directory-images?sort=name')
    assert response.status_code == 200
    assert [f['id'] for f in response.json['items']] == [4, 3, 1]
    assert response.json['next'] is None


def test_archive_filter_sort_preserves_member_identity(browser, remote):
    remote.data = make_zip([('10.jpg', b'ten'), ('movie.mp4', b'video'), ('2.jpg', b'two')])
    response = browser.get('/file/1/zip?sort=name&kind=image')
    text = response.get_data(as_text=True)
    assert response.status_code == 200
    assert text.index('archive-member-2') < text.index('archive-member-0')
    assert 'archive-member-1' not in text
    assert '/zip/member/2?' in text


def test_gallery_index_continues_over_empty_upstream_page(app, monkeypatch):
    client = BrowserClient()
    def listing(**kwargs):
        return ([], 10) if kwargs.get('last_file_id') is None else ([make_file(11, 'after.jpg')], -1)
    client.list_files = listing
    monkeypatch.setattr('routes.main.get_client', lambda: client)
    response = app.test_client().get('/api/directory-images')
    assert response.status_code == 200
    assert [f['id'] for f in response.json['items']] == [11]


def test_image_disk_full_shows_actionable_error(browser, remote, monkeypatch):
    remote.data = make_zip([('photo.png', b'image')])
    def no_space(*args, **kwargs):
        raise OSError(28, 'disk full')
    monkeypatch.setattr('api.zip_preview.tempfile.TemporaryFile', no_space)
    response = browser.get('/file/1/zip/member/0')
    assert response.status_code == 507
    assert '磁盘空间'.encode() in response.data


def test_archive_video_navigation_follows_natural_sort(browser, remote):
    import zipfile
    remote.data = make_zip([('10.mp4', b'video'), ('2.mp4', b'video'), ('1.mp4', b'video')], zipfile.ZIP_STORED)
    response = browser.get('/file/1/zip/member/1?sort=name')
    text = html.unescape(response.get_data(as_text=True))
    assert response.status_code == 200
    assert re.search(r'href="/file/1/zip/member/2\?[^" ]*sort=name[^" ]*">上一个视频', text)
    assert re.search(r'href="/file/1/zip/member/0\?[^" ]*sort=name[^" ]*">下一个视频', text)
