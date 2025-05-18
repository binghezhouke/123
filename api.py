import requests
import time
import json
from datetime import datetime, timezone  # 新增导入


class Pan123APIError(Exception):
    """自定义API错误异常"""

    def __init__(self, message, status_code=None, error_code=None):
        super().__init__(message)
        self.status_code = status_code
        self.error_code = error_code


class Pan123Client:
    BASE_URL = "https://open-api.123pan.com"
    TOKEN_ENDPOINT = "/api/v1/access_token"
    PLATFORM_HEADER = "open_platform"
    TOKEN_CACHE_FILE = ".pan123_api_token_cache.json"  # 新增：令牌缓存文件名
    CONFIG_FILE = "config.json"  # 新增：配置文件名

    def __init__(self, base_url: str = None):  # 修改：移除 client_id 和 client_secret 参数
        config = self._load_config()
        self.client_id = config.get("CLIENT_ID")
        self.client_secret = config.get("CLIENT_SECRET")

        if not self.client_id or not self.client_secret:
            raise ValueError(
                f"请在 {self.CONFIG_FILE} 中配置 CLIENT_ID 和 CLIENT_SECRET")

        self.base_url = base_url or self.BASE_URL
        self._access_token = None
        self._token_expires_at = 0
        self.session = requests.Session()
        self.session.headers.update({"Platform": self.PLATFORM_HEADER})

        # 初始化时即确保token有效 (会先尝试从缓存加载)
        self._ensure_token()

    def _load_config(self):
        """从配置文件加载 CLIENT_ID 和 CLIENT_SECRET"""
        try:
            with open(self.CONFIG_FILE, 'r') as f:
                config_data = json.load(f)
            return config_data
        except FileNotFoundError:
            print(f"错误: 配置文件 {self.CONFIG_FILE} 未找到。")
            raise
        except json.JSONDecodeError:
            print(f"错误: 配置文件 {self.CONFIG_FILE} 格式不正确。")
            raise
        except Exception as e:
            print(f"加载配置文件 {self.CONFIG_FILE} 时发生未知错误: {e}")
            raise

    def _load_token_from_cache(self):
        """尝试从本地文件加载缓存的令牌"""
        try:
            with open(self.TOKEN_CACHE_FILE, 'r') as f:
                cache_data = json.load(f)
            access_token = cache_data.get("accessToken")
            # tokenExpiresAt from cache is the actual expiry time from API
            token_expires_at_from_file = cache_data.get("tokenExpiresAt")
            if access_token and isinstance(token_expires_at_from_file, (int, float)):
                return access_token, float(token_expires_at_from_file)
        except (FileNotFoundError, json.JSONDecodeError, TypeError, KeyError):
            # 文件未找到、JSON解码错误或键错误，则认为缓存无效
            pass
        return None, 0

    def _save_token_to_cache(self, token_data_to_save):
        """将令牌数据保存到本地缓存文件"""
        # token_data_to_save is {"accessToken": ..., "tokenExpiresAt": ...}
        try:
            with open(self.TOKEN_CACHE_FILE, 'w') as f:
                json.dump(token_data_to_save, f)
        except IOError:
            print(f"警告: 无法将令牌保存到缓存文件 {self.TOKEN_CACHE_FILE}")

    def _get_access_token(self):
        """获取或刷新访问令牌，并缓存结果"""
        payload = {
            "clientID": self.client_id,
            "clientSecret": self.client_secret
        }
        try:
            response = self.session.post(
                self.base_url + self.TOKEN_ENDPOINT,
                json=payload,
                headers={"Content-Type": "application/json"}  # 确保请求头
            )
            response.raise_for_status()  # 检查HTTP错误
            data = response.json()

            if data.get("code") != 0:  # 假设响应中有业务错误码，0代表成功
                raise Pan123APIError(
                    message=data.get("message", "获取令牌失败，未提供具体错误信息"),
                    error_code=data.get("code")
                )

            token_data = data.get("data", {})  # 假设实际令牌信息在data字段下
            self._access_token = token_data.get("accessToken")

            api_true_expires_at = 0  # 将存储在缓存中的实际过期时间戳
            expired_at_str = token_data.get("expiredAt")

            if expired_at_str:
                try:
                    # 将 ISO 8601 格式的字符串转换为 datetime 对象
                    dt_object = datetime.fromisoformat(expired_at_str)
                    # 转换为 UTC 时间戳 (秒)
                    api_true_expires_at = dt_object.timestamp()
                    # self._token_expires_at 用于运行时检查，比实际过期早60秒
                    self._token_expires_at = api_true_expires_at - 60
                except ValueError:
                    print(
                        f"警告: 无法将 expiredAt ('{expired_at_str}') 解析为 ISO 8601 日期时间格式。回退到 expiresIn。")
                    expires_in = token_data.get("expiresIn", 3600)  # 默认1小时
                    api_true_expires_at = time.time() + expires_in
                    self._token_expires_at = api_true_expires_at - 60
            else:
                # 如果没有 expiredAt，则尝试使用 expiresIn
                expires_in = token_data.get("expiresIn", 3600)
                api_true_expires_at = time.time() + expires_in
                self._token_expires_at = api_true_expires_at - 60

            if not self._access_token:
                raise Pan123APIError("未能从响应中获取access_token")

            # 成功获取令牌后，保存到缓存
            if self._access_token and api_true_expires_at > 0:
                self._save_token_to_cache({
                    "accessToken": self._access_token,
                    "tokenExpiresAt": api_true_expires_at  # 存储API返回的原始过期时间戳
                })

            self.session.headers.update(
                {"Authorization": f"Bearer {self._access_token}"})
            return self._access_token
        except requests.exceptions.RequestException as e:
            raise Pan123APIError(
                f"网络请求错误: {e}", status_code=e.response.status_code if e.response else None)
        except ValueError:  # JSON解码错误
            raise Pan123APIError("无法解析令牌响应 (非JSON格式)",
                                 status_code=response.status_code)

    def _ensure_token(self):
        """确保令牌有效，如果无效或即将过期则刷新 (优先从缓存加载)"""
        access_token, cached_actual_expires_at = self._load_token_from_cache()

        # cached_actual_expires_at 是API返回的真实过期时间戳
        # 检查当前时间是否比 (真实过期时间 - 60秒缓冲) 要早
        if access_token and cached_actual_expires_at > (time.time() + 60):
            self._access_token = access_token
            self._token_expires_at = cached_actual_expires_at - 60  # 设置内部使用的、带缓冲的过期时间
            self.session.headers.update(
                {"Authorization": f"Bearer {self._access_token}"})
            return  # 缓存有效，直接返回

        # 缓存无效、不存在或即将过期，则从API获取新令牌
        self._get_access_token()  # 此方法内部会处理缓存的保存

    def _request(self, method: str, endpoint: str, **kwargs):
        """封装通用请求逻辑"""
        self._ensure_token()
        url = self.base_url + endpoint
        try:
            response = self.session.request(method, url, **kwargs)
            response.raise_for_status()  # 抛出HTTP错误 (4xx, 5xx)

            # 尝试解析JSON，如果不是JSON则返回原始响应对象
            try:
                data = response.json()
            except ValueError:
                # 如果API在某些情况下不返回JSON（例如文件下载），则返回原始响应
                # 但对于元数据操作，通常期望JSON
                if response.status_code == 200 and not response.content:  # 例如200 OK但无内容
                    return None
                raise Pan123APIError(
                    f"API响应非JSON格式: {response.text[:100]}...", status_code=response.status_code)

            # 检查业务层错误码 (假设API使用code和message字段)
            # 很多API的成功响应中，code字段为0或者不存在，错误响应中code非0
            if 'code' in data and data['code'] != 0:
                raise Pan123APIError(
                    message=data.get('message', 'API返回错误但未提供消息'),
                    status_code=response.status_code,
                    error_code=data['code']
                )
            return data  # 返回解析后的JSON数据
        except requests.exceptions.HTTPError as e:
            # 尝试从错误响应中解析JSON以获取更详细的错误信息
            error_message = f"HTTP错误: {e}"
            error_code_api = None
            if e.response is not None:
                try:
                    error_data = e.response.json()
                    error_message = error_data.get('message', error_message)
                    error_code_api = error_data.get('code')
                except ValueError:  # 如果错误响应不是JSON
                    error_message = f"HTTP错误: {e.response.status_code} - {e.response.text[:100]}..."
            raise Pan123APIError(
                error_message, status_code=e.response.status_code if e.response else None, error_code=error_code_api)
        except requests.exceptions.RequestException as e:  # 其他网络问题，如超时
            raise Pan123APIError(f"网络请求失败: {e}")

    def _get(self, endpoint: str, params=None):
        return self._request("GET", endpoint, params=params)

    def _post(self, endpoint: str, json_data=None, data=None):
        return self._request("POST", endpoint, json=json_data, data=data)

    def list_files(self,
                   parent_id: int = 0,
                   limit: int = 100,
                   search_data: str = None,
                   search_mode: int = None,
                   last_file_id: int = None
                   ) -> dict:
        """
        列举指定文件夹下的文件和子文件夹或执行搜索 (使用 /api/v2/file/list)。

        :param parent_id: 文件夹ID，根目录传 0。即使提供了 search_data，此参数也必须传递。
        :param limit: 每页文件数量，最大不超过100。
        :param search_data: 搜索关键字。如果提供，将进行全局查找。
        :param search_mode: 搜索模式。0: 全文模糊搜索, 1: 精准搜索。仅在 search_data 提供时有效。
        :param last_file_id: 翻页查询时上一页最后一个文件的ID。
        :return: API响应的JSON数据字典。
        :raises Pan123APIError: 如果API请求失败。
        """
        endpoint = "/api/v2/file/list"
        params = {
            "limit": limit,
            "parentFileId": parent_id  # 始终传递 parentFileId
        }

        if search_data:
            params["searchData"] = search_data
            if search_mode is not None:
                params["searchMode"] = search_mode
            # parentFileId is still passed, but will be ignored by the API as per user's note.

        if last_file_id is not None:
            params["lastFileId"] = last_file_id

        return self._get(endpoint, params=params)

    def get_download_info(self, file_id: int) -> dict:
        """
        获取文件的下载信息，包括下载链接 (使用 /api/v1/file/download_info)。

        :param file_id: 文件ID，必须是一个有效的文件ID。
        :return: API响应的JSON数据字典，包含downloadUrl等信息。
        :raises Pan123APIError: 如果API请求失败，或者请求的是一个文件夹而非文件。
        """
        endpoint = "/api/v1/file/download_info"
        params = {
            "fileId": file_id
        }

        return self._get(endpoint, params=params)


