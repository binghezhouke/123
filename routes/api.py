"""
API routes blueprint - REST API endpoints (/api/*)
"""
from flask import Blueprint, jsonify, request, current_app, g
from api import Pan123APIError

api_bp = Blueprint('api', __name__, url_prefix='/api')


def get_client():
    """Get the Pan123Client instance from the application context"""
    if 'client' not in g:
        from api import Pan123Client
        config = current_app.config['PAN123_CONFIG']
        redis_config = config.get('REDIS', {})

        g.client = Pan123Client(
            redis_host=redis_config.get('HOST', 'localhost'),
            redis_port=redis_config.get('PORT', 6379),
            redis_db=redis_config.get('DB', 0),
            redis_password=redis_config.get('PASSWORD', None),
            enable_cache=redis_config.get('ENABLED', True)
        )
    return g.client


@api_bp.route('/download/<int:file_id>')
def api_download(file_id):
    """API接口：获取文件下载链接"""
    try:
        client = get_client()
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


@api_bp.route('/files/batch')
def api_files_batch():
    """API接口：批量获取文件详情"""
    try:
        client = get_client()
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

        file_list = client.get_files_info(file_ids)

        if file_list and len(file_list) > 0:
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


@api_bp.route('/webdav/redirect/<int:file_id>')
def api_webdav_redirect(file_id):
    """API接口：获取WebDAV重定向后的最终下载URL"""
    try:
        client = get_client()
        redirect_url = client.get_webdav_redirect_url(file_id)

        if redirect_url:
            return jsonify({
                'success': True,
                'redirect_url': redirect_url,
                'file_id': file_id
            })
        else:
            return jsonify({'error': '获取WebDAV重定向URL失败，可能是文件不存在或WebDAV配置问题'}), 404

    except Exception as e:
        return jsonify({'error': f'服务器错误: {e}'}), 500


@api_bp.route('/download/final/<int:file_id>')
def api_final_download(file_id):
    """API接口：获取最终下载URL（优先WebDAV，回退API）"""
    try:
        client = get_client()
        prefer_webdav = request.args.get(
            'prefer_webdav', 'true').lower() == 'true'

        final_url = client.get_final_download_url(
            file_id, prefer_webdav=prefer_webdav)

        if final_url:
            url_type = 'webdav' if 'webdav' in final_url or 'pd1' in final_url else 'api'

            return jsonify({
                'success': True,
                'download_url': final_url,
                'url_type': url_type,
                'file_id': file_id
            })
        else:
            return jsonify({'error': '获取下载URL失败，请检查文件是否存在或重试'}), 404

    except Exception as e:
        return jsonify({'error': f'服务器错误: {e}'}), 500
