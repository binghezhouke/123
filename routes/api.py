"""
API routes blueprint - REST API endpoints (/api/*)
"""
from flask import Blueprint, jsonify, request, session, url_for, current_app
from api import Pan123APIError
from .utils import get_client

api_bp = Blueprint('api', __name__, url_prefix='/api')


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

        result = client.get_final_download_url(
            file_id, prefer_webdav=prefer_webdav)

        if result:
            final_url, url_type = result
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


@api_bp.route('/archive/probe/<int:file_id>', methods=['POST'])
def probe_archive(file_id):
    """Read only the signature, after an explicit same-origin JSON request."""
    from api.archive_probe import detect_archive, remember_kind

    if not request.is_json:
        return jsonify(error='请通过探测按钮发起请求'), 415
    try:
        client = get_client()
        file = client.get_file_info_single(file_id)
        if not file or file.is_folder:
            return jsonify(error='请选择一个文件'), 404
        kind = None
        if file.size:
            result = client.get_final_download_url(file_id, prefer_webdav=False)
            if not result:
                return jsonify(error='无法获取下载链接，请重试'), 502
            kind = detect_archive(result[0])
        remember_kind(session, file, kind)
        if kind:
            current_app.extensions['archive_cache'].invalidate(
                (file_id, kind, file.get('etag'), file.get('size'), file.get('updateAt')))
        response = jsonify(
            kind=kind,
            browse_url=url_for('zip.browse', file_id=file_id) if kind else None,
            message=(f'文件头识别为 {kind[1:].upper()}，可尝试浏览压缩包。'
                     if kind else '未识别为 ZIP、7z 或 RAR；可能是其他格式或非首分卷。'),
        )
        response.headers['Cache-Control'] = 'no-store'
        return response
    except Exception:
        # Upstream exceptions may contain a signed download URL.
        return jsonify(error='探测失败：无法按范围读取文件头，请稍后重试'), 502
