"""
Main routes blueprint - Page routes (index, search, file_detail, demo_webdav)
"""
from flask import Blueprint, render_template, request, flash, redirect, url_for, current_app
from api import Pan123APIError
from .utils import get_client

main_bp = Blueprint('main', __name__)


@main_bp.route('/')
def index():
    """首页 - 显示根目录文件列表"""
    try:
        client = get_client()
        parent_id = request.args.get('parent_id', 0, type=int)
        limit = request.args.get('limit', 20, type=int)
        last_file_id = request.args.get('last_file_id', type=int)

        file_list, next_last_file_id = client.list_files(
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


@main_bp.route('/search')
def search():
    """搜索文件"""
    search_query = ''
    try:
        client = get_client()
        search_query = request.args.get('q', '').strip()
        search_mode = request.args.get('mode', 0, type=int)
        limit = request.args.get('limit', 20, type=int)
        last_file_id = request.args.get('last_file_id', type=int)

        if not search_query:
            return render_template('search.html', files=[], search_query='')

        file_list, next_last_file_id = client.list_files(
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


@main_bp.route('/file/<int:file_id>')
def file_detail(file_id):
    """查看文件详情"""
    try:
        client = get_client()
        file_info = client.get_file_info_single(file_id, use_cache=True)

        if not file_info:
            flash('文件不存在', 'error')
            return redirect(url_for('main.index'))

        download_url = None
        if not file_info.is_folder:
            try:
                download_info = client.get_download_info(file_id)
                if download_info and 'data' in download_info:
                    download_url = download_info['data'].get('downloadUrl')
            except Pan123APIError:
                pass

        webdav_url = None
        if client.is_webdav_available():
            try:
                webdav_url = client.get_webdav_url(file_id)
            except Exception as e:
                current_app.logger.warning(f"获取WebDAV URL失败: {e}")

        return render_template('file_detail.html',
                               file=file_info,
                               download_url=download_url,
                               webdav_url=webdav_url)

    except Pan123APIError as e:
        flash(f'获取文件详情失败: {e}', 'error')
        return redirect(url_for('main.index'))
    except Exception as e:
        flash(f'查看文件详情时发生错误: {e}', 'error')
        return redirect(url_for('main.index'))


@main_bp.route('/demo/webdav/<int:file_id>')
def demo_webdav(file_id):
    """演示WebDAV功能的页面"""
    try:
        client = get_client()
        file_info = client.get_file_info_single(file_id, use_cache=True)
        if not file_info:
            flash('文件不存在', 'error')
            return redirect(url_for('main.index'))

        webdav_url = None
        if client.is_webdav_available():
            try:
                webdav_url = client.get_webdav_url(file_id)
            except Exception as e:
                current_app.logger.warning(f"获取WebDAV URL失败: {e}")

        return render_template('demo_webdav.html',
                               file=file_info,
                               webdav_url=webdav_url)

    except Exception as e:
        flash(f'加载演示页面时发生错误: {e}', 'error')
        return redirect(url_for('main.index'))
