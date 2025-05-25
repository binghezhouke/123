#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
123云盘文件浏览器 - Flask Web应用
"""

from flask import Flask, render_template, request, jsonify, redirect, url_for, flash
import os
import json
from api import Pan123Client, Pan123APIError
from api.models import File, FileList

app = Flask(__name__)
app.secret_key = 'your-secret-key-change-this'  # 请更改为随机密钥

# 全局变量存储客户端实例
client = None


def format_file_size(size_bytes):
    """格式化文件大小"""
    if size_bytes >= 1024 * 1024 * 1024:  # GB
        return f"{size_bytes / (1024 * 1024 * 1024):.2f} GB"
    elif size_bytes >= 1024 * 1024:  # MB
        return f"{size_bytes / (1024 * 1024):.2f} MB"
    elif size_bytes >= 1024:  # KB
        return f"{size_bytes / 1024:.2f} KB"
    else:
        return f"{size_bytes} 字节"


def get_category_name(category):
    """获取文件分类名称"""
    category_map = {0: "未知", 1: "音频", 2: "视频", 3: "图片"}
    return category_map.get(category, "未知")


def get_file_icon(filename, file_type, category):
    """根据文件类型返回图标类名"""
    if file_type == 1:  # 文件夹
        return "fas fa-folder"

    # 根据分类
    if category == 1:  # 音频
        return "fas fa-file-audio"
    elif category == 2:  # 视频
        return "fas fa-file-video"
    elif category == 3:  # 图片
        return "fas fa-file-image"

    # 根据文件扩展名
    ext = os.path.splitext(filename)[1].lower()
    if ext in ['.txt', '.md', '.log']:
        return "fas fa-file-alt"
    elif ext in ['.pdf']:
        return "fas fa-file-pdf"
    elif ext in ['.doc', '.docx']:
        return "fas fa-file-word"
    elif ext in ['.xls', '.xlsx']:
        return "fas fa-file-excel"
    elif ext in ['.ppt', '.pptx']:
        return "fas fa-file-powerpoint"
    elif ext in ['.zip', '.rar', '.7z']:
        return "fas fa-file-archive"
    else:
        return "fas fa-file"


@app.route('/')
def index():
    """首页 - 显示根目录文件列表"""
    try:
        if not client:
            flash('API客户端未初始化', 'error')
            return render_template('error.html', error="API客户端未初始化")

        parent_id = request.args.get('parent_id', 0, type=int)
        limit = request.args.get('limit', 20, type=int)
        last_file_id = request.args.get('last_file_id', type=int)

        # 获取文件列表，返回FileList对象
        file_list, next_last_file_id = client.list_files_as_objects(
            parent_id=parent_id,
            limit=limit,
            last_file_id=last_file_id
        )

        return render_template('files.html',
                               files=file_list,
                               parent_id=parent_id,
                               next_last_file_id=next_last_file_id,
                               limit=limit)

    except Pan123APIError as e:
        flash(f'API错误: {e}', 'error')
        return render_template('files.html', files=[], parent_id=0)
    except Exception as e:
        flash(f'未知错误: {e}', 'error')
        return render_template('files.html', files=[], parent_id=0)


@app.route('/search')
def search():
    """搜索文件"""
    try:
        if not client:
            flash('API客户端未初始化', 'error')
            return render_template('search.html', files=[])

        search_query = request.args.get('q', '').strip()
        search_mode = request.args.get('mode', 0, type=int)
        limit = request.args.get('limit', 20, type=int)
        last_file_id = request.args.get('last_file_id', type=int)

        if not search_query:
            return render_template('search.html', files=[], search_query='')

        # 执行搜索，返回FileList对象
        file_list, next_last_file_id = client.list_files_as_objects(
            search_data=search_query,
            search_mode=search_mode,
            limit=limit,
            last_file_id=last_file_id
        )

        return render_template('search.html',
                               files=file_list,
                               search_query=search_query,
                               search_mode=search_mode,
                               next_last_file_id=next_last_file_id,
                               limit=limit)

    except Pan123APIError as e:
        flash(f'搜索失败: {e}', 'error')
        return render_template('search.html', files=[], search_query=search_query)
    except Exception as e:
        flash(f'搜索时发生错误: {e}', 'error')
        return render_template('search.html', files=[], search_query=search_query)


@app.route('/file/<int:file_id>')
def file_detail(file_id):
    """查看文件详情"""
    try:
        if not client:
            flash('API客户端未初始化', 'error')
            return redirect(url_for('index'))

        # 使用API客户端的缓存功能获取文件详情
        file_info = client.get_file_info_single(file_id, use_cache=True)

        if not file_info:
            flash('文件不存在', 'error')
            return redirect(url_for('index'))

        # 如果是文件（非文件夹），尝试获取下载链接
        download_url = None
        if not file_info.is_folder:  # 不是文件夹
            try:
                download_info = client.get_download_info(file_id)
                if download_info and 'data' in download_info:
                    download_url = download_info['data'].get('downloadUrl')
            except Pan123APIError:
                pass  # 忽略下载链接获取失败

        return render_template('file_detail.html',
                               file=file_info,
                               download_url=download_url)

    except Pan123APIError as e:
        flash(f'获取文件详情失败: {e}', 'error')
        return redirect(url_for('index'))
    except Exception as e:
        flash(f'查看文件详情时发生错误: {e}', 'error')
        return redirect(url_for('index'))


@app.route('/api/download/<int:file_id>')
def api_download(file_id):
    """API接口：获取文件下载链接"""
    try:
        if not client:
            return jsonify({'error': 'API客户端未初始化'}), 500

        download_info = client.get_download_info(file_id)

        if download_info and 'data' in download_info and 'downloadUrl' in download_info['data']:
            return jsonify({
                'success': True,
                'download_url': download_info['data']['downloadUrl']
            })
        else:
            return jsonify({'error': '获取下载链接失败'}), 404

    except Pan123APIError as e:
        return jsonify({'error': str(e)}), 400
    except Exception as e:
        return jsonify({'error': f'服务器错误: {e}'}), 500


@app.route('/api/files/batch')
def api_files_batch():
    """API接口：批量获取文件详情"""
    try:
        if not client:
            return jsonify({'error': 'API客户端未初始化'}), 500

        file_ids_str = request.args.get('ids', '')
        if not file_ids_str:
            return jsonify({'error': '缺少文件ID参数'}), 400

        try:
            file_ids = [int(id.strip())
                        for id in file_ids_str.split(',') if id.strip()]
        except ValueError:
            return jsonify({'error': '文件ID格式错误'}), 400

        if not file_ids:
            return jsonify({'error': '没有有效的文件ID'}), 400

        # 使用FileList对象
        file_list = client.get_files_info_as_objects(file_ids)

        if file_list and len(file_list) > 0:
            # 转换为字典列表以保持API兼容性
            return jsonify({
                'success': True,
                'files': file_list.to_dict_list()
            })
        else:
            return jsonify({'error': '获取文件详情失败'}), 404

    except Pan123APIError as e:
        return jsonify({'error': str(e)}), 400
    except Exception as e:
        return jsonify({'error': f'服务器错误: {e}'}), 500


@app.errorhandler(404)
def not_found(error):
    return render_template('error.html', error="页面不存在"), 404


@app.errorhandler(500)
def internal_error(error):
    return render_template('error.html', error="服务器内部错误"), 500


def init_client():
    """初始化API客户端"""
    global client
    try:
        # 初始化API客户端，启用Redis缓存
        client = Pan123Client(
            redis_host='192.168.2.254',
            redis_port=6379,
            redis_db=0,
            redis_password=None,
            enable_cache=True
        )
        print("✓ 123云盘API客户端初始化成功")
        return True
    except Exception as e:
        print(f"✗ 123云盘API客户端初始化失败: {e}")
        return False


if __name__ == '__main__':
    print("123云盘文件浏览器启动中...")

    # 初始化API客户端
    if not init_client():
        print("错误: 无法初始化API客户端，请检查config.json配置")
        exit(1)

    print("启动Flask服务器...")
    print("访问地址: http://localhost:8080")

    # 启动Flask应用
    app.run(debug=True, host='0.0.0.0', port=8080)
