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

    def get_webdav_config(self) -> Dict[str, Any]:
        """
        获取WebDAV配置

        :return: 包含WebDAV配置的字典
        """
        config = self.load_config()
        webdav_config = config.get('WEBDAV', {})

        # 提取基本配置
        webdav_user = webdav_config.get('USERNAME')
        webdav_password = webdav_config.get('PASSWORD')
        webdav_host = webdav_config.get('BASE_URL', 'webdav-1836076489.pd1.123pan.cn')

        # 如果BASE_URL包含了完整URL，则提取主机部分
        if webdav_host and webdav_host.startswith('http'):
            from urllib.parse import urlparse
            parsed_url = urlparse(webdav_host)
            webdav_host = parsed_url.netloc

        # 返回格式化的配置
        return {
            'webdav_user': webdav_user,
            'webdav_password': webdav_password,
            'webdav_host': webdav_host,
            'webdav_enabled': webdav_config.get('ENABLED', False)
        }
