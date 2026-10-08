"""RequestHandler 的重试与限速测试（用假 session，不发真实请求）"""

import pytest
import requests

from api.exceptions import NetworkError, Pan123APIError
from api.http_client import RequestHandler


class FakeResponse:
    def __init__(self, payload, status_code=200, headers=None):
        self.payload = payload
        self.status_code = status_code
        self.content = b'{}'
        self.text = '{}'
        self.headers = headers or {}

    def json(self):
        if isinstance(self.payload, Exception):
            raise self.payload
        return self.payload

    def __bool__(self):
        return self.status_code < 400

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.exceptions.HTTPError(
                f"{self.status_code}", response=self)


class FakeSession:
    def __init__(self, responses):
        self.headers = {}
        self.responses = list(responses)
        self.calls = []
        self.request_kwargs = []

    def request(self, method, url, **kwargs):
        self.calls.append((method, url))
        self.request_kwargs.append(kwargs)
        item = self.responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return item

    def close(self):
        pass


class FakeTokenManager:
    def __init__(self):
        self.access_token = "token"

    def refresh_if_current(self, rejected_token):
        assert rejected_token == "token"
        self.access_token = "fresh"


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
    handler = make_handler([FakeResponse({"code": 401, "message": "token 超限"})] * 2)

    with pytest.raises(Pan123APIError) as excinfo:
        handler.get("/api/v1/user/info")

    assert excinfo.value.error_code == 401
    assert len(handler.session.calls) == 2


def test_body_401_refreshes_once_and_retries_with_new_header():
    handler = make_handler([
        FakeResponse({"code": 401, "message": "token expired"}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    handler.get("/api/v1/user/info")
    assert len(handler.session.calls) == 2
    assert handler.session.request_kwargs[0]["headers"]["Authorization"] == "Bearer token"
    assert handler.session.request_kwargs[1]["headers"]["Authorization"] == "Bearer fresh"
    assert "Authorization" not in handler.session.headers


def test_http_429_honors_retry_after_seconds(monkeypatch):
    delays = []
    monkeypatch.setattr("api.http_client.time.sleep", delays.append)
    handler = make_handler([
        FakeResponse({}, status_code=429, headers={"Retry-After": "3"}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    handler.get("/api/v1/user/info")
    assert delays == [3.0]


def test_body_429_honors_retry_after(monkeypatch):
    delays = []
    monkeypatch.setattr("api.http_client.time.sleep", delays.append)
    handler = make_handler([
        FakeResponse({"code": 429, "message": "slow down"}, headers={"Retry-After": "2"}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    handler.get("/api/v1/user/info")
    assert delays == [2.0]


def test_body_429_accepts_http_date_retry_after(monkeypatch):
    delays = []
    monkeypatch.setattr("api.http_client.time.sleep", delays.append)
    handler = make_handler([
        FakeResponse({"code": 429}, headers={
            "Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    handler.get("/api/v1/user/info")
    assert delays == [0.0]


def test_http_429_honors_http_date_retry_after(monkeypatch):
    delays = []
    monkeypatch.setattr("api.http_client.time.sleep", delays.append)
    handler = make_handler([
        FakeResponse({}, status_code=429,
                     headers={"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    handler.get("/api/v1/user/info")
    assert delays == [0.0]


@pytest.mark.parametrize("retry_after", ["nan", "inf", "999999999"])
def test_unusable_retry_after_fails_without_sleep(monkeypatch, retry_after):
    delays = []
    monkeypatch.setattr("api.http_client.time.sleep", delays.append)
    handler = make_handler([
        FakeResponse({}, status_code=429, headers={"Retry-After": retry_after}),
        FakeResponse({"code": 0, "data": {}}),
    ])
    with pytest.raises(Pan123APIError, match="Retry-After|超过本地上限"):
        handler.get("/api/v1/user/info")
    assert delays == []


def test_http_401_refreshes_once_then_raises_with_status():
    handler = make_handler([
        FakeResponse({"message": "expired"}, status_code=401),
        FakeResponse({"message": "still expired"}, status_code=401),
        FakeResponse({"code": 0}),
    ])
    with pytest.raises(Pan123APIError) as excinfo:
        handler.get("/api/v1/user/info")
    assert excinfo.value.status_code == 401
    assert len(handler.session.calls) == 2


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
