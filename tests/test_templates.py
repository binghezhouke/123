"""模板渲染冒烟测试。

不依赖网络和真实的 config.json：应用工厂接受配置路径，模板只做纯渲染。
这类测试能挡住"路由引用了不存在的模板"（历史上 /demo/webdav 就踩过）。
"""

import json

import pytest

from api.models import File


@pytest.fixture
def app(tmp_path):
    from app import create_app

    config_path = tmp_path / "config.json"
    config_path.write_text(json.dumps({
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "SECRET_KEY": "test",
    }), encoding="utf-8")

    flask_app = create_app(str(config_path))
    flask_app.config["TESTING"] = True
    return flask_app


@pytest.fixture
def file_obj():
    return File({
        "fileId": 1,
        "filename": "示例 视频.mp4",
        "size": 1024 * 1024,
        "type": 0,
        "category": 2,
        "parentFileId": 0,
        "etag": "abc",
        "updateAt": "2026-01-01 00:00:00",
    })


@pytest.mark.parametrize("template,context", [
    ("error.html", {"error": "出错啦"}),
    ("files.html", {"files": [], "parent_id": 0}),
    ("search.html", {"files": [], "search_query": "", "search_mode": 0}),
])
def test_simple_templates_render(app, template, context):
    with app.test_request_context("/"):
        html = app.jinja_env.get_template(template).render(**context)

    assert "CloudArchive" in html


def test_file_detail_template_renders(app, file_obj):
    with app.test_request_context("/"):
        html = app.jinja_env.get_template("file_detail.html").render(
            file=file_obj, download_url=None, webdav_url=None)

    assert "示例 视频.mp4" in html


def test_demo_webdav_template_renders(app, file_obj):
    with app.test_request_context("/"):
        html = app.jinja_env.get_template("demo_webdav.html").render(
            file=file_obj, webdav_url="https://u:p@webdav.example.cn/webdav/a.mp4")

    assert "WebDAV" in html
    assert "示例 视频.mp4" in html


class StubClient:
    """路由层的客户端替身：页面渲染不碰网络"""

    def __init__(self, file_obj):
        self.file_obj = file_obj

    def is_webdav_available(self):
        return True

    def get_file_info_single(self, file_id, use_cache=True):
        return self.file_obj

    def get_webdav_url(self, file_id, use_cache=True):
        return "https://user:pw@webdav.example.cn/webdav/a.mp4"

    def get_download_info(self, file_id):
        return {"data": {"downloadUrl": "https://cdn.example.cn/a.mp4"}}


@pytest.mark.parametrize("path", ["/file/1", "/demo/webdav/1"])
def test_webdav_pages_render(app, file_obj, monkeypatch, path):
    """历史上 /demo/webdav 引用了不存在的模板，这里连路由一起验证。"""
    import routes.main

    monkeypatch.setattr(routes.main, "get_client", lambda: StubClient(file_obj))

    response = app.test_client().get(path)

    assert response.status_code == 200
    assert "示例 视频.mp4" in response.get_data(as_text=True)
