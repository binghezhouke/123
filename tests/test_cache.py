"""FileCacheManager 缓存读写测试（用假 Redis，不需要真实实例）"""

import fnmatch
import pickle

from api.cache import FileCacheManager


class FakeRedis:
    """只实现缓存管理器用到的那几个命令"""

    def __init__(self):
        self.store = {}
        self.ttls = {}

    def get(self, key):
        return self.store.get(key)

    def setex(self, key, ttl, value):
        # 真实客户端是 decode_responses=False，写入的字符串会以 bytes 存回来
        self.store[key] = value.encode("utf-8") if isinstance(value, str) else value
        self.ttls[key] = ttl

    def delete(self, *keys):
        for key in keys:
            self.store.pop(key, None)
            self.ttls.pop(key, None)

    def scan_iter(self, match=None, count=None):
        for key in list(self.store):
            if match is None or fnmatch.fnmatch(key, match):
                yield key


def test_set_and_get_round_trip():
    redis = FakeRedis()
    cache = FileCacheManager(redis)
    info = {"fileId": 1, "filename": "中文名.txt", "size": 10}

    cache.set_cache(1, info)
    should_use, cached = cache.should_use_cache(1)

    assert should_use is True
    assert cached == info
    assert redis.ttls["file_cache:1"] == 3600


def test_cache_miss_when_never_stored():
    cache = FileCacheManager(FakeRedis())

    assert cache.should_use_cache(99) == (False, None)


def test_cache_invalidated_when_file_is_newer():
    cache = FileCacheManager(FakeRedis())
    cache.set_cache(1, {"fileId": 1})

    should_use, _ = cache.should_use_cache(1, file_update_time="2999-01-01 00:00:00")
    assert should_use is False

    should_use, _ = cache.should_use_cache(1, file_update_time="2000-01-01 00:00:00")
    assert should_use is True


def test_legacy_pickle_payload_is_treated_as_miss():
    """旧版本用 pickle 写缓存，读取时应安全地当成未命中而不是抛异常。"""
    redis = FakeRedis()
    cache = FileCacheManager(redis)
    redis.store["file_cache:1"] = pickle.dumps({"fileId": 1})
    redis.store["fetch_time:1"] = b"2026-01-01T00:00:00"

    assert cache.should_use_cache(1) == (False, None)


def test_clear_all_cache_removes_both_prefixes():
    redis = FakeRedis()
    cache = FileCacheManager(redis)
    cache.set_cache(1, {"fileId": 1})
    cache.set_cache(2, {"fileId": 2})
    redis.store["unrelated"] = b"keep"

    cache.clear_all_cache()

    assert cache.should_use_cache(1) == (False, None)
    assert cache.get_cache_stats()["total_cached_files"] == 0
    assert redis.store["unrelated"] == b"keep"


def test_delete_cache_only_removes_target():
    redis = FakeRedis()
    cache = FileCacheManager(redis)
    cache.set_cache(1, {"fileId": 1})
    cache.set_cache(2, {"fileId": 2})

    cache.delete_cache(1)

    assert cache.should_use_cache(1) == (False, None)
    assert cache.should_use_cache(2)[0] is True


def test_stats_disabled_without_redis():
    assert FileCacheManager(None).get_cache_stats() == {"enabled": False}
