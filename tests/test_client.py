"""Pan123Client 的配置解析测试（不连网络、不连 Redis）"""

import json

import pytest

from api import client as client_module


@pytest.fixture
def config_file(tmp_path):
    def write(payload):
        path = tmp_path / "config.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return str(path)
    return write


@pytest.fixture
def spy_init(monkeypatch):
    """拦掉真正的 Redis 连接和 token 申请，只记录参数"""
    captured = {}

    def fake_init_cache(self, host, port, db, password):
        captured.update(host=host, port=port, db=db, password=password)
        return None

    monkeypatch.setattr(client_module.Pan123Client, "_init_cache", fake_init_cache)
    monkeypatch.setattr(client_module.TokenManager, "ensure_valid_token", lambda self: None)
    return captured


def test_redis_params_default_to_config_file(config_file, spy_init):
    """命令行脚本里的 Pan123Client() 应该用 config.json 里的 REDIS，而不是 localhost。"""
    path = config_file({
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "REDIS": {"HOST": "192.168.2.254", "PORT": 6380, "DB": 2,
                  "PASSWORD": "pw", "ENABLED": True},
    })

    client_module.Pan123Client(config_file=path)

    assert spy_init == {"host": "192.168.2.254", "port": 6380,
                        "db": 2, "password": "pw"}


def test_explicit_params_win_over_config(config_file, spy_init):
    path = config_file({
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "REDIS": {"HOST": "192.168.2.254"},
    })

    client_module.Pan123Client(config_file=path, redis_host="localhost", redis_port=6379)

    assert spy_init["host"] == "localhost"
    assert spy_init["port"] == 6379


def test_missing_redis_section_falls_back_to_localhost(config_file, spy_init):
    path = config_file({"CLIENT_ID": "id", "CLIENT_SECRET": "secret"})

    client_module.Pan123Client(config_file=path)

    assert spy_init == {"host": "localhost", "port": 6379, "db": 0, "password": None}


def test_cache_can_be_disabled_by_config(config_file, spy_init):
    path = config_file({
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "REDIS": {"HOST": "192.168.2.254", "ENABLED": False},
    })

    client = client_module.Pan123Client(config_file=path)

    assert client.cache_manager is None
    assert spy_init == {}, "关闭缓存时不应该去连 Redis"


def test_invalid_redis_port_falls_back_to_default(config_file, spy_init):
    path = config_file({
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "REDIS": {"HOST": "192.168.2.254", "PORT": "not-a-number"},
    })

    client_module.Pan123Client(config_file=path)

    assert spy_init["port"] == 6379
