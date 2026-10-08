"""FileService 的行为测试：分页、预上传、秒传语义、目录缓存、WebDAV URL。"""

import logging
import hashlib
import os
import stat

import pytest
import requests

from api.exceptions import Pan123APIError, ValidationError
from conftest import make_file, make_page
from api.models import File, FileList


def test_file_infos_deduplicates_chunks_and_preserves_input_order(service, http_client):
    from api.file_service import FILE_INFO_BATCH_SIZE

    ids = list(range(1, FILE_INFO_BATCH_SIZE + 2)) + [1]
    http_client.queue_post(
        "/api/v1/file/infos",
        {"code": 0, "data": {"fileList": [make_file(i, str(i)) for i in reversed(ids[:FILE_INFO_BATCH_SIZE])]}},
        {"code": 0, "data": {"fileList": [make_file(ids[-2], str(ids[-2]))]}},
    )
    result = service.get_files_info(ids, use_cache=False)
    assert [item.file_id for item in result] == list(range(1, FILE_INFO_BATCH_SIZE + 2))
    batches = [call[2]["fileIds"] for call in http_client.calls]
    assert [len(batch) for batch in batches] == [FILE_INFO_BATCH_SIZE, 1]


@pytest.mark.parametrize("file_ids", [[], None])
def test_file_infos_rejects_empty_or_missing_ids(service, file_ids):
    with pytest.raises(ValidationError):
        service.get_files_info(file_ids)


def test_file_infos_mixed_cache_hit_and_miss_preserves_requested_order(service, http_client):
    class Cache:
        def should_use_cache(self, file_id, update_time=None):
            if file_id == 2 and update_time is None:
                return True, make_file(2, "cached")
            return False, None

        def set_cache(self, file_id, data):
            pass

    service.cache_manager = Cache()
    http_client.queue_post("/api/v1/file/infos", {
        "code": 0, "data": {"fileList": [make_file(3, "api-3"), make_file(1, "api-1")]}
    })
    result = service.get_files_info([3, 2, 1])
    assert [(item.file_id, item.filename) for item in result] == [
        (3, "api-3"), (2, "cached"), (1, "api-1")]


def test_file_detail_normalizes_api_id_field_variants(service, http_client):
    http_client.queue_get("/api/v1/file/detail", {
        "code": 0,
        "data": {"fileID": 7, "parentFileID": 4, "filename": "a.txt",
                 "createAt": "today", "type": 0, "size": 12, "trashed": 0,
                 "etag": "tag"},
    })
    detail = service.get_file_detail(7)
    assert (detail.file_id, detail.parent_file_id, detail.filename) == (7, 4, "a.txt")
    assert http_client.calls[-1] == ("GET", "/api/v1/file/detail", {"fileID": 7})


def _zip_file(file_id=41, parent_id=8, filename="archive.zip", type_=0, trashed=0):
    return File({"fileId": file_id, "parentFileId": parent_id, "filename": filename,
                 "type": type_, "trashed": trashed, "size": 123})


@pytest.mark.parametrize("name,sidecar", [("archive.zip", "archive.zip.pwd"),
    ("archive.7z", "archive.7z.pwd"), ("archive.rar", "archive.rar.pwd"),
    ("archive.7z.001", "archive.7z.pwd"), ("archive.7z.002", "archive.7z.pwd")])
def test_save_zip_password_uploads_exact_private_temp_sidecar_and_invalidates_cache(
        service, monkeypatch, caplog, name, sidecar):
    password = "密碼🔑 "
    expected = password.encode("utf-8")
    service._dir_cache[(8, 100, 100)] = (FileList([]), None)
    monkeypatch.setattr(service, "get_file_detail", lambda file_id: _zip_file(filename=name))
    list_calls = []

    def list_files(**kwargs):
        list_calls.append(kwargs)
        return FileList([_zip_file(77, 8, sidecar)]), None

    monkeypatch.setattr(service, "list_files", list_files)
    upload_calls = []

    def upload_file(local_path, parent_id, **kwargs):
        upload_calls.append((local_path, parent_id, kwargs))
        assert os.path.exists(local_path)
        assert stat.S_IMODE(os.stat(local_path).st_mode) == 0o600
        assert open(local_path, "rb").read() == expected
        return {"fileID": 90, "filename": kwargs["filename"], "size": len(expected)}

    monkeypatch.setattr(service, "upload_file", upload_file)
    result = service.save_archive_password(41, password)

    assert result["fileID"] == 90
    assert list_calls == [{"parent_id": 8, "auto_fetch_all": True, "use_cache": False}]
    temp_path, parent_id, options = upload_calls[0]
    assert parent_id == 8
    assert options == {"filename": sidecar, "duplicate": 2,
                       "skip_if_exists": False, "try_sha1_reuse": False,
                       "sensitive": True}
    assert not os.path.exists(temp_path)
    assert service._dir_cache == {}
    assert password not in caplog.text
    assert hashlib.md5(expected).hexdigest() not in caplog.text


@pytest.mark.parametrize("siblings", [
    [make_file(88, "archive.zip.pwd", type_=1)],
    [make_file(88, "archive.zip.pwd"), make_file(89, "archive.zip.pwd")],
])
def test_save_zip_password_rejects_conflicting_sibling_before_upload(
        service, monkeypatch, siblings):
    monkeypatch.setattr(service, "get_file_detail", lambda file_id: _zip_file())
    monkeypatch.setattr(service, "list_files", lambda **kwargs: (FileList(siblings), None))
    monkeypatch.setattr(service, "upload_file", lambda *a, **kw: pytest.fail("must not upload"))

    with pytest.raises(ValidationError):
        service.save_zip_password(41, "secret")


