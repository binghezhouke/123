"""upload_core 测试：哈希解析、清单解析、目录树缓存、秒传结果口径。"""

import json

import pytest

from api.exceptions import Pan123APIError
from api.models import FileList
import upload_core
from upload_core import (
    RemoteDirTree,
    RemoteIndex,
    STATUS_HIT,
    STATUS_MISS,
    STATUS_SKIP,
    decode_hash,
    load_manifest,
    normalize_remote_path,
    parse_export_text,
    reuse_file,
    upload_directory,
    upload_manifest,
)


# ---------------------------------------------------------------- 哈希解析

@pytest.mark.parametrize("raw,expected_type", [
    ("a" * 40, "sha1"),
    ("A" * 40, "sha1"),
    ("b" * 32, "md5"),
    ("0" * 8, "md5"),           # 不足长度按 md5 补零
    ("f" * 36, "sha1"),
])
def test_decode_hash_hex_inputs(raw, expected_type):
    hash_hex, hash_type = decode_hash(raw)

    assert hash_type == expected_type
    assert len(hash_hex) == (40 if expected_type == "sha1" else 32)


def test_decode_hash_rejects_empty_and_garbage():
    assert decode_hash("") == ("", "")
    assert decode_hash(None) == ("", "")
    assert decode_hash("!!!not a hash!!!") == ("", "")


def test_decode_hash_base62_round_trip():
    import base62

    raw = "f23456789abcdef0"   # 最高位非 0，避免前导零造成表示差异
    encoded = base62.encode(int(raw, 16), charset=base62.CHARSET_INVERTED)

    hash_hex, hash_type = decode_hash(encoded, uses_base62=True)

    assert hash_type == "md5"
    # base62 解出来的 16 字节补零到标准 MD5 长度
    assert hash_hex == raw.zfill(32)


# ---------------------------------------------------------------- 清单解析

def test_parse_export_text():
    text = "abc123#100#dir/a.txt$def456#200#b.txt"

    manifest = parse_export_text(text)

    assert manifest["usesBase62EtagsInExport"] is True
    assert manifest["files"] == [
        {"etag": "abc123", "size": "100", "path": "dir/a.txt"},
        {"etag": "def456", "size": "200", "path": "b.txt"},
    ]


def test_parse_export_text_ignores_malformed_lines():
    manifest = parse_export_text("only#two$a#1#b.txt")

    assert [f["path"] for f in manifest["files"]] == ["b.txt"]


def test_load_manifest_reads_json(tmp_path):
    path = tmp_path / "m.json"
    path.write_text(json.dumps({"commonPath": "x", "files": []}), encoding="utf-8")

    assert load_manifest(str(path))["commonPath"] == "x"


def test_load_manifest_falls_back_to_export_text(tmp_path):
    path = tmp_path / "m.txt"
    path.write_text("hash#1#a.txt", encoding="utf-8")

    manifest = load_manifest(str(path))

    assert manifest["files"][0]["path"] == "a.txt"


@pytest.mark.parametrize("raw,expected", [
    ("/a/b/", "a/b"),
    ("a//b", "a/b"),
    ("", ""),
    (None, ""),
])
def test_normalize_remote_path(raw, expected):
    assert normalize_remote_path(raw) == expected


# ---------------------------------------------------------------- 目录树

class StubFileService:
    """记录 mkdir 调用、按名字返回自增ID，并支持列出目录内容"""

    def __init__(self):
        self.mkdir_calls = []
        self.existing = {}   # (parent_id, name) -> id
        self.listings = {}   # dir_id -> [原始文件记录]
        self.list_calls = []

    def mkdir(self, name, parent_id):
        self.mkdir_calls.append((name, parent_id))
        key = (parent_id, name)
        if key not in self.existing:
            self.existing[key] = len(self.existing) + 1
        return self.existing[key]

    def list_files(self, parent_id=0, auto_fetch_all=False, use_cache=True, **kwargs):
        self.list_calls.append(parent_id)
        return FileList(self.listings.get(parent_id, [])), None


