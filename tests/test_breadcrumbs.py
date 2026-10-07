from api.models import File
from routes.utils import folder_breadcrumbs
from flask import Flask


class StubClient:
    def __init__(self, files):
        self.files = files
        self.calls = []

    def get_file_info_single(self, file_id, use_cache=True):
        self.calls.append((file_id, use_cache))
        return self.files.get(file_id)


def test_folder_breadcrumbs_returns_root_to_leaf_and_respects_refresh():
    client = StubClient(
        {
            10: File({"fileId": 10, "filename": "第一层", "parentFileId": 0}),
            20: File({"fileId": 20, "filename": "第二层", "parentFileId": 10}),
        }
    )

    with Flask(__name__).test_request_context("/"):
        assert folder_breadcrumbs(client, 20, refresh=True) == [
            {"name": "第一层", "file_id": 10},
            {"name": "第二层", "file_id": 20},
        ]

    assert client.calls == [(20, False), (10, False)]


def test_folder_breadcrumbs_stops_cycles_and_returns_available_path():
    client = StubClient(
        {
            1: File({"fileId": 1, "filename": "甲", "parentFileId": 2}),
            2: File({"fileId": 2, "filename": "乙", "parentFileId": 1}),
        }
    )

    with Flask(__name__).test_request_context("/"):
        result = folder_breadcrumbs(client, 1)

    assert result == [
        {"name": "乙", "file_id": 2},
        {"name": "甲", "file_id": 1},
    ]
    assert len(client.calls) == 2
