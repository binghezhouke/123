"""FileService 的行为测试：分页、预上传、秒传语义、目录缓存、WebDAV URL。"""

import logging

import pytest
import requests

from api.exceptions import Pan123APIError, ValidationError
from conftest import make_file, make_page


# ---------------------------------------------------------------- 分页

def test_pagination_keeps_going_when_a_page_is_all_trashed(service, http_client):
    """整页都是回收站记录时不能提前结束分页（曾经会静默丢文件）。"""
    http_client.queue_get(
        "/api/v2/file/list",
        make_page([make_file(1, "a.txt", trashed=1),
                   make_file(2, "keep.txt")], 111),
        make_page([make_file(3, "trashed.txt", trashed=1)], 222),
        make_page([make_file(4, "last.txt")], -1),
    )

    file_list, next_id = service.list_files(parent_id=0, auto_fetch_all=True)

    assert [f.filename for f in file_list] == ["keep.txt", "last.txt"]
    assert next_id is None
    assert http_client.count_calls("/api/v2/file/list") == 3


def test_pagination_accepts_string_trashed_flag(service, http_client):
    """trashed 字段可能是字符串或布尔值，过滤逻辑要都能处理。"""
    http_client.queue_get(
        "/api/v2/file/list",
        make_page([make_file(1, "a.txt", trashed="1"),
                   make_file(2, "b.txt", trashed=True),
                   make_file(3, "c.txt", trashed=0),
                   make_file(4, "d.txt", trashed=None)], -1),
    )

    file_list, _ = service.list_files(parent_id=0, auto_fetch_all=True)

    assert [f.filename for f in file_list] == ["c.txt", "d.txt"]


def test_pagination_stops_on_empty_raw_page(service, http_client):
    """服务端返回空页（即使 lastFileId 不是 -1）就直接结束。"""
    http_client.queue_get("/api/v2/file/list", make_page([], 333))

    file_list, _ = service.list_files(parent_id=0, auto_fetch_all=True)

    assert len(file_list) == 0
    assert http_client.count_calls("/api/v2/file/list") == 1


def test_pagination_respects_max_pages(service, http_client):
    for page in range(5):
        http_client.queue_get(
            "/api/v2/file/list",
            make_page([make_file(page, f"f{page}.txt")], page + 1),
        )

    file_list, _ = service.list_files(parent_id=0, auto_fetch_all=True, max_pages=2)

    assert len(file_list) == 2
    assert http_client.count_calls("/api/v2/file/list") == 2


def test_list_files_reports_each_page(service, http_client):
    """自动翻页时要能把进度回报出去（拉大目录时用户才知道没卡住）。"""
    http_client.queue_get(
        "/api/v2/file/list",
        make_page([make_file(1, "a.txt")], 11),
        make_page([make_file(2, "b.txt")], -1),
    )
    seen = []

    service.list_files(parent_id=0, auto_fetch_all=True,
                       on_page=lambda *args: seen.append(args))

    assert seen == [(1, 1, 1), (2, 1, 2)]


def test_single_page_returns_next_id(service, http_client):
    http_client.queue_get("/api/v2/file/list", make_page([make_file(1, "a.txt")], 55))

    file_list, next_id = service.list_files(parent_id=0, limit=20)

    assert len(file_list) == 1
    assert next_id == 55


# ---------------------------------------------------------------- 目录缓存

def test_dir_cache_is_reused_and_invalidated_by_mkdir(service, http_client):
    http_client.queue_get("/api/v2/file/list", make_page([make_file(1, "a.txt")], -1))
    http_client.queue_post("/upload/v1/file/mkdir", {"code": 0, "data": {"dirID": 42}})
    http_client.queue_get("/api/v2/file/list", make_page([make_file(1, "a.txt")], -1))

    service.list_files(parent_id=0, auto_fetch_all=True)
    service.list_files(parent_id=0, auto_fetch_all=True)
    assert http_client.count_calls("/api/v2/file/list") == 1, "第二次应命中目录缓存"

    assert service.mkdir("newdir", 0) == 42
    service.list_files(parent_id=0, auto_fetch_all=True)
    assert http_client.count_calls("/api/v2/file/list") == 2, "新建目录后缓存应失效"


