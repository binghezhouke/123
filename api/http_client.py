"""
HTTP请求处理器
"""
import requests
import time
from typing import Dict, Any, Optional
from .exceptions import Pan123APIError, NetworkError


class RequestHandler:
    """HTTP请求处理器"""

    PLATFORM_HEADER = "open_platform"

    def __init__(self, base_url: str, token_manager):
        self.base_url = base_url
        self.token_manager = token_manager
        self.session = requests.Session()
        self.session.headers.update({"Platform": self.PLATFORM_HEADER})
        self.max_retries = 5  # 默认最大重试次数
        self.retry_delay = 0.5  # 重试延迟时间(秒)

    def _update_auth_header(self) -> None:
        """更新认证头"""
        token = self.token_manager.access_token
        self.session.headers.update({"Authorization": f"Bearer {token}"})

    def request(self, method: str, endpoint: str, **kwargs) -> Dict[str, Any]:
        """
        发送HTTP请求
        :param method: HTTP方法
        :param endpoint: API端点
        :param kwargs: 请求参数
        :return: 响应数据
        """
        self._update_auth_header()
        url = self.base_url + endpoint
        retry_count = 0

        while True:
            try:
                response = self.session.request(
                    method, url, timeout=30, **kwargs)
                response.raise_for_status()

                # 先尝试解析响应
                try:
                    data = response.json()
                    # 检查业务层错误码是否为429(请求过于频繁)
                    if 'code' in data and data['code'] == 429:
                        # 达到最大重试次数则抛出异常
                        if retry_count >= self.max_retries:
                            return self._parse_response(response)
                        # 未达到最大重试次数则等待后重试
                        retry_count += 1
                        time.sleep(self.retry_delay)
                        continue
                except ValueError:
                    # 如果不是JSON格式，继续正常处理
                    pass

                return self._parse_response(response)

            except requests.exceptions.HTTPError as e:
                self._handle_http_error(e)
            except requests.exceptions.RequestException as e:
                raise NetworkError(f"网络请求失败: {e}")

            # 如果上面没有继续循环，就退出循环
            break

    def _parse_response(self, response: requests.Response) -> Dict[str, Any]:
        """解析响应"""
        try:
            data = response.json()
        except ValueError:
            if response.status_code == 200 and not response.content:
                return {}
            raise Pan123APIError(
                f"API响应非JSON格式: {response.text[:100]}...",
                status_code=response.status_code
            )

        # 检查业务层错误码
        if 'code' in data and data['code'] != 0:
            raise Pan123APIError(
                message=data.get('message', 'API返回错误'),
                status_code=response.status_code,
                error_code=data['code']
            )

        return data

    def _handle_http_error(self, error: requests.exceptions.HTTPError) -> None:
        """处理HTTP错误"""
        error_message = f"HTTP错误: {error}"
        error_code_api = None

        if error.response is not None:
            try:
                error_data = error.response.json()
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

    def post(self, endpoint: str, json_data: Optional[Dict] = None, data: Optional[Dict] = None) -> Dict[str, Any]:
        """POST请求"""
        return self.request("POST", endpoint, json=json_data, data=data)
