"""Manual detection must not turn ordinary navigation into remote content reads."""
import io

import py7zr
import pytest

import test_file_browser
import test_zip_preview
from api.archive_probe import detect_archive, detected_kind, remember_kind
from test_file_browser import BrowserClient, make_file

app = test_file_browser.app
remote = test_zip_preview.remote


@pytest.mark.parametrize('header,kind', [
    (b'7z\xbc\xaf\x27\x1c\0\3', '.7z'),
    (b'Rar!\x1a\x07\x00', '.rar'),
    (b'Rar!\x1a\x07\x01\x00', '.rar'),
    (b'PK\x03\x04', '.zip'), (b'PK\x05\x06', '.zip'),
    (b'not archive', None), (b'7z', None),
])
def test_probe_is_bounded_to_signature(remote, header, kind):
    remote.data = header + b'\0' * 100_000
    assert detect_archive('https://download.test/file') == kind
    assert sum(end - start + 1 for start, end in remote.ranges) <= 9


def client_for(monkeypatch, file):
    client = BrowserClient({7: file})
    for module in ['routes.api', 'routes.main', 'routes.zip_browser']:
        monkeypatch.setattr(f'{module}.get_client', lambda: client)
    return client


def test_only_clicking_probe_enables_extensionless_archive(app, remote, monkeypatch):
    remote.data = test_zip_preview.make_zip([('hello.txt', b'hello')])
    file = make_file(7, '01')
    client = client_for(monkeypatch, file)
    links = []
    client.get_final_download_url = lambda file_id, prefer_webdav=True: (
        links.append(prefer_webdav) or ('https://download.test/file', 'api'))
    web = app.test_client()
    page = web.get('/file/7')
    assert page.status_code == 200
    assert b'id="archive-probe"' in page.data
    assert remote.ranges == [] and links == []
    assert web.get('/api/archive/probe/7').status_code == 405
    assert web.post('/api/archive/probe/7').status_code == 415
    assert remote.ranges == []
    response = web.post('/api/archive/probe/7', json={})
    assert response.json['kind'] == '.zip'
    assert response.json['browse_url'] == '/file/7/zip'
    assert links == [False]
    assert max(end for start, end in remote.ranges) <= 7
    listing = web.get(response.json['browse_url'])
    assert b'hello.txt' in listing.data
    assert web.get('/file/7/zip/member/0').data == b'hello'
    # Detection is scoped to the browser, not a global filename override.
    assert app.test_client().get('/file/7/zip').status_code == 400


def test_detected_encrypted_7z_enters_existing_password_flow(app, remote, monkeypatch):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, 'w', password='test', header_encryption=True) as archive:
        archive.writestr(b'hello', 'hello.txt')
    remote.data = data.getvalue()
    client_for(monkeypatch, make_file(7, '01'))
    web = app.test_client()
    result = web.post('/api/archive/probe/7', json={})
    assert result.json['kind'] == '.7z'
    page = web.get(result.json['browse_url'])
    assert page.status_code == 401
    assert b'id="zip-password-entry"' in page.data


def test_unknown_and_range_failure_do_not_offer_browse(app, remote, monkeypatch):
    client_for(monkeypatch, make_file(7, '01'))
    web = app.test_client()
    remote.data = b'plain text'
    response = web.post('/api/archive/probe/7', json={})
    assert response.json['kind'] is None and response.json['browse_url'] is None
    remote.status = 200
    response = web.post('/api/archive/probe/7', json={})
    assert response.status_code == 502
    assert 'https://' not in response.json['error']


def test_detection_is_bounded_and_invalidated_by_metadata_changes():
    session = {}
    for i in range(40):
        remember_kind(session, make_file(i, str(i)), '.7z')
    assert len(session['detected_archives']) == 24
    file = make_file(39, '39')
    assert detected_kind(session, file) == '.7z'
    assert detected_kind(session, make_file(39, 'changed')) is None
    remember_kind(session, file, None)
    assert detected_kind(session, file) is None
