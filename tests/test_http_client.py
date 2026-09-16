"""RequestHandler 的重试与限速测试（用假 session，不发真实请求）"""

import pytest
import requests

from api.exceptions import NetworkError, Pan123APIError
from api.http_client import RequestHandler


class FakeResponse:
    def __init__(self, payload, status_code=200):
        self.payload = payload
        self.status_code = status_code
        self.content = b'{}'
        self.text = '{}'
        self.headers = {}

    def json(self):
        if isinstance(self.payload, Exception):
            raise self.payload
        return self.payload

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.exceptions.HTTPError(
                f"{self.status_code}", response=self)


class FakeSession:
    def __init__(self, responses):
        self.headers = {}
        self.responses = list(responses)
        self.calls = []

    def request(self, method, url, **kwargs):
        self.calls.append((method, url))
        item = self.responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return item

    def close(self):
        pass


class FakeTokenManager:
    access_token = "token"


class FakeLimiter:
    def __init__(self):
        self.acquires = 0

    def acquire(self, tokens=1.0):
        self.acquires += 1
        return 0.0


def make_handler(responses, **kwargs):
    kwargs.setdefault('retry_delay', 0)
    handler = RequestHandler("https://api.example.com", FakeTokenManager(), **kwargs)
    handler.session = FakeSession(responses)
    return handler


def test_business_code_1_is_retried():
    """业务码 1（"请慢一点"）是账号级限流，必须重试而不是直接失败。"""
    handler = make_handler([
        FakeResponse({"code": 1, "message": "请慢一点"}),
        FakeResponse({"code": 1, "message": "请慢一点"}),
        FakeResponse({"code": 0, "data": {"fileID": 9}}),
    ])

    result = handler.post("/upload/v2/file/sha1_reuse", json_data={})

    assert result["data"]["fileID"] == 9
    assert len(handler.session.calls) == 3


def test_rate_limit_code_429_is_retried():
    handler = make_handler([
        FakeResponse({"code": 429, "message": "全站请求过于频繁"}),
        FakeResponse({"code": 0, "data": {}}),
    ])

    handler.get("/api/v2/file/list")

    assert len(handler.session.calls) == 2


def test_retry_gives_up_and_raises():
    handler = make_handler([FakeResponse({"code": 1, "message": "请慢一点"})] * 5,
                           max_retries=2)

    with pytest.raises(Pan123APIError) as excinfo:
        handler.get("/api/v2/file/list")

    assert excinfo.value.error_code == 1
    assert len(handler.session.calls) == 3, "1 次原始请求 + 2 次重试"


def test_non_retryable_business_code_fails_immediately():
    handler = make_handler([FakeResponse({"code": 401, "message": "token 超限"})])

    with pytest.raises(Pan123APIError) as excinfo:
        handler.get("/api/v1/user/info")

    assert excinfo.value.error_code == 401
    assert len(handler.session.calls) == 1


def test_server_error_is_retried_then_raises():
    handler = make_handler([FakeResponse({}, status_code=502)] * 3, max_retries=2)

    with pytest.raises(Pan123APIError):
        handler.get("/api/v2/file/list")

    assert len(handler.session.calls) == 3


def test_network_error_becomes_network_error_exception():
    handler = make_handler([requests.exceptions.ConnectionError("boom")] * 3,
                           max_retries=2)

    with pytest.raises(NetworkError):
        handler.get("/api/v2/file/list")


def test_rate_limiter_applies_to_every_attempt():
    limiter = FakeLimiter()
    handler = make_handler([
        FakeResponse({"code": 1, "message": "请慢一点"}),
        FakeResponse({"code": 0, "data": {}}),
    ], rate_limiter=limiter)

    handler.get("/api/v2/file/list")

    assert limiter.acquires == 2, "每次尝试（含重试）都要过限速器"


def test_rate_limiter_can_be_swapped():
    handler = make_handler([FakeResponse({"code": 0, "data": {}})])
    limiter = FakeLimiter()

    handler.set_rate_limiter(limiter)
    handler.get("/api/v2/file/list")

    assert limiter.acquires == 1


def test_empty_body_returns_empty_dict():
    """空响应体（200 且没有内容）视为成功但无数据，不抛异常。"""
    response = FakeResponse(ValueError("no json"))
    response.content = b""
    handler = make_handler([response])

    assert handler.get("/upload/v2/file/upload_complete") == {}


def test_non_json_body_raises():
    handler = make_handler([FakeResponse(ValueError("no json"))])

    with pytest.raises(Pan123APIError):
        handler.get("/api/v2/file/list")
