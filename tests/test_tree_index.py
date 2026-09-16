"""目录树导出索引（ExportTreeIndex）与 --tree 判重流程的测试。"""

import json

from api.exceptions import Pan123APIError
from api.models import FileList
import upload_core
from upload_core import (
    STATUS_HIT,
    ExportTreeIndex,
    load_tree_index,
    upload_manifest,
)

MD5_A = "a" * 32
MD5_B = "b" * 32

TREE_TEXT = "\n".join([
    "我的文件",
    "└─fffff",
    "    ├─movies",
    "    │     a.mp4",
    "    │     b(1).mkv",
    "    │   └─sub",
    "    │         c.mp4",
    "    └─docs",
    "          note.txt",
]) + "\n"


# ---------------------------------------------------------------- 替身

class StubFileService:
    def __init__(self):
        self.mkdir_calls = []
        self.existing = {}
        self.listings = {}   # dir_id -> [原始记录]
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


def remote_dir_record(name, dir_id):
    return {"fileId": dir_id, "filename": name, "size": 0, "type": 1, "etag": ""}


def remote_file_record(name, size, etag):
    return {"fileId": 99, "filename": name, "size": size, "type": 0, "etag": etag}


def write_manifest(tmp_path, files, common_path="fffff", name="m.json"):
    path = tmp_path / name
    path.write_text(json.dumps(
        {"commonPath": common_path, "files": files}), encoding="utf-8")
    return str(path)


def spy_reuse(monkeypatch, result=(STATUS_HIT, "ok")):
    calls = []

    def fake(client_, filename, parent_id, hash_hex, hash_type, size):
        calls.append(filename)
        return result

    monkeypatch.setattr(upload_core, "reuse_hashes", fake)
    return calls


# ---------------------------------------------------------------- 索引解析

def test_tree_text_parse_and_lookup():
    index = ExportTreeIndex.from_tree_text(TREE_TEXT)

    assert index.file_count == 4
    assert index.dir_count == 4          # fffff / movies / sub / docs（不含根）
    assert index.find_file("fffff/movies", "a.mp4") == {'size': None, 'etag': ''}
    assert index.find_file("fffff/movies/sub", "c.mp4") is not None
    assert index.find_file("fffff/movies", "nope.mp4") is None
    # 子树里的新路径也算"归目录树管"（目录不存在 = 需要新建，不是未知）
    assert index.covers("fffff/新目录/更深")
    # 导出之外的路径不归它管，判重结果作不得数
    assert not index.covers("图片/xx")
    assert not index.covers("fffff2")


def test_tree_root_can_be_overridden():
    """--tree-root 只改导出根对应的远程路径，树里的真实层级（fffff）保留。"""
    index = ExportTreeIndex.from_tree_text(TREE_TEXT, tree_root="backup")

    assert index.find_file("backup/fffff/movies", "a.mp4") is not None
    assert not index.covers("fffff/movies")


def test_tree_root_folder_name_defaults_to_itself():
    """首行不是"我的文件"时，把它当真实目录名。"""
    text = TREE_TEXT.replace("我的文件", "备份盘", 1)
    index = ExportTreeIndex.from_tree_text(text)

    assert index.find_file("备份盘/fffff/movies", "a.mp4") is not None


def test_manifest_export_keeps_size_and_md5():
    data = {"commonPath": "fffff", "files": [
        {"path": "movies/a.mp4", "size": 100, "etag": MD5_A}]}

    index = ExportTreeIndex.from_manifest(data)

    assert index.find_file("fffff/movies", "a.mp4") == {'size': 100, 'etag': MD5_A}
    assert index.covers("fffff/movies")


def test_load_tree_index_detects_formats(tmp_path):
    tree_path = tmp_path / "tree.txt"
    tree_path.write_text(TREE_TEXT, encoding="utf-8")
    assert load_tree_index(str(tree_path)).find_file("fffff/movies", "a.mp4") is not None

    rich_path = tmp_path / "rich.txt"
    rich_path.write_text("f23456789abcdef0#100#movies/a.mp4", encoding="utf-8")
    entry = load_tree_index(str(rich_path)).find_file("movies", "a.mp4")
    assert entry is not None and entry['size'] == '100' and entry['etag']

    json_path = tmp_path / "m.json"
    json_path.write_text(json.dumps({"commonPath": "x", "files": [
        {"path": "a.txt", "size": 5, "etag": MD5_B}]}), encoding="utf-8")
    assert load_tree_index(str(json_path)).find_file("x", "a.txt") == \
        {'size': 5, 'etag': MD5_B}


# ---------------------------------------------------------------- 判重流程