def test_save_zip_password_refuses_missing_parent_and_invalid_archive(service, monkeypatch):
    monkeypatch.setattr(service, "list_files", lambda **kwargs: pytest.fail("must not list"))
    monkeypatch.setattr(service, "get_file_detail", lambda file_id: _zip_file(parent_id=None))
    with pytest.raises(ValidationError, match="父目录"):
        service.save_zip_password(41, "secret")

    monkeypatch.setattr(service, "get_file_detail", lambda file_id: _zip_file(filename="archive.txt"))
    with pytest.raises(ValidationError):
        service.save_zip_password(41, "secret")


@pytest.mark.parametrize("password", ["", "x" * 4097, "\ud800"])
def test_save_zip_password_validates_utf8_and_byte_limit_before_api(service, monkeypatch, password):
    monkeypatch.setattr(service, "get_file_detail", lambda file_id: pytest.fail("must not request detail"))
    with pytest.raises(ValidationError):
        service.save_zip_password(41, password)


def test_save_zip_password_cleans_temp_file_when_upload_fails(service, monkeypatch):
    monkeypatch.setattr(service, "get_file_detail", lambda file_id: _zip_file())
    monkeypatch.setattr(service, "list_files", lambda **kwargs: (FileList([]), None))
    captured = {}

    def fail_upload(local_path, *args, **kwargs):
        captured["path"] = local_path
        assert stat.S_IMODE(os.stat(local_path).st_mode) == 0o600
        raise Pan123APIError("upload failed")

    monkeypatch.setattr(service, "upload_file", fail_upload)
    with pytest.raises(Pan123APIError):
        service.save_zip_password(41, "secret")
    assert captured["path"]
    assert not os.path.exists(captured["path"])


def test_sensitive_upload_does_not_log_secret_or_its_md5(service, tmp_path, monkeypatch, caplog):
    password_bytes = "secret-内容".encode("utf-8")
    password_file = tmp_path / "password.pwd"
    password_file.write_bytes(password_bytes)
    monkeypatch.setattr(service, "create_file", lambda **kwargs: {"reuse": True, "fileID": 10})

    with caplog.at_level(logging.INFO):
        result = service.upload_file(
            str(password_file), 8, filename="archive.zip.pwd",
            try_sha1_reuse=False, sensitive=True)

    assert result["fileID"] == 10
    assert password_bytes.decode("utf-8") not in caplog.text
    assert hashlib.md5(password_bytes).hexdigest() not in caplog.text


def test_sensitive_chunk_upload_hides_slice_hash_endpoint_and_preupload_id(
        service, http_client, tmp_path, monkeypatch, caplog):
    content = b"sidecar secret bytes"
    password_file = tmp_path / "password.pwd"
    password_file.write_bytes(content)
    monkeypatch.setattr(service, "create_file", lambda **kwargs: {
        "preuploadID": "private-preupload-id", "sliceSize": 5,
        "servers": ["https://upload.example.test"],
    })
    http_client.queue_post(
        "https://upload.example.test/upload/v2/file/slice",
        *([{"code": 0}] * ((len(content) + 4) // 5)),
    )
    http_client.queue_post("/upload/v2/file/upload_complete", {
        "code": 0, "data": {"completed": True, "fileID": 91},
    })

    with caplog.at_level(logging.INFO):
        result = service.upload_file(
            str(password_file), 8, filename="archive.zip.pwd",
            try_sha1_reuse=False, sensitive=True)

    assert result["fileID"] == 91
    assert hashlib.md5(content[:5]).hexdigest() not in caplog.text
    assert "upload.example.test" not in caplog.text
    assert "private-preupload-id" not in caplog.text


def test_sensitive_chunk_upload_hides_exception_detail(service, http_client, tmp_path, monkeypatch, caplog):
    password_file = tmp_path / "password.pwd"
    password_file.write_bytes(b"secret chunk")
    monkeypatch.setattr(service, "create_file", lambda **kwargs: {
        "preuploadID": "private-preupload-id", "sliceSize": 100,
        "servers": ["upload.example.test"],
    })
    http_client.queue_post("http://upload.example.test/upload/v2/file/slice",
                           Pan123APIError("secret exception detail"))

    with caplog.at_level(logging.INFO):
        result = service.upload_file(
            str(password_file), 8, filename="archive.zip.pwd",
            try_sha1_reuse=False, sensitive=True)

    assert result is None
    assert "secret exception detail" not in caplog.text
    assert "upload.example.test" not in caplog.text
    assert "private-preupload-id" not in caplog.text


# ---------------------------------------------------------------- 分页

def test_file_infos_normalizes_uppercase_integer_id(service, http_client):
    http_client.queue_post("/api/v1/file/infos", {
        "code": 0, "data": {"fileList": [
            {"fileID": 42, "parentFileID": 7, "filename": "file.txt", "type": 0}
        ]}
    })
    result = service.get_files_info([42], use_cache=False)
    assert result[0].file_id == 42
    assert result[0].parent_file_id == 7

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