class StubClient:
    def __init__(self):
        self.file_service = StubFileService()


def test_remote_dir_tree_creates_each_level_once():
    client = StubClient()
    tree = RemoteDirTree(client, "a/b")

    first = tree.ensure("a/b/c")
    second = tree.ensure("a/b/c")
    sibling = tree.ensure("a/b/d")

    assert first == second
    assert client.file_service.mkdir_calls == [("a", 0), ("b", 1), ("c", 2), ("d", 2)]
    assert sibling != first


def test_remote_dir_tree_lookup_does_not_create():
    client = StubClient()
    tree = RemoteDirTree(client, "a")

    assert tree.lookup("a") is None

    tree.ensure("a")

    assert tree.lookup("a") == tree.root_id
    assert tree.lookup("a/missing") is None


def test_remote_dir_tree_root_id_is_zero_for_empty_path():
    tree = RemoteDirTree(StubClient(), "")

    assert tree.root_id == 0


def test_ensure_dirs_reports_failures_and_keeps_going():
    client = StubClient()
    original = client.file_service.mkdir

    def flaky_mkdir(name, parent_id):
        if name == "bad":
            raise Pan123APIError("boom")
        return original(name, parent_id)

    client.file_service.mkdir = flaky_mkdir
    tree = RemoteDirTree(client, "")
    tree.ensure("root")

    failures = tree.ensure_dirs(["root/bad", "root/good"])

    assert failures == [("root/bad", "boom")]
    assert tree.lookup("root/good") is not None


# ---------------------------------------------------------------- 秒传口径

class StubFileServiceForReuse:
    def __init__(self, sha1_result=None, create_result=None, error=None):
        self.sha1_result = sha1_result
        self.create_result = create_result
        self.error = error
        self.calls = []

    def try_sha1_reuse(self, **kwargs):
        self.calls.append(("sha1", kwargs))
        if self.error:
            raise self.error
        return self.sha1_result

    def create_file(self, **kwargs):
        self.calls.append(("md5", kwargs))
        if self.error:
            raise self.error
        return self.create_result or {}


class StubReuseClient:
    def __init__(self, **kwargs):
        self.file_service = StubFileServiceForReuse(**kwargs)


def test_reuse_file_reports_hit():
    client = StubReuseClient(sha1_result={"reuse": True, "fileID": 5})

    status, detail = reuse_file(client, "a.txt", 1, "a" * 40, 100)

    assert status == STATUS_HIT
    assert "5" in detail


def test_reuse_file_reports_miss_without_error():
    client = StubReuseClient(sha1_result=None)

    status, _ = reuse_file(client, "a.txt", 1, "a" * 40, 100)

    assert status == STATUS_MISS


def test_reuse_file_raises_when_call_fails():
    """调用失败必须抛出异常，不能被记成"未命中"。"""
    client = StubReuseClient(error=Pan123APIError("500"))

    with pytest.raises(Pan123APIError):
        reuse_file(client, "a.txt", 1, "a" * 40, 100)


def test_reuse_file_md5_existing_file_is_skip_not_hit():
    client = StubReuseClient(create_result={"skipped": True, "fileID": 9})

    status, detail = reuse_file(client, "a.txt", 1, "b" * 32, 100)

    assert status == STATUS_SKIP
    assert "已存在" in detail


def test_reuse_file_md5_reuse_is_hit():
    client = StubReuseClient(create_result={"reuse": True, "fileID": 9})

    status, _ = reuse_file(client, "a.txt", 1, "b" * 32, 100)

    assert status == STATUS_HIT


def test_reuse_file_skips_incomplete_records():
    client = StubReuseClient()

    assert reuse_file(client, "a.txt", 1, "", 100)[0] == STATUS_SKIP
    assert reuse_file(client, "a.txt", 1, "b" * 32, "not-a-number")[0] == STATUS_SKIP


# ---------------------------------------------------------------- 清单上传

