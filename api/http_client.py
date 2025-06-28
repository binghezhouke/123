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
        # 如果endpoint是完整URL，则直接使用；否则，与base_url拼接
        if endpoint.startswith(('http://', 'https://')):
            url = endpoint
        else:
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

    def post(self, endpoint: str, json_data: Optional[Dict] = None, data: Optional[Dict] = None, files: Optional[Dict] = None) -> Dict[str, Any]:
        """POST请求"""
        return self.request("POST", endpoint, json=json_data, data=data, files=files)

    def mkdir(self, name: str, parent_id: int) -> int:
        """
        创建目录
        :param name: 目录名(注:不能重名)
        :param parent_id: 父目录id，上传到根目录时填写 0
        :return: 创建的目录ID
        """
        endpoint = "/upload/v1/file/mkdir"
        json_data = {"name": name, "parentID": parent_id}
        data = self.post(endpoint, json_data=json_data)
        return data['data']['dirID']

    def mkdir_recursive(self, path: str, parent_id: int = 0) -> int:
        """
        递归创建目录
        :param path: 目录路径，如 "foo/bar/baz"
        :param parent_id: 父目录id，上传到根目录时填写 0
        :return: 创建的最终目录ID
        """
        # 处理空路径或根路径的情况
        if not path or path == '/':
            return parent_id

        # 去除开头和结尾的斜杠
        path = path.strip('/')

        # 分割路径
        parts = path.split('/')
        current_id = parent_id

        for part in parts:
            if not part:  # 跳过空目录名
                continue

            try:
                # 尝试创建目录
                current_id = self.mkdir(part, current_id)
                print(f"成功创建目录: {part}, ID: {current_id}")
            except Exception as e:
                # 如果创建失败，可能是目录已存在，尝试查找
                if hasattr(self, 'list_files'):
                    # 如果RequestHandler有list_files方法
                    print(f"创建目录失败，尝试查找已有目录: {part}")
                    try:
                        # 假设list_files方法返回一个包含文件信息的对象
                        files = self.list_files(current_id)
                        for file in files:
                            if file.get('name') == part and file.get('type') == 'folder':
                                current_id = file.get('id')
                                print(f"找到已存在的目录: {part}, ID: {current_id}")
                                break
                        else:
                            raise ValueError(f"无法创建或找到目录: {part}")
                    except Exception as find_error:
                        # 如果查找也失败，则抛出异常
                        raise ValueError(
                            f"创建目录失败，且无法找到已有目录: {part}，错误: {str(e)}, {str(find_error)}")
                else:
                    # 如果没有list_files方法，直接抛出异常
                    raise ValueError(f"创建目录失败: {part}，错误: {str(e)}")

        return current_id
