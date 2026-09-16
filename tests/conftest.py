"""共享测试夹具。

这里用最小的假客户端替代真实 HTTP 层，测试不依赖网络、Redis 或 config.json。
"""

import pytest

from api.exceptions import Pan123APIError
from api.file_service import FileService


class FakeHttpClient:
    """RequestHandler 的替身：按 endpoint 返回预设响应，并记录所有调用。"""

    def __init__(self):
        self.get_responses = {}   # endpoint -> 响应列表（按顺序弹出）
        self.post_responses = {}
        self.calls = []

    def queue_get(self, endpoint: str, *responses):
        self.get_responses.setdefault(endpoint, []).extend(responses)
        return self

    def queue_post(self, endpoint: str, *responses):
        self.post_responses.setdefault(endpoint, []).extend(responses)
        return self

    def _next(self, store, endpoint, method):
        queue = store.get(endpoint)
        if not queue:
            raise AssertionError(f"未预设 {method} {endpoint} 的响应")
        response = queue.pop(0)
        if isinstance(response, Exception):
            raise response
        return response

    def get(self, endpoint, params=None):
        self.calls.append(("GET", endpoint, params))
        return self._next(self.get_responses, endpoint, "GET")

    def post(self, endpoint, json_data=None, data=None, files=None):
        self.calls.append(("POST", endpoint, json_data))
        return self._next(self.post_responses, endpoint, "POST")

    def count_calls(self, endpoint: str = None) -> int:
        if endpoint is None:
            return len(self.calls)
        return sum(1 for _, ep, _ in self.calls if ep == endpoint)


def make_page(file_list, last_file_id):
    """构造 /api/v2/file/list 的响应体"""
    return {"code": 0, "data": {"fileList": file_list, "lastFileId": last_file_id}}


def make_file(file_id, filename, size=100, trashed=0, type_=0, parent_id=0):
    """构造一条文件记录"""
    return {
        "fileId": file_id,
        "filename": filename,
        "size": size,
        "type": type_,
        "trashed": trashed,
        "parentFileId": parent_id,
        "etag": "abc",
    }


@pytest.fixture
def http_client():
    return FakeHttpClient()


@pytest.fixture
def service(http_client):
    return FileService(http_client, cache_manager=None, config={})


__all__ = [
    "FakeHttpClient",
    "Pan123APIError",
    "make_page",
    "make_file",
    "service",
    "http_client",
]