MANIFEST = {
    "commonPath": "base",
    "files": [
        {"path": "a.txt", "size": 10, "etag": "a" * 40},
        {"path": "sub/b.txt", "size": 20, "etag": "b" * 32},
    ],
}

MD5_A = "a" * 32
MD5_B = "b" * 32
SHA1_C = "c" * 40


def write_manifest(tmp_path, payload=None, name="m.json"):
    path = tmp_path / name
    path.write_text(json.dumps(payload or MANIFEST), encoding="utf-8")
    return str(path)


def manifest_with(*files, common_path="base"):
    return {"commonPath": common_path, "files": list(files)}


def remote_dir_record(name, dir_id):
    return {"fileId": dir_id, "filename": name, "size": 0, "type": 1, "etag": ""}


def remote_file_record(name, size, etag):
    return {"fileId": 99, "filename": name, "size": size, "type": 0, "etag": etag}


def spy_reuse(monkeypatch, result=(STATUS_HIT, "ok")):
    """替换真正的秒传调用，记录调用参数"""
    calls = []

    def fake(client_, filename, parent_id, hash_hex, hash_type, size):
        calls.append((filename, parent_id, hash_hex, hash_type, size))
        return result

    monkeypatch.setattr(upload_core, "reuse_hashes", fake)
    return calls


def test_upload_manifest_counts_hits_and_skips(tmp_path, monkeypatch):
    client = StubClient()

    def fake(client_, filename, parent_id, hash_hex, hash_type, size):
        if filename == "a.txt":
            return STATUS_HIT, "ok"
        return STATUS_SKIP, "云端已存在同名同大小文件"

    monkeypatch.setattr(upload_core, "reuse_hashes", fake)

    stats = upload_manifest(client, write_manifest(tmp_path),
                            progress=False)

    assert (stats.hit, stats.skip) == (1, 1)
    assert stats.fail == 0
    # base 和 base/sub 两级目录都被创建
    assert client.file_service.mkdir_calls == [("base", 0), ("sub", 1)]


def test_upload_manifest_records_failure_without_stopping(tmp_path, monkeypatch):
    client = StubClient()
    calls = spy_reuse(monkeypatch)

    def fake(client_, filename, parent_id, hash_hex, hash_type, size):
        calls.append(filename)
        if filename == "a.txt":
            raise Pan123APIError("boom")
        return STATUS_HIT, "ok"

    monkeypatch.setattr(upload_core, "reuse_hashes", fake)

    stats = upload_manifest(client, write_manifest(tmp_path),
                            progress=False)

    assert stats.fail == 1
    assert stats.hit == 1
    assert stats.failures[0][1] == "boom"


def test_failed_files_get_one_serial_retry(tmp_path, monkeypatch):
    """第一次失败（比如被限流）不算最终失败，串行重试一轮。"""
    client = StubClient()
    attempts = []

    def flaky(client_, filename, parent_id, hash_hex, hash_type, size):
        attempts.append(filename)
        if len(attempts) == 1:
            raise Pan123APIError("请慢一点")
        return STATUS_HIT, "ok"

    monkeypatch.setattr(upload_core, "reuse_hashes", flaky)

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})),
        progress=False)

    assert attempts == ["a.txt", "a.txt"]
    assert stats.fail == 0
    assert stats.hit == 1
    assert stats.failures == []


def test_upload_manifest_requires_base_path(tmp_path):
    path = write_manifest(tmp_path, {"files": [{"path": "a.txt", "size": 1, "etag": SHA1_C}]})

    with pytest.raises(ValueError):
        upload_manifest(StubClient(), path, progress=False)


def test_upload_manifest_requires_existing_file(tmp_path):
    with pytest.raises(FileNotFoundError):
        upload_manifest(StubClient(), str(tmp_path / "nope.json"),
                        progress=False)


