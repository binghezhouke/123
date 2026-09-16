"""ConfigManager 配置解析测试（尤其是 WebDAV 键名）"""

import json

import pytest

from api.config import ConfigManager
from api.exceptions import ConfigurationError


def write_config(tmp_path, payload):
    path = tmp_path / "config.json"
    path.write_text(json.dumps(payload), encoding="utf-8")
    return str(path)


def test_missing_file_raises(tmp_path):
    with pytest.raises(ConfigurationError):
        ConfigManager(str(tmp_path / "nope.json")).load_config()


def test_missing_credentials_raises(tmp_path):
    path = write_config(tmp_path, {"CLIENT_ID": "id"})

    with pytest.raises(ConfigurationError):
        ConfigManager(path).load_config()


def test_credentials_are_returned(tmp_path):
    path = write_config(tmp_path, {"CLIENT_ID": "id", "CLIENT_SECRET": "secret"})

    assert ConfigManager(path).get_client_credentials() == ("id", "secret")


def test_webdav_config_prefers_documented_keys(tmp_path):
    path = write_config(tmp_path, {
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "WEBDAV": {
            "ENABLED": True,
            "USERNAME": "u",
            "PASSWORD": "p",
            "BASE_URL": "https://webdav-1234.pd1.123pan.cn",
            "PATH_PREFIX": "/dav",
        },
    })

    config = ConfigManager(path).get_webdav_config()

    assert config["webdav_user"] == "u"
    assert config["webdav_host"] == "webdav-1234.pd1.123pan.cn"
    assert config["webdav_path_prefix"] == "/dav"
    assert config["webdav_enabled"] is True


def test_webdav_config_accepts_legacy_keys(tmp_path):
    """config.json.template 早期版本用的是 USER/HOST，继续兼容。"""
    path = write_config(tmp_path, {
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "WEBDAV": {"ENABLED": True, "USER": "u", "PASSWORD": "p",
                   "HOST": "webdav-legacy.pd1.123pan.cn"},
    })

    config = ConfigManager(path).get_webdav_config()

    assert config["webdav_user"] == "u"
    assert config["webdav_host"] == "webdav-legacy.pd1.123pan.cn"
    assert config["webdav_path_prefix"] == "/webdav"


def test_webdav_host_is_extracted_from_full_url(tmp_path):
    path = write_config(tmp_path, {
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "WEBDAV": {"ENABLED": True, "USERNAME": "u", "PASSWORD": "p",
                   "BASE_URL": "https://webdav-1234.pd1.123pan.cn/webdav"},
    })

    assert ConfigManager(path).get_webdav_config()["webdav_host"] == "webdav-1234.pd1.123pan.cn"


def test_enabled_webdav_without_host_raises(tmp_path):
    path = write_config(tmp_path, {
        "CLIENT_ID": "id",
        "CLIENT_SECRET": "secret",
        "WEBDAV": {"ENABLED": True, "USERNAME": "u", "PASSWORD": "p"},
    })

    with pytest.raises(ConfigurationError):
        ConfigManager(path).get_webdav_config()


def test_disabled_webdav_without_host_is_allowed(tmp_path):
    """没启用 WebDAV 时不该因为缺配置就让整个客户端起不来。"""
    path = write_config(tmp_path, {"CLIENT_ID": "id", "CLIENT_SECRET": "secret"})

    config = ConfigManager(path).get_webdav_config()

    assert config["webdav_enabled"] is False
    assert config["webdav_host"] == ""
