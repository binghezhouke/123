"""Route coverage for directory navigation and inline previews."""

import json

import pytest

from api.models import File


@pytest.fixture
def app(tmp_path):
    from app import create_app

    config_path = tmp_path / "config.json"
    config_path.write_text(json.dumps({"CLIENT_ID": "id", "CLIENT_SECRET": "secret"}), encoding="utf-8")
    flask_app = create_app(str(config_path))
    flask_app.config["TESTING"] = True
    return flask_app


class BrowserClient:
    def __init__(self, files=None):
        self.files = files or {}
        self.list_calls = []
        self.info_calls = []

    def list_files(self, **kwargs):
        self.list_calls.append(kwargs)
        return [], None

    def get_file_info_single(self, file_id, use_cache=True):
        self.info_calls.append((file_id, use_cache))
        return self.files.get(file_id)

    def get_final_download_url(self, file_id, prefer_webdav=True):
        return (f"https://download.test/{file_id}", "api")

    def get_download_info(self, file_id):
        return {"data": {"downloadUrl": f"https://download.test/{file_id}"}}

    def is_webdav_available(self):
        return False


def make_file(file_id, filename, parent_id=0, type_=0):
    return File(
        {"fileId": file_id, "filename": filename, "parentFileId": parent_id, "type": type_, "size": 123, "category": 0}
    )


def test_index_page_cache_reuses_pages_and_refresh_refetches(app, monkeypatch):
    import routes.main

    client = BrowserClient()
    monkeypatch.setattr(routes.main, "get_client", lambda: client)
    web = app.test_client()

    assert web.get("/").status_code == 200
    assert web.get("/").status_code == 200
    assert len(client.list_calls) == 1

    assert web.get("/?refresh=1").status_code == 200
    assert len(client.list_calls) == 2


def test_index_breadcrumbs_link_to_ancestor_folders(app, monkeypatch):
    import routes.main

    client = BrowserClient(
        {
            20: make_file(20, "当前目录", parent_id=10, type_=1),
            10: make_file(10, "上层目录", parent_id=0, type_=1),
        }
    )
    monkeypatch.setattr(routes.main, "get_client", lambda: client)

    response = app.test_client().get("/?parent_id=20")
    html = response.get_data(as_text=True)
    assert response.status_code == 200
    assert 'href="/?parent_id=10"' in html
    assert "上层目录" in html
    assert "当前目录" in html


@pytest.mark.parametrize("filename", ["photo.jpg", "clip.mp4", "report.pdf", "notes.txt"])
def test_file_route_defaults_to_preview_for_supported_files(app, monkeypatch, filename):
    import routes.main

    file = make_file(7, filename, parent_id=3)
    client = BrowserClient({7: file})
    monkeypatch.setattr(routes.main, "get_client", lambda: client)

    response = app.test_client().get("/file/7")
    html = response.get_data(as_text=True)
    assert response.status_code == 200
    assert 'src="/file/7/content"' in html
    assert filename in html


def test_file_info_query_displays_details(app, monkeypatch):
    import routes.main

    client = BrowserClient({7: make_file(7, "photo.jpg")})
    monkeypatch.setattr(routes.main, "get_client", lambda: client)

    response = app.test_client().get("/file/7?info=1")
    assert response.status_code == 200
    html = response.get_data(as_text=True)
    assert "文件ID" in html
    assert 'src="/file/7/content"' not in html


@pytest.mark.parametrize("filename", ["bundle.zip", "bundle.7z", "bundle.rar"])
def test_file_route_redirects_archives_to_browser(app, monkeypatch, filename):
    import routes.main

    client = BrowserClient({7: make_file(7, filename)})
    monkeypatch.setattr(routes.main, "get_client", lambda: client)

    response = app.test_client().get("/file/7")
    assert response.status_code == 302
    assert response.headers["Location"].endswith("/file/7/zip")


class FakeUpstream:
    def __init__(self, body, status=200, headers=None):
        self.body = body
        self.status_code = status
        self.headers = headers or {}
        self.closed = False

    def iter_content(self, chunk_size):
        yield self.body

    def close(self):
        self.closed = True

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


def test_preview_forwards_range_header_and_content_range(app, monkeypatch):
    import routes.preview

    client = BrowserClient({7: make_file(7, "clip.mp4")})
    monkeypatch.setattr(routes.preview, "get_client", lambda: client)
    upstream = FakeUpstream(
        b"video", status=206, headers={"Content-Length": "5", "Content-Range": "bytes 0-4/20", "Accept-Ranges": "bytes"}
    )
    calls = []

    def fake_get(url, **kwargs):
        calls.append((url, kwargs))
        return upstream

    monkeypatch.setattr(routes.preview.requests, "get", fake_get)
    response = app.test_client().get("/file/7/content", headers={"Range": "bytes=0-4"})

    assert response.status_code == 206
    assert response.data == b"video"
    assert calls[0][1]["headers"]["Range"] == "bytes=0-4"
    assert response.headers["Content-Range"] == "bytes 0-4/20"


def test_html_text_preview_is_served_as_safe_plain_text(app, monkeypatch):
    import routes.preview

    client = BrowserClient({7: make_file(7, "page.html")})
    monkeypatch.setattr(routes.preview, "get_client", lambda: client)
    html_text = b"<script>alert(1)</script><h1>hello</h1>"
    upstream = FakeUpstream(html_text, headers={"Content-Length": str(len(html_text))})
    monkeypatch.setattr(routes.preview.requests, "get", lambda *args, **kwargs: upstream)

    response = app.test_client().get("/file/7/content")

    assert response.status_code == 200
    assert response.data == html_text
    assert response.mimetype == "text/plain"
    assert "sandbox" in response.headers["Content-Security-Policy"]
    assert response.headers["X-Content-Type-Options"] == "nosniff"


def test_page_cache_expiry_and_capacity(monkeypatch):
    from routes.browser_cache import PageCache

    clock = [100.0]
    monkeypatch.setattr("routes.browser_cache.monotonic", lambda: clock[0])
    cache = PageCache(ttl=30, capacity=2)
    cache.put((1, 20, None), "one")
    cache.put((2, 20, None), "two")
    cache.put((3, 20, None), "three")
    assert cache.get((1, 20, None)) is None
    assert cache.get((2, 20, None)) == "two"
    clock[0] += 31
    assert cache.get((2, 20, None)) is None
    assert cache.get((3, 20, None)) is None