def test_upload_manifest_uses_shared_dir_tree(tmp_path, monkeypatch):
    """batch 场景下同一棵树只建一次。"""
    client = StubClient()
    tree = RemoteDirTree(client, "")
    spy_reuse(monkeypatch)

    upload_manifest(client, write_manifest(tmp_path), dir_tree=tree,
                    progress=False)
    upload_manifest(client, write_manifest(tmp_path, name="m2.json"),
                    dir_tree=tree, progress=False)

    assert client.file_service.mkdir_calls == [("base", 0), ("sub", 1)]


# ---------------------------------------------------------------- 判重

def test_dedup_skips_file_already_in_target_dir(tmp_path, monkeypatch):
    """目标目录里已有同名同大小且 MD5 一致的文件：一个秒传请求都不发，也不 mkdir。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_A)]
    calls = spy_reuse(monkeypatch)

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})),
        progress=False)

    assert calls == []
    assert client.file_service.mkdir_calls == []
    assert stats.hit == 1
    assert stats.deduped == 1
    assert stats.miss == 0
    assert stats.breakdown_line() == "合计 1 条：已有 1（MD5 一致 1）"


def test_dedup_weak_match_skips_sha1_manifest(tmp_path, monkeypatch):
    """清单只有 SHA1、没法比 MD5：默认按同名同大小跳过。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_B)]
    calls = spy_reuse(monkeypatch)

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": SHA1_C})),
        progress=False)

    assert calls == []
    assert stats.skip == 1
    assert stats.existed == 1
    assert stats.breakdown_line() == "合计 1 条：已有 1（同名同大小 1）"


def test_verify_sha1_forces_request(tmp_path, monkeypatch):
    """--verify 时不再按同名同大小跳过，而是发请求确认。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_B)]
    calls = spy_reuse(monkeypatch, result=(STATUS_MISS, "SHA1秒传未命中"))

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": SHA1_C})),
        progress=False, verify_sha1=True)

    assert len(calls) == 1
    assert stats.miss == 1


def test_dedup_reports_conflict_when_md5_differs(tmp_path, monkeypatch):
    """同名同大小但 MD5 不同：不静默跳过，也不造重复文件，单独报出来。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_B)]
    calls = spy_reuse(monkeypatch)

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})),
        progress=False)

    assert calls == []
    assert stats.skip == 1
    assert len(stats.conflicts) == 1
    assert "MD5 不同" in stats.conflicts[0][1]


def test_no_dedup_uses_mkdir_and_always_requests(tmp_path, monkeypatch):
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_A)]
    calls = spy_reuse(monkeypatch)

    stats = upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})),
        progress=False, dedup=False)

    assert len(calls) == 1
    assert client.file_service.mkdir_calls == [("base", 0)]
    assert stats.hit == 1


def test_dir_tree_takes_existing_dir_from_index():
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    index = RemoteIndex(client)
    tree = RemoteDirTree(client, "", index)

    assert tree.ensure("base") == 7
    assert client.file_service.mkdir_calls == []


def test_created_dir_is_remembered_in_parent_snapshot():
    """本次运行里新建的目录会补进父目录快照：后面再查它不用重新列目录。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = []
    index = RemoteIndex(client)
    tree = RemoteDirTree(client, "", index)

    child = tree.ensure("base/sub")   # base 从快照里找到，sub 需要 mkdir

    assert client.file_service.mkdir_calls == [("sub", 7)]
    client.file_service.list_calls.clear()
    assert index.find_dir(7, "sub") == child
    assert client.file_service.list_calls == []


# ---------------------------------------------------------------- 重跑

def test_rerun_dedups_against_cloud_listing(tmp_path, monkeypatch):
    """没有本地记录后重跑同一个清单：按目录快照判重，仍然零秒传请求。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_A)]
    calls = spy_reuse(monkeypatch)
    path = write_manifest(tmp_path, manifest_with(
        {"path": "a.txt", "size": 10, "etag": MD5_A}))

    first = upload_manifest(client, path, progress=False)
    client.file_service.list_calls.clear()

    second = upload_manifest(client, path, progress=False)

    assert calls == []              # 两轮都是判重命中，没发过秒传请求
    assert first.hit == 1 and first.deduped == 1
    assert second.hit == 1 and second.deduped == 1


