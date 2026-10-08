#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
123云盘文件浏览器 - Flask Web应用 (应用工厂模式)
"""

import json
import logging
import os
import atexit
from threading import Lock
from flask import Flask, render_template
from routes import main_bp, api_bp
from routes.zip_browser import zip_bp
from routes.browser_cache import PageCache
from api.archive_cache import ArchiveCache
from api.archive_passwords import ArchivePasswordVault
from api.archive_jobs import ArchivePreparationJobs
from routes.preview import preview_bp
from routes.nested_archive import nested_archive_bp
from routes.batch_passwords import batch_passwords_bp


def load_config(config_path='config.json'):
    """加载配置文件"""
    if not os.path.exists(config_path):
        raise FileNotFoundError(f"配置文件 {config_path} 不存在")

    with open(config_path, 'r', encoding='utf-8') as f:
        return json.load(f)


def create_app(config_path='config.json'):
    """
    Flask应用工厂函数

    :param config_path: 配置文件路径
    :return: Flask应用实例
    """
    # 加载配置
    config = load_config(config_path)

    # 创建Flask应用
    app = Flask(__name__)

    # 从配置读取SECRET_KEY
    app.secret_key = config.get('SECRET_KEY', 'dev-secret-key-change-in-production')
    app.config['SESSION_COOKIE_SAMESITE'] = 'Lax'

    # 存储完整配置供蓝图使用
    app.config['PAN123_CONFIG'] = config
    for key in ('ARCHIVE_PREP_TIMEOUT_SECONDS', 'ARCHIVE_PREP_MAX_MEMBER_BYTES'):
        if key in config:
            app.config[key] = config[key]
    app.extensions['directory_pages'] = PageCache()
    app.extensions['archive_cache'] = ArchiveCache()
    app.extensions['archive_passwords'] = ArchivePasswordVault()
    app.extensions['batch_password_locks'] = [Lock() for _ in range(32)]
    archive_preparation_jobs = ArchivePreparationJobs(
        root=config.get('ARCHIVE_PREP_CACHE_DIR'),
        max_bytes=int(config.get('ARCHIVE_PREP_CACHE_BYTES', 64 * 1024**3)),
        ttl=int(config.get('ARCHIVE_PREP_CACHE_TTL_SECONDS', 6 * 3600)),
    )
    app.extensions['archive_preparation_jobs'] = archive_preparation_jobs
    atexit.register(archive_preparation_jobs.close)

    # 注册蓝图
    app.register_blueprint(main_bp)
    app.register_blueprint(api_bp)
    app.register_blueprint(zip_bp)
    app.register_blueprint(preview_bp)
    app.register_blueprint(nested_archive_bp)
    app.register_blueprint(batch_passwords_bp)

    # 配置日志
    logging.basicConfig(
        level=logging.INFO,
        format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
    )
    app.logger.setLevel(logging.INFO)

    # 错误处理器
    @app.errorhandler(404)
    def not_found(error):
        return render_template('error.html', error="页面不存在"), 404

    @app.errorhandler(500)
    def internal_error(error):
        return render_template('error.html', error="服务器内部错误"), 500

    # 请求结束时清理资源
    @app.teardown_appcontext
    def teardown_client(exception):
        """在请求结束时清理Pan123Client资源"""
        from flask import g
        client = g.pop('client', None)
        if client is not None:
            try:
                client.__exit__(None, None, None)
            except Exception as e:
                app.logger.warning(f"关闭client session时出错: {e}")

    return app


if __name__ == '__main__':
    print("123云盘文件浏览器启动中...")

    try:
        app = create_app()
        app.logger.info("✓ Flask应用创建成功")
    except FileNotFoundError as e:
        print(f"错误: {e}")
        print("请确保config.json配置文件存在")
        exit(1)
    except Exception as e:
        print(f"✗ 应用初始化失败: {e}")
        exit(1)

    # 监听地址/端口/调试开关都可以用环境变量覆盖
    debug = os.environ.get('FLASK_DEBUG', '').lower() in ('1', 'true', 'yes')
    host = os.environ.get('FLASK_HOST', '0.0.0.0')
    port = int(os.environ.get('FLASK_PORT', '8081'))

    print("启动Flask服务器...")
    print(f"访问地址: http://{'localhost' if host == '0.0.0.0' else host}:{port}")
    if debug:
        print("⚠ 调试模式已开启，Werkzeug 调试器可以执行任意代码，只能在本机临时使用")

    # 注意：本应用没有任何登录鉴权，监听 0.0.0.0 等于把云盘内容开放给同网段所有人。
    # 需要长期运行时请放在反向代理后面并加上认证，或把 FLASK_HOST 设为 127.0.0.1。
    app.run(debug=debug, host=host, port=port)
