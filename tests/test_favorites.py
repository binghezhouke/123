"""Favorites are local; rendering must work without cloud access and escape filenames."""
import pytest

import test_file_browser
from test_file_browser import BrowserClient, make_file

app = test_file_browser.app


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