def test_snapshot_loading_reports_progress(tmp_path, monkeypatch, capsys):
    """判重阶段要有反馈：第一行是阶段与当前目录，第二行是整体进度。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("base", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 10, MD5_A)]
    spy_reuse(monkeypatch)

    upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})))

    err = capsys.readouterr().err
    assert "📂 读取目录快照 /base" in err, "第一行要有当前目录"
    assert "判重 1/1" in err, "第一行要有判重进度"
    assert "1/1 (100%)" in err, "第二行要是整体进度"
    assert "→ 1 条记录已在云端" in err


def test_progress_can_be_silenced(tmp_path, monkeypatch, capsys):
    client = StubClient()
    spy_reuse(monkeypatch)

    upload_manifest(client, write_manifest(
        tmp_path, manifest_with({"path": "a.txt", "size": 10, "etag": MD5_A})),
        progress=False)

    assert "读取目录快照" not in capsys.readouterr().err


def test_rate_limiter_is_installed_once():
    class FakeHttp:
        def __init__(self):
            self.rate_limiter = None
            self.installed = []

        def set_rate_limiter(self, limiter):
            self.rate_limiter = limiter
            self.installed.append(limiter)

    client = StubClient()
    client.http_client = FakeHttp()

    upload_core._configure_rate(client, 5)
    upload_core._configure_rate(client, 5)

    assert len(client.http_client.installed) == 1
    assert client.http_client.rate_limiter.rate == 5


# ---------------------------------------------------------------- 目录上传

class StubUploadFileService(StubFileService):
    """在目录树替身的基础上补一个 upload_file"""

    def __init__(self, results=None, error=None):
        super().__init__()
        self.upload_results = results or {}
        self.error = error
        self.uploaded = []
        self.calls = []   # (filename, skip_if_exists, try_sha1_reuse)

    def upload_file(self, local_path, parent_id, filename, skip_if_exists, try_sha1_reuse):
        if self.error:
            raise self.error
        self.uploaded.append((filename, parent_id))
        self.calls.append((filename, skip_if_exists, try_sha1_reuse))
        return self.upload_results.get(filename, {"fileID": 1, "method": "sha1_reuse"})


class StubUploadClient:
    def __init__(self, results=None, error=None):
        self.file_service = StubUploadFileService(results, error)


def test_upload_directory_dry_run_makes_no_api_calls(tmp_path):
    (tmp_path / "sub").mkdir()
    (tmp_path / "sub" / "a.txt").write_text("x", encoding="utf-8")
    client = StubUploadClient()

    stats = upload_directory(client, str(tmp_path), "remote", dry_run=True, progress=False)

    assert client.file_service.mkdir_calls == []
    assert client.file_service.uploaded == []
    assert stats.total == 0


def test_upload_directory_uploads_and_counts(tmp_path):
    (tmp_path / "sub").mkdir()
    (tmp_path / "a.txt").write_text("x", encoding="utf-8")
    (tmp_path / "sub" / "b.txt").write_text("y", encoding="utf-8")
    client = StubUploadClient(results={
        "a.txt": {"fileID": 1, "reuse": True, "method": "sha1_reuse"},
        "b.txt": {"fileID": 2, "skipped": True},
    })

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert stats.hit == 1
    assert stats.reuse == 1
    assert stats.skip == 1
    assert stats.fail == 0
    assert sorted(name for name, _ in client.file_service.uploaded) == ["a.txt", "b.txt"]
    # remote 根目录 + sub 子目录
    assert ("remote", 0) in client.file_service.mkdir_calls
    assert ("sub", 1) in client.file_service.mkdir_calls


@pytest.mark.parametrize("remote_path,expected", [
    ("", "/sub/a.txt"),
    ("remote", "/remote/sub/a.txt"),
])
def test_dry_run_prints_normalized_paths(tmp_path, monkeypatch, remote_path, expected):
    """dry-run 里的目标路径不能出现 // 之类的怪样子。"""
    (tmp_path / "sub").mkdir()
    (tmp_path / "sub" / "a.txt").write_text("x", encoding="utf-8")
    lines = []
    monkeypatch.setattr(upload_core, "emit", lines.append)

    upload_directory(StubUploadClient(), str(tmp_path), remote_path,
                     dry_run=True, progress=False)

    assert any(str(line).endswith(expected) for line in lines), lines


