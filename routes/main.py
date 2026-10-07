"""
Main routes blueprint - Page routes (index, search, file_detail, demo_webdav)
"""
from flask import Blueprint, render_template, request, flash, redirect, url_for, current_app, jsonify
from api import Pan123APIError
from api.split_archive import SPLIT_7Z
from .utils import get_client, folder_breadcrumbs
from pathlib import PurePosixPath
from routes.zip_browser import TEXT_EXTENSIONS, PREVIEW_TYPES
from .listing import options, directory_snapshot, order_files

main_bp = Blueprint('main', __name__)


@main_bp.route('/')
def index():
    """首页 - 显示根目录文件列表"""
    try:
        client = get_client()
        parent_id = request.args.get('parent_id', 0, type=int)
        limit = request.args.get('limit', 20, type=int)
        last_file_id = request.args.get('last_file_id', type=int)

        limit = min(max(limit, 1), 100)
        refresh = request.args.get('refresh') == '1'
        cache = current_app.extensions['directory_pages']
        key = (parent_id, limit, last_file_id)
        if refresh:
            cache.invalidate_directory(parent_id)
        sort, direction, kind = options(request.args)
        snapshot = None
        next_offset = None
        result = cache.get(key)
        if sort != 'original' or kind != 'all':
            all_files, snapshot = directory_snapshot(client, cache, parent_id, None if refresh else request.args.get('snapshot'))
            ordered = order_files(all_files, sort, direction, kind)
            offset = 0 if refresh else max(0, request.args.get('offset', 0, type=int))
            file_list = ordered[offset:offset + limit]
            next_offset = offset + limit if offset + limit < len(ordered) else None
            next_last_file_id = None
        else:
            if result is None:
                result = client.list_files(parent_id=parent_id, limit=limit, last_file_id=last_file_id)
                cache.put(key, result)
            file_list, next_last_file_id = result
        breadcrumbs = folder_breadcrumbs(client, parent_id, refresh=refresh)

        return render_template('files.html',
                               files=file_list,
                               breadcrumbs=breadcrumbs,
                               parent_id=parent_id,
                               next_last_file_id=next_last_file_id,
                               limit=limit, sort=sort, direction=direction, kind=kind,
                               snapshot=snapshot, next_offset=next_offset)

    except Pan123APIError as e:
        flash(f'API错误: {e}', 'error')
        status = 502 if request.headers.get('X-Requested-With') == 'XMLHttpRequest' else 200
        return render_template('files.html', files=[], parent_id=0), status
    except Exception as e:
        flash(f'未知错误: {e}', 'error')
        status = 502 if request.headers.get('X-Requested-With') == 'XMLHttpRequest' else 200
        return render_template('files.html', files=[], parent_id=0), status


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
        status = 502 if request.headers.get('X-Requested-With') == 'XMLHttpRequest' else 200
        return render_template('search.html', files=[], search_query=search_query), status
    except Exception as e:
        flash(f'搜索时发生错误: {e}', 'error')
        status = 502 if request.headers.get('X-Requested-With') == 'XMLHttpRequest' else 200
        return render_template('search.html', files=[], search_query=search_query), status


@main_bp.route('/file/<int:file_id>')
def file_detail(file_id):
    """查看文件详情"""
    try:
        client = get_client()
        file_info = client.get_file_info_single(file_id, use_cache=True)

        if not file_info:
            flash('文件不存在', 'error')
            return redirect(url_for('main.index'))

        if request.args.get('info') != '1':
            if file_info.is_folder:
                return redirect(url_for('main.index', parent_id=file_id))
            suffix = PurePosixPath(file_info.filename).suffix.lower()
            if suffix in ('.zip', '.7z', '.rar') or SPLIT_7Z.fullmatch(file_info.filename):
                return redirect(url_for('zip.browse', file_id=file_id))
            media = {'.mp4': 'video', '.webm': 'video', '.mov': 'video',
                     '.mp3': 'audio', '.wav': 'audio', '.ogg': 'audio', '.m4a': 'audio'}
            kind = ('image' if PREVIEW_TYPES.get(suffix, '').startswith('image/') else
                    'pdf' if suffix == '.pdf' else 'text' if suffix in TEXT_EXTENSIONS else media.get(suffix))
            if kind:
                return render_template('file_preview.html', file=file_info, kind=kind,
                                       breadcrumbs=folder_breadcrumbs(client, file_info.get('parentFileId', 0)),
                                       gallery_index_url=url_for('main.directory_images', parent_id=file_info.get('parentFileId', 0)))

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


@main_bp.route('/favorites')
def favorites():
    """Browser-local colored favorites; no cloud API request is necessary."""
    return render_template('favorites.html')


@main_bp.route('/api/directory-images')
def directory_images():
    try:
        parent_id = request.args.get('parent_id', 0, type=int)
        sort, direction, _ = options(request.args)
        entries, token = directory_snapshot(get_client(), current_app.extensions['directory_pages'],
                                            parent_id, request.args.get('snapshot'))
        images = [file for file in order_files(entries, sort, direction, 'image') if not file.is_folder]
        offset = max(0, request.args.get('offset', 0, type=int))
        page = images[offset:offset + 100]
        next_url = (url_for('main.directory_images', parent_id=parent_id, snapshot=token,
                            sort=sort, direction=direction, offset=offset + 100)
                    if offset + 100 < len(images) else None)
        return jsonify(items=[dict(id=f.file_id, name=f.filename,
                                   url=url_for('preview.content', file_id=f.file_id)) for f in page], next=next_url)
    except Pan123APIError as exc:
        return jsonify(error=str(exc)), 502
