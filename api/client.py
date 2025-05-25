"""
重构后的Pan123客户端主类
"""
import redis
from typing import Optional
from .config import ConfigManager
from .auth import TokenManager
from .http_client import RequestHandler
from .cache import FileCacheManager
from .file_service import FileService
from .exceptions import ConfigurationError


class Pan123Client:
    """Pan123 API客户端"""

    BASE_URL = "https://open-api.123pan.com"

    def __init__(self,
                 base_url: str = None,
                 config_file: str = None,
                 redis_host: str = 'localhost',
                 redis_port: int = 6379,
                 redis_db: int = 0,
                 redis_password: str = None,
                 enable_cache: bool = True):
        """
        初始化Pan123客户端

        :param base_url: API基础URL
        :param config_file: 配置文件路径
        :param redis_host: Redis主机
        :param redis_port: Redis端口
        :param redis_db: Redis数据库
        :param redis_password: Redis密码
        :param enable_cache: 是否启用缓存
        """
        self.base_url = base_url or self.BASE_URL

        # 初始化配置管理器
        self.config_manager = ConfigManager(config_file)

        try:
            client_id, client_secret = self.config_manager.get_client_credentials()
        except Exception as e:
            raise ConfigurationError(f"配置初始化失败: {e}")

        # 初始化认证管理器
        self.token_manager = TokenManager(
            self.base_url, client_id, client_secret)

        # 初始化HTTP客户端
        self.http_client = RequestHandler(self.base_url, self.token_manager)

        # 初始化缓存管理器
        self.cache_manager = None
        if enable_cache:
            self.cache_manager = self._init_cache(
                redis_host, redis_port, redis_db, redis_password)

        # 初始化文件服务
        self.file_service = FileService(self.http_client, self.cache_manager)

        # 确保token有效
        self.token_manager.ensure_valid_token()

    def _init_cache(self, host: str, port: int, db: int, password: str) -> Optional[FileCacheManager]:
        """初始化缓存管理器"""
        try:
            redis_client = redis.Redis(
                host=host,
                port=port,
                db=db,
                password=password,
                decode_responses=False
            )
            # 测试连接
            redis_client.ping()
            print("✓ Redis缓存初始化成功")
            return FileCacheManager(redis_client)
        except Exception as e:
            print(f"✗ Redis缓存初始化失败: {e}")
            return None

    # 文件操作方法（委托给file_service）
    def list_files(self, parent_id: int = 0, limit: int = 100,
                   search_data: str = None, search_mode: int = None,
                   last_file_id: int = None, auto_fetch_all: bool = False,
                   qps_limit: float = 1.0, max_pages: int = 100):
        """列出文件"""
        return self.file_service.list_files(
            parent_id, limit, search_data, search_mode, last_file_id,
            auto_fetch_all, qps_limit, max_pages)

    def get_files_info(self, file_ids: list, use_cache: bool = True):
        """获取多个文件信息"""
        return self.file_service.get_files_info(file_ids, use_cache)

    def get_file_info_single(self, file_id: int, use_cache: bool = True):
        """获取单个文件信息"""
        return self.file_service.get_file_info_single(file_id, use_cache)

    def get_file_path(self, file_id: int, use_cache: bool = True):
        """获取文件完整路径"""
        return self.file_service.get_file_path(file_id, use_cache)

    def get_file_path_with_details(self, file_id: int, use_cache: bool = True):
        """获取文件完整路径及详细信息"""
        return self.file_service.get_file_path_with_details(file_id, use_cache)

    def get_download_info(self, file_id: int):
        """获取下载信息"""
        return self.file_service.get_download_info(file_id)

    def clear_file_cache(self, file_id: int = None):
        """清除文件缓存"""
        self.file_service.clear_file_cache(file_id)

    # 实用方法
    def get_cache_stats(self) -> dict:
        """获取缓存统计信息"""
        if self.cache_manager:
            return self.cache_manager.get_cache_stats()
        return {"enabled": False}

    def is_authenticated(self) -> bool:
        """检查是否已认证"""
        return self.token_manager.is_token_valid()

    def refresh_token(self):
        """刷新访问令牌"""
        self.token_manager.clear_cache()
        self.token_manager.ensure_valid_token()

    def __enter__(self):
        """上下文管理器支持"""
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """清理资源"""
        if hasattr(self.http_client.session, 'close'):
            self.http_client.session.close()
