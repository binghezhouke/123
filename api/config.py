"""
配置管理器
"""
import json
import os
from typing import Dict, Any
from .exceptions import ConfigurationError


class ConfigManager:
    """配置管理器"""

    DEFAULT_CONFIG_FILE = "config.json"

    def __init__(self, config_file: str = None):
        self.config_file = config_file or self.DEFAULT_CONFIG_FILE
        self._config = None

    def load_config(self) -> Dict[str, Any]:
        """加载配置文件"""
        if self._config is not None:
            return self._config

        try:
            if not os.path.exists(self.config_file):
                raise ConfigurationError(f"配置文件 {self.config_file} 不存在")

            with open(self.config_file, 'r', encoding='utf-8') as f:
                self._config = json.load(f)

            # 验证必需的配置项
            required_keys = ['CLIENT_ID', 'CLIENT_SECRET']
            missing_keys = [
                key for key in required_keys if not self._config.get(key)]

            if missing_keys:
                raise ConfigurationError(f"配置文件缺少必需的配置项: {missing_keys}")

            return self._config

        except json.JSONDecodeError as e:
            raise ConfigurationError(f"配置文件 {self.config_file} 格式错误: {e}")
        except Exception as e:
            raise ConfigurationError(f"加载配置文件失败: {e}")

    def get(self, key: str, default=None):
        """获取配置项"""
        config = self.load_config()
        return config.get(key, default)

    def get_client_credentials(self) -> tuple:
        """获取客户端凭据"""
        config = self.load_config()
        return config['CLIENT_ID'], config['CLIENT_SECRET']

    def get_redis_config(self) -> Dict[str, Any]:
        """
        获取 Redis 配置。

        没配置 REDIS 段时给出与旧代码一致的默认值（本机 6379、启用缓存），
        这样从 config.json 读配置的调用方（命令行脚本）也能用上缓存。
        """
        config = self.load_config()
        redis_config = config.get('REDIS') or {}

        def as_int(value, default):
            try:
                return int(value)
            except (TypeError, ValueError):
                return default

        return {
            'host': redis_config.get('HOST', 'localhost'),
            'port': as_int(redis_config.get('PORT'), 6379),
            'db': as_int(redis_config.get('DB'), 0),
            'password': redis_config.get('PASSWORD'),
            'enabled': redis_config.get('ENABLED', True),
        }

    def get_webdav_config(self) -> Dict[str, Any]:
        """
        获取WebDAV配置

        键名以 USERNAME / BASE_URL / PATH_PREFIX 为准；
        USER / HOST 是 config.json.template 早期版本用过的名字，这里继续兼容。

        :return: 包含WebDAV配置的字典
        """
        config = self.load_config()
        webdav_config = config.get('WEBDAV', {})

        # 提取基本配置（兼容旧键名）
        webdav_user = webdav_config.get('USERNAME', webdav_config.get('USER'))
        webdav_password = webdav_config.get('PASSWORD')
        webdav_host = webdav_config.get(
            'BASE_URL', webdav_config.get('HOST', ''))
        webdav_path_prefix = webdav_config.get('PATH_PREFIX', '/webdav')

        # 如果BASE_URL包含了完整URL，则提取主机部分
        if webdav_host and webdav_host.startswith('http'):
            from urllib.parse import urlparse
            parsed_url = urlparse(webdav_host)
            webdav_host = parsed_url.netloc

        if not webdav_host:
            # 未启用 WebDAV 时允许留空；启用了却配不全则直接报错，避免静默不可用
            if webdav_config.get('ENABLED', False):
                raise ConfigurationError(
                    "WebDAV 已启用但缺少 BASE_URL（或旧键名 HOST）")

        # 返回格式化的配置
        return {
            'webdav_user': webdav_user,
            'webdav_password': webdav_password,
            'webdav_host': webdav_host,
            'webdav_path_prefix': webdav_path_prefix,
            'webdav_enabled': webdav_config.get('ENABLED', False)
        }
