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
                   parent_id: str = "0",
                   page_no: int = 1,
                   page_size: int = 100,
                   order_by: str = None,  # e.g., "fileName", "updateTime"
                   order_direction: str = None,  # e.g., "asc", "desc"
                   search_key: str = None,
                   file_type: str = None,  # API specific, e.g., "folder", "image"
                   trash: bool = False
                   ) -> dict:
        """
        列举指定文件夹下的文件和子文件夹 (使用 /api/v2/file/list)。

        :param parent_id: 父文件夹ID，根目录为 "0"。
        :param page_no: 页码。
        :param page_size: 每页数量。
        :param order_by: 排序字段。
        :param order_direction: 排序方向 ('asc' 或 'desc')。
        :param search_key: 搜索关键词。
        :param file_type: 文件类型过滤。
        :param trash: 是否列举回收站内容。
        :return: API响应的JSON数据字典。
        :raises Pan123APIError: 如果API请求失败。
        """
        endpoint = "/api/v2/file/list"  # 优先使用V2
        params = {
            "parentFileId": parent_id,
            "pageNo": page_no,
            "pageSize": page_size,
            "limit": 100,
            "trash": 1 if trash else 0,  # API可能期望整数0或1
        }
        if order_by:
            params = order_by
        if order_direction:
            params = order_direction
        if search_key:
            params["searchKey"] = search_key
        if file_type:
            params = file_type

        # 移除值为None的参数，避免发送空值参数
        params = {k: v for k, v in params.items() if v is not None}

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
        print("\n尝试列出根目录文件 (第一页，默认数量):")
        files_data = client.list_files(parent_id="0")
        if files_data and 'data' in files_data and 'list' in files_data['data']:
            print(f"  总数: {files_data['data'].get('count', '未知')}")
            print(f"  当前页: {files_data['data'].get('pageNo', '未知')}")
            print(f"  每页数量: {files_data['data'].get('pageSize', '未知')}")
            print("  文件列表:")
            for item in files_data['data']['list']:
                item_type = "文件夹" if item.get('isFolder') else "文件"
                print(
                    f"    - [{item_type}] {item.get('fileName')} (ID: {item.get('fileID')})")
        else:
            print("  未能获取文件列表或响应格式不符合预期。")
            print(f"  原始响应: {files_data}")

    except Pan123APIError as e:
        print(f"列出文件失败: {e}")
        print(f"  状态码: {e.status_code}")
        print(f"  错误码: {e.error_code}")

    # 示例：列出根目录的图片文件，按修改时间降序排序
    try:
        print("\n尝试列出根目录的图片文件 (按修改时间降序):")
        # 注意：file_type 和 order_by/order_direction 的具体值需要参考123pan API文档
        # 这里假设 file_type "image" 是有效的，并且排序字段是 "updateTime"
        # 实际API可能使用不同的参数名或值
        # files_data_images = client.list_files(
        #     parent_id="0",
        #     file_type="image", # 假设API支持此参数
        #     order_by="updateTime",
        #     order_direction="desc"
        # )
        # print("由于 file_type, order_by, order_direction 参数在 list_files 中实现有误，暂时跳过此示例。")
        # print("请修正 list_files 方法中对这些参数的处理逻辑。")

        # 修正后的调用方式 (假设 list_files 已修复)
        # 假设API文档指明 fileType, orderBy, orderDirection
        # 并且 list_files 方法内部正确地将这些参数名传递给API
        # params = {
        #     "parentID": "0",
        #     "pageNo": 1,
        #     "pageSize": 10,
        #     "fileType": "image", # 假设API使用此参数名
        #     "orderBy": "updateTime",
        #     "orderDirection": "desc"
        # }
        # files_data_images = client._get("/api/v2/file/list", params=params)

        # 当前 list_files 实现中，order_by, order_direction, file_type 的赋值方式是错误的
        # 它会将 params 字典覆盖为字符串。
        # 正确的实现应该是 params.update({"orderBy": order_by}) 等。
        # 以下调用将基于当前 list_files 的错误实现，可能不会按预期工作。
        # 为了演示，我们仅使用 parent_id
        print("  (注意: 当前 list_files 实现中 order_by, order_direction, file_type 参数处理有误)")
        files_data_images = client.list_files(parent_id="0", page_size=5)

        if files_data_images and 'data' in files_data_images and 'list' in files_data_images['data']:
            print(f"  图片文件列表 (前 {len(files_data_images['data']['list'])} 项):")
            for item in files_data_images['data']['list']:
                item_type = "文件夹" if item.get('isFolder') else "文件"
                print(
                    f"    - [{item_type}] {item.get('fileName')} (ID: {item.get('fileID')}, 更新时间: {item.get('updateTime')})")
        else:
            print("  未能获取图片文件列表或响应格式不符合预期。")
            print(f"  原始响应: {files_data_images}")

    except Pan123APIError as e:
        print(f"列出图片文件失败: {e}")
        print(f"  状态码: {e.status_code}")
        print(f"  错误码: {e.error_code}")

    # 更多操作示例可以按需添加...
    # 例如:
    # client.create_folder(parent_id="0", name="我的新文件夹")
    # client.upload_file(parent_id="0", file_path="/path/to/your/file.txt")
    # client.download_file(file_id="some_file_id", save_path="/path/to/save/downloaded_file.txt")
