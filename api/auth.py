"""
认证和令牌管理
"""
import json
import time
from datetime import datetime
from typing import Optional, Tuple
import requests
from .exceptions import AuthenticationError, NetworkError


class TokenManager:
    """令牌管理器"""

    TOKEN_CACHE_FILE = ".pan123_api_token_cache.json"
    TOKEN_ENDPOINT = "/api/v1/access_token"

    def __init__(self, base_url: str, client_id: str, client_secret: str):
        self.base_url = base_url
        self.client_id = client_id
        self.client_secret = client_secret
        self._access_token = None
        self._token_expires_at = 0

    @property
    def access_token(self) -> str:
        """获取当前有效的访问令牌"""
        self.ensure_valid_token()
        return self._access_token

    def ensure_valid_token(self) -> None:
        """确保令牌有效，如果无效则刷新"""
        # 首先尝试从缓存加载
        if self._try_load_from_cache():
            return

        # 缓存无效，从API获取新令牌
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
        """保存令牌到缓存"""
        try:
            cache_data = {
                "accessToken": access_token,
                "tokenExpiresAt": expires_at
            }
            with open(self.TOKEN_CACHE_FILE, 'w', encoding='utf-8') as f:
                json.dump(cache_data, f)
        except IOError as e:
            print(f"警告: 无法保存令牌到缓存: {e}")

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
                print(f"警告: 无法解析过期时间格式: {expired_at_str}")

        # 回退到使用 expiresIn
        expires_in = token_data.get("expiresIn", 3600)
        return time.time() + expires_in

    def clear_cache(self) -> None:
        """清除令牌缓存"""
        try:
            import os
            if os.path.exists(self.TOKEN_CACHE_FILE):
                os.remove(self.TOKEN_CACHE_FILE)
        except Exception as e:
            print(f"清除令牌缓存失败: {e}")

    def is_token_valid(self) -> bool:
        """检查当前令牌是否有效"""
        return (self._access_token is not None and
                self._token_expires_at > time.time())
