"""
HTTP请求处理器（带集中重试逻辑）
"""
import requests
import time
from typing import Dict, Any, Optional, Set
from .exceptions import Pan123APIError, NetworkError


class RequestHandler:
    """HTTP请求处理器，网络层统一负责重试逻辑"""

    PLATFORM_HEADER = "open_platform"

    def __init__(self, base_url: str, token_manager, *, max_retries: int = 5, retry_delay: float = 0.5, backoff_factor: float = 2.0, retry_api_codes: Optional[Set[int]] = None):
        self.base_url = base_url
        self.token_manager = token_manager
        self.session = requests.Session()
        self.session.headers.update({"Platform": self.PLATFORM_HEADER})

        # retry 配置
        self.max_retries = max_retries
        self.retry_delay = retry_delay
        self.backoff_factor = backoff_factor
        # 默认重试业务码：429 (Too Many Requests) 与 20103 (文件校验中)
        self.retry_api_codes = set(
            retry_api_codes) if retry_api_codes is not None else {429, 20103}

    def _update_auth_header(self) -> None:
        """更新认证头"""
        token = self.token_manager.access_token
        if token:
            self.session.headers.update({"Authorization": f"Bearer {token}"})

    def request(self, method: str, endpoint: str, **kwargs) -> Dict[str, Any]:
        """
        发送HTTP请求并在网络层处理重试。
        支持对网络错误、HTTP 5xx、以及响应体中约定的业务错误码进行重试。
        """
        self._update_auth_header()
        if endpoint.startswith(('http://', 'https://')):
            url = endpoint
        else:
            url = self.base_url + endpoint

        attempt = 0
        while True:
            try:
                response = self.session.request(
                    method, url, timeout=30, **kwargs)

                # 服务器端错误（5xx）可重试
                if 500 <= response.status_code < 600:
                    if attempt < self.max_retries:
                        attempt += 1
                        sleep_time = self.retry_delay * \
                            (self.backoff_factor ** (attempt - 1))
                        time.sleep(sleep_time)
                        continue
                    response.raise_for_status()

                # 尝试解析 JSON，看是否包含业务错误码需要重试
                try:
                    data = response.json()
                    if isinstance(data, dict):
                        code = data.get('code')
                        if code is not None and code in self.retry_api_codes:
                            if attempt < self.max_retries:
                                attempt += 1
                                sleep_time = self.retry_delay * \
                                    (self.backoff_factor ** (attempt - 1))
                                time.sleep(sleep_time)
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
                    sleep_time = self.retry_delay * \
                        (self.backoff_factor ** (attempt - 1))
                    time.sleep(sleep_time)
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
            status_code=error.response.status_code if error.response else None,
            error_code=error_code_api
        )

    def get(self, endpoint: str, params: Optional[Dict] = None) -> Dict[str, Any]:
        """GET请求"""
        return self.request("GET", endpoint, params=params)

    def post(self, endpoint: str, json_data: Optional[Dict] = None, data: Optional[Dict] = None, files: Optional[Dict] = None) -> Dict[str, Any]:
        """POST请求（支持 json/data/files）"""
        return self.request("POST", endpoint, json=json_data, data=data, files=files)