def test_tree_hit_skips_everything(tmp_path, monkeypatch):
    """目录树里已有：零列表请求、零 mkdir、零秒传。"""
    client = StubClient()
    calls = spy_reuse(monkeypatch)
    manifest = write_manifest(tmp_path, [
        {"path": "movies/a.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest, progress=False,
                            tree_index=ExportTreeIndex.from_tree_text(TREE_TEXT))

    assert (stats.hit, stats.tree) == (1, 1)
    assert calls == []
    assert client.file_service.list_calls == []
    assert client.file_service.mkdir_calls == []
    assert stats.breakdown_line() == "合计 1 条：已有 1（目录树 1）"


def test_tree_miss_prepares_dir_and_uploads(tmp_path, monkeypatch):
    """目录树里没有：只给这一条的目标目录做解析，然后正常秒传。"""
    client = StubClient()
    calls = spy_reuse(monkeypatch)
    manifest = write_manifest(tmp_path, [
        {"path": "movies/new.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest,
                            progress=False,
                            tree_index=ExportTreeIndex.from_tree_text(TREE_TEXT))

    assert calls == ["new.mp4"]
    assert (stats.hit, stats.reuse) == (1, 1)
    assert client.file_service.mkdir_calls == [("fffff", 0), ("movies", 1)]


def test_tree_verify_lists_directory_and_dedups(tmp_path, monkeypatch):
    """--tree-verify：名字命中后仍列目录核实，MD5 一致按经典判重记账。"""
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("fffff", 7)]
    client.file_service.listings[7] = [remote_dir_record("movies", 8)]
    client.file_service.listings[8] = [remote_file_record("a.mp4", 10, MD5_A)]
    calls = spy_reuse(monkeypatch)
    manifest = write_manifest(tmp_path, [
        {"path": "movies/a.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest, progress=False,
                            tree_index=ExportTreeIndex.from_tree_text(TREE_TEXT),
                            tree_verify=True)

    assert calls == []
    assert (stats.hit, stats.deduped, stats.tree) == (1, 1, 0)
    assert 8 in client.file_service.list_calls, "核实要列目标目录"
    assert client.file_service.mkdir_calls == []


def test_tree_verify_reports_md5_conflict(tmp_path, monkeypatch):
    client = StubClient()
    client.file_service.listings[0] = [remote_dir_record("fffff", 7)]
    client.file_service.listings[7] = [remote_dir_record("movies", 8)]
    client.file_service.listings[8] = [remote_file_record("a.mp4", 10, MD5_B)]
    calls = spy_reuse(monkeypatch)
    manifest = write_manifest(tmp_path, [
        {"path": "movies/a.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest,
                            progress=False,
                            tree_index=ExportTreeIndex.from_tree_text(TREE_TEXT),
                            tree_verify=True)

    assert calls == []
    assert stats.skip == 1 and len(stats.conflicts) == 1


def test_rich_tree_size_mismatch_still_uploads(tmp_path, monkeypatch):
    """带大小的导出：同名但大小不同不算已存在，照样走秒传。"""
    client = StubClient()
    calls = spy_reuse(monkeypatch)
    rich = ExportTreeIndex.from_manifest({"commonPath": "fffff", "files": [
        {"path": "movies/a.mp4", "size": 999, "etag": MD5_A}]})
    manifest = write_manifest(tmp_path, [
        {"path": "movies/a.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest,
                            progress=False,
                            tree_index=rich)

    assert calls == ["a.mp4"]
    assert stats.tree == 0 and stats.hit == 1


def test_tree_flow_survives_dir_failure(tmp_path, monkeypatch):
    """判重后建目录失败：条目记成跳过，进度不丢。"""
    client = StubClient()
    original = client.file_service.mkdir

    def flaky(name, parent_id):
        if name == "newdir":
            raise Pan123APIError("boom")
        return original(name, parent_id)

    client.file_service.mkdir = flaky
    calls = spy_reuse(monkeypatch)
    manifest = write_manifest(tmp_path, [
        {"path": "newdir/a.mp4", "size": 10, "etag": MD5_A}])

    stats = upload_manifest(client, manifest,
                            progress=False,
                            tree_index=ExportTreeIndex.from_tree_text(TREE_TEXT))

    assert stats.skip == 1
    assert calls == []


def test_tree_index_shared_across_manifests(tmp_path, monkeypatch):
    """batch 场景：目录树与目录树缓存跨清单共用，命中清单零请求零 mkdir。"""
    client = StubClient()
    calls = spy_reuse(monkeypatch)
    index = upload_core.RemoteIndex(client)
    dir_tree = upload_core.RemoteDirTree(client, '', index)
    tree = ExportTreeIndex.from_tree_text(TREE_TEXT)
    m1 = write_manifest(tmp_path, [
        {"path": "movies/new.mp4", "size": 10, "etag": MD5_A}], name="m1.json")
    m2 = write_manifest(tmp_path, [
        {"path": "movies/a.mp4", "size": 10, "etag": MD5_A}], name="m2.json")

    stats1 = upload_manifest(client, m1, dir_tree=dir_tree, index=index,
                             progress=False, tree_index=tree)
    stats2 = upload_manifest(client, m2, dir_tree=dir_tree, index=index,
                             progress=False, tree_index=tree)

    assert calls == ["new.mp4"]          # 只有目录树里没有的那条真的上传
    assert (stats1.hit, stats1.tree) == (1, 0)
    assert (stats2.hit, stats2.tree) == (1, 1)
    # 第二个清单全命中：目录建一次就够，没有为它再列目录
    assert client.file_service.mkdir_calls == [("fffff", 0), ("movies", 1)]
    listed_after_m1 = list(client.file_service.list_calls)
    assert client.file_service.list_calls == listed_after_m1