if __name__ == "__main__":
    # CLIENT_ID 和 CLIENT_SECRET 将从 config.json 加载
    # 不再在此处硬编码

    # 创建客户端实例
    try:
        # 修改：不再传递 client_id 和 client_secret
        client = Pan123Client()
        print("客户端初始化成功，并已获取访问令牌。")
    except Pan123APIError as e:
        print(f"客户端初始化或令牌获取失败: {e}")
        if e.status_code:
            print(f"  状态码: {e.status_code}")
        if e.error_code:
            print(f"  错误码: {e.error_code}")
        exit()
    except ValueError as e:  # 捕获配置文件相关的ValueError
        print(f"客户端初始化失败: {e}")
        exit()
    except FileNotFoundError:  # 捕获配置文件未找到的错误
        print(f"请确保 {Pan123Client.CONFIG_FILE} 文件存在且配置正确。")
        exit()
    except Exception as e:  # 其他可能的初始化错误
        print(f"客户端初始化时发生未知错误: {e}")
        exit()

    # 示例：列出根目录的文件和文件夹
    try:
        print("\n尝试列出根目录文件 (默认数量):")
        # Using new parameters: parent_id and limit. last_file_id for pagination.
        files_data = client.list_files(parent_id=0, limit=10)
        if files_data and 'data' in files_data and 'fileList' in files_data['data'] and isinstance(files_data['data']['fileList'], list):
            print(f"  本页返回文件数量: {len(files_data['data']['fileList'])}")
            returned_last_file_id = files_data['data'].get('lastFileId')
            if returned_last_file_id is not None and returned_last_file_id != -1:  # -1 might mean no more pages
                print(f"  lastFileId (for next page): {returned_last_file_id}")
            else:
                print("  (已是最后一页或无法确定下一页的 lastFileId)")
            print("  文件列表:")
            for item in files_data['data']['fileList']:
                item_type = "文件夹" if item.get('type') == 1 else "文件"
                print(
                    f"    - [{item_type}] {item.get('filename')} (ID: {item.get('fileId')})")
        else:
            print("  未能获取文件列表或响应格式不符合预期。")
            print(f"  原始响应: {files_data}")

    except Pan123APIError as e:
        print(f"列出文件失败: {e}")
        if e.status_code:
            print(f"  状态码: {e.status_code}")
        if e.error_code:
            print(f"  错误码: {e.error_code}")

    # 示例：搜索文件
    try:
        print("\n尝试搜索文件 (全局模糊搜索 'download'):")
        # Using new search parameters
        search_results = client.list_files(
            search_data="mp4", search_mode=0, limit=5)
        if search_results and 'data' in search_results and 'fileList' in search_results['data'] and isinstance(search_results['data']['fileList'], list):
            print(f"  搜索结果返回数量: {len(search_results['data']['fileList'])}")
            returned_last_file_id_search = search_results['data'].get(
                'lastFileId')
            if returned_last_file_id_search is not None and returned_last_file_id_search != -1:
                print(
                    f"  lastFileId (for next page of search): {returned_last_file_id_search}")
            else:
                print("  (已是最后一页搜索结果或无法确定下一页的 lastFileId)")
            print("  文件列表:")
            for item in search_results['data']['fileList']:
                # Assuming 'type' field still indicates folder/file
                item_type = "文件夹" if item.get('type') == 1 else "文件"
                print(
                    f"    - [{item_type}] {item.get('filename')} (ID: {item.get('fileId')}, Size: {item.get('size')})")
        else:
            print("  未能获取搜索结果或响应格式不符合预期。")
            print(f"  原始响应: {search_results}")

    except Pan123APIError as e:
        print(f"搜索文件失败: {e}")
        if e.status_code:
            print(f"  状态码: {e.status_code}")
        if e.error_code:
            print(f"  错误码: {e.error_code}")

    # 示例：获取文件下载链接
    try:
        # 假设从前面的搜索或列表操作中获取了有效的文件ID
        # 这里使用一个示例文件ID，实际使用时应替换为真实的文件ID
        sample_file_id = None

        # 尝试从搜索结果中获取一个文件ID（非文件夹）
        if (search_results and 'data' in search_results and 'fileList' in search_results['data'] and
                isinstance(search_results['data']['fileList'], list)):
            for item in search_results['data']['fileList']:
                if item.get('type') != 1:  # 不是文件夹
                    sample_file_id = item.get('fileId')
                    break

        if sample_file_id:
            print(f"\n尝试获取文件下载链接 (文件ID: {sample_file_id}):")
            download_info = client.get_download_info(sample_file_id)
            if download_info and 'data' in download_info and 'downloadUrl' in download_info['data']:
                download_url = download_info['data']['downloadUrl']
                print(f"  下载链接: {download_url}")
            else:
                print("  未能获取下载链接或响应格式不符合预期。")
                print(f"  原始响应: {download_info}")
        else:
            print("\n无法获取文件下载链接示例：未找到有效的文件ID。请确保有可用的文件。")

    except Pan123APIError as e:
        print(f"获取下载链接失败: {e}")
        if e.status_code:
            print(f"  状态码: {e.status_code}")
        if e.error_code:
            print(f"  错误码: {e.error_code}")

    # 更多操作示例可以按需添加...
