"""
文件缓存管理器

缓存内容用 JSON 序列化：Redis 里的数据一旦被第三方写入，pickle 反序列化
就等于在本进程里执行任意代码，这里不值得冒这个风险。
"""
import json
import logging
import redis
from datetime import datetime
from typing import Optional, Tuple, Any

logger = logging.getLogger(__name__)


def _to_text(value) -> str:
    """兼容 decode_responses=True/False 两种 Redis 客户端配置。"""
    return value.decode('utf-8') if isinstance(value, (bytes, bytearray)) else value


class FileCacheManager:
    """文件信息缓存管理器"""

    def __init__(self, redis_client: redis.Redis, default_ttl: int = 3600):
        """
        初始化缓存管理器
        :param redis_client: Redis客户端实例
        :param default_ttl: 默认缓存过期时间（秒），默认1小时
        """
        self.redis_client = redis_client
        self.default_ttl = default_ttl
        self.cache_prefix = "file_cache:"
        self.fetch_time_prefix = "fetch_time:"

    def _get_cache_key(self, file_id: int) -> str:
        """获取文件缓存键"""
        return f"{self.cache_prefix}{file_id}"

    def _get_fetch_time_key(self, file_id: int) -> str:
        """获取文件获取时间缓存键"""
        return f"{self.fetch_time_prefix}{file_id}"

    def should_use_cache(self, file_id: int, file_update_time: str = None) -> Tuple[bool, Any]:
        """
        判断是否应该使用缓存
        :param file_id: 文件ID
        :param file_update_time: 文件更新时间字符串
        :return: (should_use_cache, cached_data)
        """
        if not self.redis_client:
            return False, None

        try:
            cache_key = self._get_cache_key(file_id)
            fetch_time_key = self._get_fetch_time_key(file_id)

            cached_data = self.redis_client.get(cache_key)
            fetch_time_str = self.redis_client.get(fetch_time_key)

            if not cached_data or not fetch_time_str:
                return False, None

            # 反序列化缓存数据（旧版本写入的 pickle 数据会解析失败，
            # 被下面的 except 兜住当成缓存未命中，不影响正确性）
            cached_file_info = json.loads(_to_text(cached_data))
            fetch_time = datetime.fromisoformat(_to_text(fetch_time_str))

            # 如果没有文件更新时间，直接使用缓存
            if not file_update_time:
                return True, cached_file_info

            # 解析文件更新时间
            file_update_dt = self._parse_update_time(file_update_time)
            if file_update_dt and file_update_dt <= fetch_time:
                return True, cached_file_info

            return False, None

        except Exception as e:
            logger.warning("缓存检查失败: %s", e)
            return False, None

    def _parse_update_time(self, time_str: str) -> Optional[datetime]:
        """解析更新时间字符串"""
        if not isinstance(time_str, str):
            return None

        # 尝试解析不同格式的时间字符串
        time_formats = [
            '%Y-%m-%d %H:%M:%S',
            '%Y-%m-%dT%H:%M:%S',
            '%Y-%m-%d'
        ]

        for fmt in time_formats:
            try:
                return datetime.strptime(time_str, fmt)
            except ValueError:
                continue

        return None

    def set_cache(self, file_id: int, file_info: dict, ttl: int = None) -> None:
        """
        设置文件信息缓存
        :param file_id: 文件ID
        :param file_info: 文件信息
        :param ttl: 缓存过期时间（秒）
        """
        if not self.redis_client:
            return

        try:
            cache_key = self._get_cache_key(file_id)
            fetch_time_key = self._get_fetch_time_key(file_id)

            # 序列化文件信息
            cached_data = json.dumps(file_info, ensure_ascii=False).encode('utf-8')
            fetch_time = datetime.now().isoformat()

            ttl = ttl or self.default_ttl

            # 设置缓存
            self.redis_client.setex(cache_key, ttl, cached_data)
            self.redis_client.setex(fetch_time_key, ttl, fetch_time)

        except Exception as e:
            logger.warning("设置缓存失败: %s", e)

    def delete_cache(self, file_id: int) -> None:
        """删除指定文件的缓存"""
        if not self.redis_client:
            return

        try:
            cache_key = self._get_cache_key(file_id)
            fetch_time_key = self._get_fetch_time_key(file_id)

            self.redis_client.delete(cache_key)
            self.redis_client.delete(fetch_time_key)

        except Exception as e:
            logger.warning("删除缓存失败: %s", e)

    def _scan_keys(self, pattern: str):
        """用 SCAN 迭代匹配的键（KEYS 会阻塞整个 Redis 实例）。"""
        keys = []
        for key in self.redis_client.scan_iter(match=pattern, count=500):
            keys.append(key)
        return keys

    def clear_all_cache(self) -> None:
        """清空所有文件缓存"""
        if not self.redis_client:
            return

        try:
            # 获取所有相关的键
            all_keys = self._scan_keys(f"{self.cache_prefix}*") + \
                self._scan_keys(f"{self.fetch_time_prefix}*")
            if all_keys:
                self.redis_client.delete(*all_keys)

        except Exception as e:
            logger.warning("清空缓存失败: %s", e)

    def get_cache_stats(self) -> dict:
        """获取缓存统计信息"""
        if not self.redis_client:
            return {"enabled": False}

        try:
            cache_keys = self._scan_keys(f"{self.cache_prefix}*")
            return {
                "enabled": True,
                "total_cached_files": len(cache_keys),
                "cache_prefix": self.cache_prefix
            }
        except Exception as e:
            return {"enabled": True, "error": str(e)}