def test_dir_cache_key_includes_pagination_params(service, http_client):
    """max_pages 不同的结果不能互相复用，否则截断结果会污染完整结果。"""
    http_client.queue_get("/api/v2/file/list", make_page([make_file(1, "a.txt")], -1))
    http_client.queue_get("/api/v2/file/list", make_page([make_file(1, "a.txt")], -1))

    service.list_files(parent_id=0, auto_fetch_all=True, max_pages=1)
    service.list_files(parent_id=0, auto_fetch_all=True, max_pages=100)

    assert http_client.count_calls("/api/v2/file/list") == 2


# ---------------------------------------------------------------- 创建文件校验

@pytest.mark.parametrize("filename", ['a/b.txt', 'a\\b.txt', 'a:b.txt', 'a*b.txt'])
def test_create_file_rejects_illegal_chars(service, filename):
    with pytest.raises(ValidationError):
        service.create_file(parent_id=0, filename=filename, etag="x", size=1)


def test_create_file_allows_slash_when_contain_dir(service, http_client):
    http_client.queue_post("/upload/v2/file/create",
                           {"code": 0, "data": {"reuse": True, "fileID": 9}})

    result = service.create_file(parent_id=0, filename="dir/a.txt", etag="x",
                                 size=1, contain_dir=True)

    assert result["fileID"] == 9


def test_create_file_rejects_overlong_name(service):
    with pytest.raises(ValidationError):
        service.create_file(parent_id=0, filename="啊" * 100, etag="x", size=1)


# ---------------------------------------------------------------- 预上传兜底

def test_create_file_fallback_reports_skipped_not_reuse(service, http_client):
    """预上传失败但云端已有同名同大小文件：应报告"已存在"，而不是伪装成秒传成功。"""
    http_client.queue_post("/upload/v2/file/create", Pan123APIError("boom"))
    http_client.queue_get("/api/v2/file/list",
                          make_page([make_file(7, "a.txt", size=5)], -1))

    result = service.create_file(parent_id=0, filename="a.txt", etag="x", size=5)

    assert result["skipped"] is True
    assert result.get("reuse") is None
    assert result["fileID"] == 7


def test_create_file_fallback_reraises_on_overwrite(service, http_client):
    """duplicate=2 是覆盖上传，不能把已有文件当成成功。"""
    http_client.queue_post("/upload/v2/file/create", Pan123APIError("boom"))

    with pytest.raises(Pan123APIError):
        service.create_file(parent_id=0, filename="a.txt", etag="x", size=5,
                            duplicate=2)


def test_create_file_fallback_reraises_when_size_differs(service, http_client):
    http_client.queue_post("/upload/v2/file/create", Pan123APIError("boom"))
    http_client.queue_get("/api/v2/file/list",
                          make_page([make_file(7, "a.txt", size=999)], -1))

    with pytest.raises(Pan123APIError):
        service.create_file(parent_id=0, filename="a.txt", etag="x", size=5)


# ---------------------------------------------------------------- 秒传语义

def test_try_sha1_reuse_returns_none_on_miss(service, http_client):
    http_client.queue_post("/upload/v2/file/sha1_reuse",
                           {"code": 0, "data": {"reuse": False}})

    result = service.try_sha1_reuse(None, "a.txt", 0, sha1="a" * 40, size=5)

    assert result is None


def test_try_sha1_reuse_returns_data_on_hit(service, http_client):
    http_client.queue_post("/upload/v2/file/sha1_reuse",
                           {"code": 0, "data": {"reuse": True, "fileID": 3}})

    result = service.try_sha1_reuse(None, "a.txt", 0, sha1="a" * 40, size=5)

    assert result["reuse"] is True
    assert result["fileID"] == 3


def test_try_sha1_reuse_raises_on_api_failure(service, http_client):
    """接口调用失败必须抛出异常，调用方才能和"未命中"区分开。"""
    http_client.queue_post("/upload/v2/file/sha1_reuse", Pan123APIError("500"))

    with pytest.raises(Pan123APIError):
        service.try_sha1_reuse(None, "a.txt", 0, sha1="a" * 40, size=5)


