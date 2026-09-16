"""
认证和令牌管理
"""
import json
import logging
import os
import threading
import time
from datetime import datetime
import requests
from .exceptions import AuthenticationError, NetworkError

logger = logging.getLogger(__name__)


class TokenManager:
    """令牌管理器"""

    # 固定到家目录缓存：同一台机器上无论从哪个目录运行，都共享同一个 token，
    # 避免开放平台"同 clientID 最多 3 个 token"限制被自己挤爆
    TOKEN_CACHE_FILE = os.path.join(
        os.path.expanduser("~"), ".cache", "pan123_api", "token.json")
    TOKEN_ENDPOINT = "/api/v1/access_token"

    def __init__(self, base_url: str, client_id: str, client_secret: str):
        self.base_url = base_url
        self.client_id = client_id
        self.client_secret = client_secret
        self._access_token = None
        self._token_expires_at = 0
        self._lock = threading.Lock()

    @property
    def access_token(self) -> str:
        """获取当前有效的访问令牌"""
        self.ensure_valid_token()
        return self._access_token

    def ensure_valid_token(self) -> None:
        """
        确保令牌有效。

        内存里已经有效就直接返回：既省掉每个请求读一次缓存文件，
        也避免并发时多个线程同时去申请新 token —— 同一个 clientID
        同时最多只能有 3 个 token，申请多了会互相挤掉。
        """
        if self.is_token_valid():
            return

        with self._lock:
            # 双重检查：等锁期间可能已经有别的线程刷新好了
            if self.is_token_valid():
                return

            if not self._try_load_from_cache():
                self._fetch_new_token()

    def _try_load_from_cache(self) -> bool:
        """尝试从缓存加载令牌"""
        try:
            with open(self.TOKEN_CACHE_FILE, 'r', encoding='utf-8') as f:
                cache_data = json.load(f)

            access_token = cache_data.get("accessToken")
            expires_at = cache_data.get("tokenExpiresAt")

            if access_token and expires_at and expires_at > (time.time() + 60):
                self._access_token = access_token
                self._token_expires_at = expires_at - 60
                return True

        except (FileNotFoundError, json.JSONDecodeError, KeyError, TypeError):
            pass

        return False

    def _save_to_cache(self, access_token: str, expires_at: float) -> None:
        """保存令牌到缓存（文件权限 0600，避免同机其他用户读取令牌）"""
        try:
            cache_dir = os.path.dirname(self.TOKEN_CACHE_FILE)
            os.makedirs(cache_dir, mode=0o700, exist_ok=True)
            # makedirs 的 mode 只在创建时生效，已存在的目录需要显式收紧
            os.chmod(cache_dir, 0o700)

            cache_data = {
                "accessToken": access_token,
                "tokenExpiresAt": expires_at
            }
            # 直接以 0600 打开写入，避免出现"先 0644 再 chmod"的窗口期
            fd = os.open(self.TOKEN_CACHE_FILE,
                         os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, 'w', encoding='utf-8') as f:
                json.dump(cache_data, f)
            os.chmod(self.TOKEN_CACHE_FILE, 0o600)
        except IOError as e:
            logger.warning("无法保存令牌到缓存: %s", e)

    def _fetch_new_token(self) -> None:
        """从API获取新令牌"""
        payload = {
            "clientID": self.client_id,
            "clientSecret": self.client_secret
        }

        try:
            response = requests.post(
                self.base_url + self.TOKEN_ENDPOINT,
                json=payload,
                headers={"Content-Type": "application/json",
                         "Platform": "open_platform"},
                timeout=30
            )
            response.raise_for_status()

            data = response.json()

            if data.get("code") != 0:
                raise AuthenticationError(
                    message=data.get("message", "获取令牌失败"),
                    error_code=data.get("code")
                )

            token_data = data.get("data", {})
            self._access_token = token_data.get("accessToken")

            if not self._access_token:
                raise AuthenticationError("响应中缺少访问令牌")

            # 处理过期时间
            expires_at = self._parse_expiry_time(token_data)
            self._token_expires_at = expires_at - 60  # 提前60秒过期

            # 保存到缓存
            self._save_to_cache(self._access_token, expires_at)

        except requests.exceptions.RequestException as e:
            raise NetworkError(f"网络请求失败: {e}")
        except json.JSONDecodeError:
            raise AuthenticationError("令牌响应格式错误")

    def _parse_expiry_time(self, token_data: dict) -> float:
        """解析令牌过期时间"""
        expired_at_str = token_data.get("expiredAt")

        if expired_at_str:
            try:
                dt_object = datetime.fromisoformat(expired_at_str)
                return dt_object.timestamp()
            except ValueError:
                logger.warning("无法解析过期时间格式: %s", expired_at_str)

        # 回退到使用 expiresIn
        expires_in = token_data.get("expiresIn", 3600)
        return time.time() + expires_in

    def clear_cache(self) -> None:
        """清除令牌缓存（文件和内存里的都清掉，下次调用会重新申请）"""
        with self._lock:
            self._access_token = None
            self._token_expires_at = 0
            try:
                if os.path.exists(self.TOKEN_CACHE_FILE):
                    os.remove(self.TOKEN_CACHE_FILE)
            except Exception as e:
                logger.warning("清除令牌缓存失败: %s", e)

    def is_token_valid(self) -> bool:
        """检查当前令牌是否有效"""
        return (self._access_token is not None and
                self._token_expires_at > time.time())
