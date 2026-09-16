"""File / FileList 数据模型测试"""

from api.models import File, FileList


def raw_file(**overrides):
    """API 返回的原始记录"""
    data = {
        "fileId": 1,
        "filename": "a.txt",
        "size": 2048,
        "type": 0,
        "category": 0,
        "parentFileId": 0,
        "etag": "abc",
        "updateAt": "2026-01-01 00:00:00",
    }
    data.update(overrides)
    return data


def make_file(**overrides):
    return File(raw_file(**overrides))


def test_basic_attributes_and_formatting():
    f = make_file(size=1024 * 1024 * 3)

    assert f.file_id == 1
    assert f.is_folder is False
    assert f.size_formatted == "3.00 MB"
    assert f.file_extension == ".txt"


def test_folder_detection_and_icon():
    folder = make_file(type=1, filename="docs")

    assert folder.is_folder is True
    assert folder.icon == "fas fa-folder"


def test_to_dict_keeps_raw_fields_and_adds_derived():
    f = make_file()

    result = f.to_dict()

    assert result["fileId"] == 1
    assert result["size_formatted"] == "2.00 KB"
    assert result["is_folder"] is False


def test_setitem_updates_attributes_not_only_raw_data():
    """f['size'] = x 之后 f.size 必须同步，曾经只写了原始字典。"""
    f = make_file(size=100)

    f["size"] = 2048

    assert f.size == 2048
    assert f.size_formatted == "2.00 KB"
    assert f.to_dict()["size"] == 2048


def test_setitem_maps_camel_case_keys():
    f = make_file()

    f["filename"] = "renamed.txt"
    f["parentFileId"] = 42

    assert f.filename == "renamed.txt"
    assert f.parent_file_id == 42
    assert f.file_extension == ".txt"


def test_setitem_does_not_clobber_methods():
    f = make_file()

    f["get"] = "boom"

    assert callable(f.get)
    assert f.get("filename") == "a.txt"


def test_getitem_and_contains():
    f = make_file()

    assert f["filename"] == "a.txt"
    assert "filename" in f
    assert "not_a_field" not in f
    assert f.get("not_a_field", "fallback") == "fallback"


def test_file_list_iteration_and_lookup():
    file_list = FileList([
        raw_file(fileId=1, filename="a.txt"),
        raw_file(fileId=2, filename="folder", type=1),
    ])

    assert len(file_list) == 2
    assert file_list.find_by_name("a.txt").file_id == 1
    assert file_list.find_by_name("missing") is None
    assert len(file_list.filter_by_type(is_folder=True)) == 1
    assert [f["fileId"] for f in file_list.to_dict_list()] == [1, 2]