def test_upload_directory_records_exceptions(tmp_path):
    (tmp_path / "a.txt").write_text("x", encoding="utf-8")
    client = StubUploadClient(error=Pan123APIError("disk full"))

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert stats.fail == 1
    assert "disk full" in stats.failures[0][1]


def test_upload_directory_reports_dir_failure_and_continues(tmp_path):
    (tmp_path / "bad").mkdir()
    (tmp_path / "bad" / "a.txt").write_text("x", encoding="utf-8")
    (tmp_path / "ok.txt").write_text("y", encoding="utf-8")
    client = StubUploadClient()

    def flaky_mkdir(name, parent_id):
        if name == "bad":
            raise Pan123APIError("no permission")
        return 1

    client.file_service.mkdir = flaky_mkdir

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert stats.fail == 1
    assert stats.hit == 1


def test_upload_directory_skips_existing_via_snapshot(tmp_path):
    """快照判重：目标目录已列出的同名同大小文件零请求跳过，不再调 upload_file。"""
    (tmp_path / "a.txt").write_text("hello", encoding="utf-8")
    (tmp_path / "b.txt").write_text("world!", encoding="utf-8")
    client = StubUploadClient()
    client.file_service.listings[0] = [remote_dir_record("remote", 7)]
    client.file_service.listings[7] = [
        remote_file_record("a.txt", 5, MD5_A),
        remote_file_record("b.txt", 6, MD5_B),
    ]

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert client.file_service.uploaded == []
    assert client.file_service.mkdir_calls == []   # 目录从快照取 ID，也不 mkdir
    assert stats.skip == 2
    assert stats.existed == 2
    assert stats.total == 2


def test_upload_directory_uploads_same_name_different_size(tmp_path):
    """同名但大小不同的文件不走快照跳过，照常上传。"""
    (tmp_path / "a.txt").write_text("changed content", encoding="utf-8")
    client = StubUploadClient()
    client.file_service.listings[0] = [remote_dir_record("remote", 7)]
    client.file_service.listings[7] = [remote_file_record("a.txt", 5, MD5_A)]

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert [name for name, _ in client.file_service.uploaded] == ["a.txt"]
    # 判重已在快照阶段做过，upload_file 不再为每个文件列目录
    assert client.file_service.calls[0] == ("a.txt", False, True)
    assert stats.hit == 1


def test_upload_directory_retries_failed_uploads(tmp_path):
    """第一轮全部失败、重试成功：最终计入 hit，而不是 fail。"""
    (tmp_path / "a.txt").write_text("x", encoding="utf-8")
    client = StubUploadClient()
    attempts = []

    def flaky_upload(**kwargs):
        attempts.append(kwargs["filename"])
        if len(attempts) == 1:
            raise Pan123APIError("slow down")
        return {"fileID": 1, "reuse": True, "method": "sha1_reuse"}

    client.file_service.upload_file = flaky_upload

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert attempts == ["a.txt", "a.txt"]
    assert stats.fail == 0
    assert stats.hit == 1
    assert stats.reuse == 1


def test_upload_directory_dedup_counts_only_once_on_persistent_failure(tmp_path):
    """持续失败的文件只记一次 fail（重试不重复计数），原因取第一轮的错误。"""
    (tmp_path / "a.txt").write_text("x", encoding="utf-8")
    client = StubUploadClient(error=Pan123APIError("disk full"))

    stats = upload_directory(client, str(tmp_path), "remote", progress=False)

    assert stats.fail == 1
    assert "disk full" in stats.failures[0][1]