def test_try_sha1_reuse_raises_on_empty_response(service, http_client):
    http_client.queue_post("/upload/v2/file/sha1_reuse", {})

    with pytest.raises(Pan123APIError):
        service.try_sha1_reuse(None, "a.txt", 0, sha1="a" * 40, size=5)


def test_try_sha1_reuse_needs_local_file_when_metadata_missing(service):
    with pytest.raises(FileNotFoundError):
        service.try_sha1_reuse("/no/such/file", "a.txt", 0)


# ---------------------------------------------------------------- WebDAV

def test_webdav_url_uses_config_prefix_and_escapes_credentials(service, monkeypatch):
    service.config = {
        "webdav_user": "user@example.com",
        "webdav_password": "p:a ss",
        "webdav_host": "webdav.example.cn",
        "webdav_path_prefix": "/dav",
    }
    monkeypatch.setattr(service, "get_file_path", lambda *a, **k: "/dir/a b.txt")

    url = service.get_webdav_url(1, use_cache=False)

    assert url.startswith("https://user%40example.com:p%3Aa%20ss@webdav.example.cn/dav/")
    assert url.endswith("dir/a%20b.txt")


def test_webdav_url_returns_none_without_credentials(service, monkeypatch):
    service.config = {"webdav_host": "h"}
    monkeypatch.setattr(service, "get_file_path", lambda *a, **k: "/a.txt")

    assert service.get_webdav_url(1) is None


def test_mask_credentials_hides_password():
    from api.file_service import _mask_credentials

    assert _mask_credentials("https://u:p@host/path") == "https://***@host/path"
    assert _mask_credentials("https://host/path") == "https://host/path"


def test_webdav_redirect_does_not_buffer_body_and_hides_password(service, monkeypatch, caplog):
    """跳转探测必须 stream（否则 200 响应会把整个文件读进内存），日志不能带密码。"""
    service.config = {
        "webdav_user": "user",
        "webdav_password": "sup3rsecret",
        "webdav_host": "webdav.example.cn",
    }
    monkeypatch.setattr(service, "get_file_path", lambda *a, **k: "/a.txt")

    captured = {}

    class FakeResponse:
        status_code = 302
        headers = {"Location": "https://cdn.example.cn/final"}

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_get(url, **kwargs):
        captured["url"] = url
        captured["kwargs"] = kwargs
        return FakeResponse()

    monkeypatch.setattr(requests, "get", fake_get)

    with caplog.at_level(logging.INFO):
        url = service.get_webdav_redirect_url(1, use_cache=False)

    assert url == "https://cdn.example.cn/final"
    assert captured["kwargs"]["stream"] is True
    assert captured["kwargs"]["allow_redirects"] is False
    assert "sup3rsecret" not in caplog.text


# ---------------------------------------------------------------- 上传流程

def test_upload_file_falls_back_to_normal_upload_when_sha1_fails(service, tmp_path, monkeypatch):
    local_file = tmp_path / "a.txt"
    local_file.write_text("hello", encoding="utf-8")

    def failing_sha1(*args, **kwargs):
        raise Pan123APIError("sha1_reuse 接口 500")

    monkeypatch.setattr(service, "try_sha1_reuse", failing_sha1)
    monkeypatch.setattr(service, "create_file",
                        lambda **kwargs: {"reuse": True, "fileID": 77})

    result = service.upload_file(str(local_file), parent_id=0)

    assert result["reuse"] is True
    assert result["fileID"] == 77


def test_upload_file_reports_skipped_from_preupload_fallback(service, tmp_path, monkeypatch):
    """预上传兜底发现同名同大小文件时，upload_file 应如实返回跳过而不是"秒传成功"。"""
    local_file = tmp_path / "a.txt"
    local_file.write_text("hello", encoding="utf-8")

    monkeypatch.setattr(service, "create_file",
                        lambda **kwargs: {"fileID": 5, "skipped": True, "existing": True})

    result = service.upload_file(str(local_file), parent_id=0, try_sha1_reuse=False)

    assert result["skipped"] is True
    assert result.get("reuse") is None
    assert result["fileID"] == 5
