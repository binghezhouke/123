"""
HTTP请求处理器（带集中重试逻辑）
"""
import random
import requests
import time
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import math
from typing import Dict, Any, Optional, Set
from .exceptions import Pan123APIError, NetworkError


class RequestHandler:
    """HTTP请求处理器，网络层统一负责重试逻辑"""

    PLATFORM_HEADER = "open_platform"
    # 业务码 1 是"请慢一点"（账号级限流），429 是"全站请求过于频繁"，
    # 20103 是"文件校验中"。这三种都是可重试的临时状态。
    DEFAULT_RETRY_API_CODES = {1, 429, 20103}
    MAX_BACKOFF_SECONDS = 60.0
    MAX_RETRY_AFTER_SECONDS = 300.0

    def __init__(self, base_url: str, token_manager, *, max_retries: int = 5, retry_delay: float = 0.5, backoff_factor: float = 2.0, retry_api_codes: Optional[Set[int]] = None, rate_limiter=None):
        self.base_url = base_url
        self.token_manager = token_manager
        self.session = requests.Session()
        self.session.headers.update({"Platform": self.PLATFORM_HEADER})

        # retry 配置
        self.max_retries = max_retries
        self.retry_delay = retry_delay
        self.backoff_factor = backoff_factor
        self.retry_api_codes = set(
            retry_api_codes) if retry_api_codes is not None else set(self.DEFAULT_RETRY_API_CODES)
        # 可选限速器（见 api/ratelimit.py），批量上传时由 upload_core 安装
        self.rate_limiter = rate_limiter

    def set_rate_limiter(self, rate_limiter) -> None:
        """安装/替换限速器；传 None 表示不限速。"""
        self.rate_limiter = rate_limiter

    def _backoff_sleep(self, attempt: int) -> None:
        """指数退避 + 抖动。

        抖动是必要的：并发场景下所有线程会在同一时刻失败，
        固定退避会让它们同时重试、再次撞上限流。
        """
        try:
            base = self.retry_delay * (self.backoff_factor ** (attempt - 1))
        except OverflowError:
            base = self.MAX_BACKOFF_SECONDS
        if not math.isfinite(base):
            base = self.MAX_BACKOFF_SECONDS
        base = min(max(0.0, base), self.MAX_BACKOFF_SECONDS)
        time.sleep(min(self.MAX_BACKOFF_SECONDS, base * random.uniform(0.6, 1.6)))

    def _retry_after_seconds(self, response) -> Optional[float]:
        value = getattr(response, "headers", {}).get("Retry-After")
        if value is None:
            return None
        try:
            parsed = float(value)
            if not math.isfinite(parsed):
                raise Pan123APIError("服务器返回了无效的 Retry-After，已停止重试")
            return max(0.0, parsed)
        except (TypeError, ValueError):
            try:
                when = parsedate_to_datetime(value)
                if when.tzinfo is None:
                    when = when.replace(tzinfo=timezone.utc)
                return max(0.0, (when - datetime.now(timezone.utc)).total_seconds())
            except (TypeError, ValueError, OverflowError):
                return None

    def _retry_delay(self, attempt: int, response=None) -> None:
        retry_after = self._retry_after_seconds(response) if response is not None else None
        if retry_after is None:
            self._backoff_sleep(attempt)
        else:
            if retry_after > self.MAX_RETRY_AFTER_SECONDS:
                raise Pan123APIError(
                    f"服务器要求等待 {retry_after:g} 秒后重试，超过本地上限 "
                    f"{self.MAX_RETRY_AFTER_SECONDS:g} 秒，已停止重试"
                )
            time.sleep(retry_after)

    def _auth_header(self) -> Dict[str, str]:
        """Return per-request auth headers without mutating shared session state."""
        token = self.token_manager.access_token
        return {"Authorization": f"Bearer {token}"} if token else {}

    def request(self, method: str, endpoint: str, **kwargs) -> Dict[str, Any]:
        """
        发送HTTP请求并在网络层处理重试。
        支持对网络错误、HTTP 5xx、以及响应体中约定的业务错误码进行重试。
        """
        if endpoint.startswith(('http://', 'https://')):
            url = endpoint
        else:
            url = self.base_url + endpoint

        attempt = 0
        auth_refreshed = False
        while True:
            if self.rate_limiter is not None:
                self.rate_limiter.acquire()
            try:
                request_kwargs = dict(kwargs)
                headers = dict(request_kwargs.pop("headers", {}) or {})
                headers.update(self._auth_header())
                response = self.session.request(
                    method, url, timeout=30, headers=headers, **request_kwargs)

                # A 401 can be an HTTP status or the documented body code.
                body = None
                try:
                    candidate = response.json()
                    if isinstance(candidate, dict):
                        body = candidate
                except ValueError:
                    pass
                body_code = body.get("code") if body else None
                try:
                    is_auth_error = response.status_code == 401 or int(body_code) == 401
                except (TypeError, ValueError):
                    is_auth_error = response.status_code == 401
                if is_auth_error and not auth_refreshed:
                    auth_refreshed = True
                    old_token = headers.get("Authorization", "").removeprefix("Bearer ")
                    self.token_manager.refresh_if_current(old_token)
                    continue

                if response.status_code == 429:
                    if attempt < self.max_retries:
                        attempt += 1
                        self._retry_delay(attempt, response)
                        continue

                # 服务器端错误（5xx）可重试
                if 500 <= response.status_code < 600:
                    if attempt < self.max_retries:
                        attempt += 1
                        self._backoff_sleep(attempt)
                        continue
                    response.raise_for_status()

                # 尝试解析 JSON，看是否包含业务错误码需要重试
                try:
                    data = response.json()
                    if isinstance(data, dict):
                        code = data.get('code')
                        try:
                            retry_code = int(code)
                        except (TypeError, ValueError):
                            retry_code = code
                        if retry_code in self.retry_api_codes:
                            if attempt < self.max_retries:
                                attempt += 1
                                self._retry_delay(attempt, response if retry_code == 429 else None)
                                continue
                except ValueError:
                    # 非 JSON 响应则按普通流程继续
                    pass

                # 对剩余的 HTTP 错误统一处理（例如 4xx）
                response.raise_for_status()

                # 交由解析器解析并抛出业务异常（如果有）
                return self._parse_response(response)

            except requests.exceptions.HTTPError as e:
                # HTTP 错误统一转换为 Pan123APIError
                self._handle_http_error(e)
            except requests.exceptions.RequestException as e:
                # 网络级错误（连接、超时等），尝试重试，超出则抛出 NetworkError
                if attempt < self.max_retries:
                    attempt += 1
                    self._backoff_sleep(attempt)
                    continue
                raise NetworkError(f"网络请求失败: {e}")

    def _parse_response(self, response: requests.Response) -> Dict[str, Any]:
        """解析响应体并在业务层发现错误时抛出 Pan123APIError"""
        try:
            data = response.json()
        except ValueError:
            # 空响应且状态码200，返回空字典以兼容现有调用
            if response.status_code == 200 and not response.content:
                return {}
            raise Pan123APIError(
                f"API响应非JSON格式: {response.text[:100]}...", status_code=response.status_code)

        if isinstance(data, dict) and 'code' in data and data['code'] != 0:
            raise Pan123APIError(
                message=data.get('message', 'API返回错误'),
                status_code=response.status_code,
                error_code=data.get('code')
            )

        return data

    def _handle_http_error(self, error: requests.exceptions.HTTPError) -> None:
        """将 requests 的 HTTPError 转换为 Pan123APIError，带上可能的 API 错误码和信息"""
        error_message = f"HTTP错误: {error}"
        error_code_api = None

        if error.response is not None:
            try:
                error_data = error.response.json()
                if isinstance(error_data, dict):
                    error_message = error_data.get('message', error_message)
                    error_code_api = error_data.get('code')
            except ValueError:
                error_message = f"HTTP错误: {error.response.status_code} - {error.response.text[:100]}..."

        raise Pan123APIError(
            error_message,
            status_code=error.response.status_code if error.response is not None else None,
            error_code=error_code_api
        )

    def get(self, endpoint: str, params: Optional[Dict] = None) -> Dict[str, Any]:
        """GET请求"""
        return self.request("GET", endpoint, params=params)

    def post(self, endpoint: str, json_data: Optional[Dict] = None, data: Optional[Dict] = None, files: Optional[Dict] = None) -> Dict[str, Any]:
        """POST请求（支持 json/data/files）"""
        return self.request("POST", endpoint, json=json_data, data=data, files=files)
