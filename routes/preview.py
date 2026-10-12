"""Inline file content from API download links, preserving media range requests."""

from pathlib import PurePosixPath
from urllib.parse import quote

import requests
from flask import Blueprint, Response, abort, render_template, request, stream_with_context

from api import Pan123APIError
from .utils import get_client
from .zip_browser import PREVIEW_TYPES, TEXT_EXTENSIONS

preview_bp = Blueprint("preview", __name__)
MEDIA_TYPES = {
    ".mp4": "video/mp4",
    ".webm": "video/webm",
    ".mov": "video/quicktime",
    ".mp3": "audio/mpeg",
    ".wav": "audio/wav",
    ".ogg": "audio/ogg",
    ".m4a": "audio/mp4",
}


@preview_bp.route("/file/<int:file_id>/content")
def content(file_id):
    upstream = None
    try:
        client = get_client()
        file = client.get_file_info_single(file_id, use_cache=True)
        if not file or file.is_folder:
            abort(404)
        result = client.get_final_download_url(file_id, prefer_webdav=False)
        if not result:
            return render_template("error.html", error="获取下载地址失败"), 502
        suffix = PurePosixPath(file.filename).suffix.lower()
        download = request.args.get("download") == "1"
        text = suffix in TEXT_EXTENSIONS and not download
        headers = {"Accept-Encoding": "identity"}
        if request.headers.get("Range") and not text:
            headers["Range"] = request.headers["Range"]
        upstream = requests.get(result[0], headers=headers, stream=True, timeout=(5, 30))
        if upstream.status_code not in (200, 206):
            status = upstream.status_code
            content_range = upstream.headers.get("Content-Range")
            upstream.close()
            if status == 416:
                return Response(status=416, headers={"Content-Range": content_range} if content_range else {})
            return render_template("error.html", error="文件读取失败，请刷新重试"), 502
        mimetype = PREVIEW_TYPES.get(suffix) or MEDIA_TYPES.get(suffix)
        if text:
            with upstream:
                data = bytearray()
                for chunk in upstream.iter_content(65536):
                    data.extend(chunk)
                    if len(data) > 2 * 1024 * 1024:
                        return Response(
                            "文本超过 2 MiB，请下载后查看", status=413, content_type="text/plain; charset=utf-8"
                        )
            try:
                body = data.decode("utf-8-sig")
            except UnicodeDecodeError:
                body = data.decode("gb18030", errors="replace")
            response = Response(body, content_type="text/plain; charset=utf-8")
        else:

            def chunks():
                try:
                    yield from upstream.iter_content(256 * 1024)
                finally:
                    upstream.close()

            response = Response(
                stream_with_context(chunks()),
                status=upstream.status_code,
                content_type=mimetype or "application/octet-stream",
            )
            response.call_on_close(upstream.close)
            for name in ("Content-Length", "Content-Range", "Accept-Ranges"):
                if name in upstream.headers and upstream.headers.get("Content-Encoding", "identity") == "identity":
                    response.headers[name] = upstream.headers[name]
        disposition = "attachment" if download or not (mimetype or text) else "inline"
        response.headers["Content-Disposition"] = f"{disposition}; filename*=UTF-8''{quote(file.filename, safe='')}"
        response.headers["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["Cache-Control"] = "private, no-store"
        return response
    except (requests.RequestException, Pan123APIError):
        if upstream is not None:
            upstream.close()
        return render_template("error.html", error="读取文件失败，请稍后重试"), 502
