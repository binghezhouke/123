"""Read-only archive pages. Member IDs are central-directory positions, not disk paths."""

import io
import zipfile
import zlib
from time import perf_counter
from pathlib import PurePosixPath

import requests
import secrets
from flask import (
    Blueprint,
    abort,
    render_template,
    request,
    send_file,
    current_app,
    session,
    redirect,
    url_for,
    make_response,
    jsonify,
    flash,
)

from api import Pan123APIError
from api.zip_media import VIDEO_TYPES, can_stream, locate_member, can_inflate_in_browser, locate_deflate_member
from .zip_media import video_response
from api.archive_probe import detected_kind
from api.archive_names import member_name, decode_archive_text
from api.split_archive import SPLIT_7Z, discover_volumes
from api.zip_preview import ZipPreviewError, ChangedArchive, ArchivePasswordRequired
from api.archive_preview import read_archive_member, read_archive_member_file
from api.zip_password_validation import validate_archive_password
from .utils import get_client, folder_breadcrumbs
from .listing import options, natural_key

zip_bp = Blueprint("zip", __name__)
TEXT_EXTENSIONS = {
    ".txt",
    ".md",
    ".log",
    ".json",
    ".csv",
    ".xml",
    ".html",
    ".css",
    ".js",
    ".py",
    ".yaml",
    ".yml",
    ".ini",
    ".toml",
    ".sh",
    ".sql",
    ".svg",
}
PREVIEW_TYPES = {
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
    ".pdf": "application/pdf",
}


def no_store(response):
    response = make_response(response)
    response.headers["Cache-Control"] = "no-store"
    return response


@zip_bp.route("/file/<int:file_id>/zip", methods=["GET", "POST"])
@zip_bp.route("/file/<int:file_id>/zip/member/<int:member_id>", methods=["GET", "POST"])
def browse(file_id, member_id=None):
    file = split = kind = None
    try:
        client = get_client()
        refresh = request.args.get("refresh") == "1"
        if refresh and hasattr(client, "clear_file_cache"):
            client.clear_file_cache(file_id)
        file = client.get_file_info_single(file_id, use_cache=not refresh and request.method != "POST")
        if not file:
            abort(404)
        split = SPLIT_7Z.fullmatch(file.filename)
        kind = ".7z" if split else (detected_kind(session, file) or PurePosixPath(file.filename).suffix.lower())
        if file.is_folder or kind not in (".zip", ".7z", ".rar"):
            raise ZipPreviewError("请选择 ZIP、7z 或 RAR 文件")
        can_save_password = kind in (".zip", ".7z", ".rar")

        def render_archive(archive, source):
            if request.args.get("v") and request.args["v"] != source.index_version:
                raise ChangedArchive("压缩包目录已变化，请刷新目录后重新选择文件")
            entries = archive.infolist()
            if member_id is not None:
                if member_id >= len(entries):
                    abort(404)
                entry = entries[member_id]
                if request.args.get("expected_path") is not None and request.args["expected_path"] != member_name(entry):
                    raise ChangedArchive("收藏指向的文件已变化，请返回目录重新定位")
                filename = member_name(entry).rsplit("/", 1)[-1]
                suffix = PurePosixPath(filename).suffix.lower()
                download = request.args.get("download") == "1"
                if request.args.get("client_video") == "1" or request.args.get("raw_deflate") == "1":
                    if not request.args.get("v"):
                        raise ChangedArchive("请从压缩包目录重新打开视频，以获取当前索引版本")
                    location = locate_deflate_member(archive, source, entry, kind, suffix)
                    if request.args.get("raw_deflate") == "1":
                        return video_response(location, filename, "application/octet-stream", allow_range=False)
                    return jsonify(
                        key=f"{file_id}:{source.index_version}:{member_id}",
                        raw_url=url_for(
                            "zip.browse", file_id=file_id, member_id=member_id, raw_deflate=1, v=source.index_version
                        ),
                        filename=filename,
                        size=entry.file_size,
                        compressed_size=entry.compress_size,
                        crc32=entry.CRC,
                        mimetype="video/mp4",
                    )
                if suffix in VIDEO_TYPES and can_stream(entry, kind):
                    location = locate_member(archive, source, entry)
                    if request.args.get("stream") == "1" or download:
                        return video_response(location, filename, VIDEO_TYPES[suffix], download)
                    parent_path = member_name(entry).rpartition("/")[0]
                    video_sort, video_direction, _ = options(request.args)
                    peers = [(i, item) for i, item in enumerate(entries)
                             if member_name(item).rpartition('/')[0] == parent_path
                             and PurePosixPath(member_name(item)).suffix.lower() in VIDEO_TYPES
                             and can_stream(item, kind)]
                    if video_sort != 'original':
                        peers.sort(key=lambda pair: ((pair[1].file_size if video_sort == 'size' else natural_key(member_name(pair[1]))),
                                                     natural_key(member_name(pair[1])), pair[0]), reverse=video_direction == 'desc')
                    position = next(i for i, pair in enumerate(peers) if pair[0] == member_id)
                    def peer_url(offset):
                        target = position + offset
                        return url_for('zip.browse', file_id=file_id, member_id=peers[target][0], v=source.index_version,
                                       sort=video_sort, direction=video_direction) if 0 <= target < len(peers) else None
                    return render_template(
                        "zip_video.html",
                        file=file,
                        filename=filename,
                        previous_video=peer_url(-1), next_video=peer_url(1),
                        source_url=url_for(
                            "zip.browse", file_id=file_id, member_id=member_id, stream=1, v=source.index_version
                        ),
                        back_url=url_for("zip.browse", file_id=file_id, path=parent_path),
                        download_url=url_for(
                            "zip.browse", file_id=file_id, member_id=member_id, download=1, v=source.index_version
                        ),
                        breadcrumbs=folder_breadcrumbs(client, file.get("parentFileId", 0)),
                    )
                if request.args.get("stream") == "1":
                    raise ZipPreviewError("当前仅支持 ZIP 中未加密、仅打包（Store）的视频直接播放")
                extraction_started = perf_counter()
                mimetype = PREVIEW_TYPES.get(suffix)
                if mimetype and mimetype.startswith("image/"):
                    data = read_archive_member_file(archive, source, entry)
                else:
                    data = read_archive_member(archive, source, entry)
                if suffix in TEXT_EXTENSIONS and not download:
                    data = decode_archive_text(data).encode("utf-8")
                    mimetype = "text/plain; charset=utf-8"
                try:
                    response = send_file(
                        data if hasattr(data, "read") else io.BytesIO(data),
                        mimetype=mimetype or "application/octet-stream",
                        as_attachment=download or not mimetype,
                        download_name=filename,
                        conditional=False,
                        max_age=0,
                    )
                except BaseException:
                    if hasattr(data, "close"):
                        data.close()
                    raise
                if hasattr(data, "close"):
                    response.call_on_close(data.close)
                response.headers["Server-Timing"] = f"extract;dur={(perf_counter() - extraction_started) * 1000:.1f}"
                response.headers["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
                response.headers["X-Content-Type-Options"] = "nosniff"
                response.headers["Cache-Control"] = "no-store"
                return response

            prefix = request.args.get("path", "")
            if prefix and not prefix.endswith("/"):
                prefix += "/"
            folders, files = set(), []
            for index, entry in enumerate(entries):
                name = member_name(entry)
                if not name.startswith(prefix):
                    continue
                remainder = name[len(prefix) :]
                if not remainder:
                    continue
                if "/" in remainder:
                    folders.add(remainder.split("/", 1)[0])
                elif not entry.is_dir():
                    suffix = PurePosixPath(name).suffix.lower()
                    split_member = SPLIT_7Z.fullmatch(name)
                    files.append(
                        {
                            "id": index,
                            "name": remainder,
                            "member_path": name,
                            "nested": suffix in (".zip", ".7z", ".7zz", ".rar") or bool(split_member and split_member[2] == "001"),
                            "size": entry.file_size,
                            "encrypted": bool(entry.flag_bits & 1),
                            "image": PREVIEW_TYPES.get(suffix, "").startswith("image/"),
                            "video": suffix in VIDEO_TYPES,
                            "client_video": can_inflate_in_browser(entry, kind, suffix),
                            "streamable": suffix in VIDEO_TYPES and can_stream(entry, kind),
                            "preview": suffix in TEXT_EXTENSIONS or suffix in PREVIEW_TYPES,
                        }
                    )
            if prefix and not folders and not files and not any(member_name(e) == prefix for e in entries):
                abort(404)
            sort, direction, media_kind = options(request.args)
            if media_kind != 'all':
                files = [entry for entry in files if entry[media_kind]]
            if sort != 'original':
                files.sort(key=lambda entry: ((entry['size'] if sort == 'size' else natural_key(entry['name'])),
                                              natural_key(entry['name']), entry['id']), reverse=direction == 'desc')
            parent = prefix.rstrip("/").rsplit("/", 1)[0] + "/" if "/" in prefix.rstrip("/") else ""
            return render_template(
                "zip_browser.html",
                file=file,
                prefix=prefix,
                parent=parent,
                folders=sorted(folders, key=natural_key, reverse=direction == "desc"),
                sort=sort, direction=direction, kind=media_kind,
                files=files,
                total=len(entries),
                solid=getattr(archive, "solid", False),
                password_set=bool(password),
                csrf_token=csrf,
                password_required=False,
                can_save_password=can_save_password,
                password_filename=(split[1] if split else file.filename) + ".pwd",
                member_id=None,
                has_encrypted=any(bool(entry.flag_bits & 1) for entry in entries),
                archive_version=source.index_version,
                breadcrumbs=folder_breadcrumbs(client, file.get("parentFileId", 0)),
                archive_crumbs=[
                    {"name": part, "path": "/".join(prefix.rstrip("/").split("/")[: i + 1]) + "/"}
                    for i, part in enumerate(prefix.rstrip("/").split("/"))
                    if part
                ],
            )

        # Metadata comes from the account-scoped client; cache lives only in this app.
        key = (
            ("split", file.get("parentFileId", 0), split[1])
            if split
            else (file_id, kind, file.get("etag"), file.get("size"), file.get("updateAt"))
        )
        browser_token = session.setdefault("archive_session", secrets.token_urlsafe(32))
        csrf = session.setdefault("archive_csrf", secrets.token_urlsafe(32))
        password_vault = current_app.extensions["archive_passwords"]
        if request.method == "POST":
            import hmac

            submitted_csrf = request.form.get("csrf_token", "").encode("utf-8")
            if not hmac.compare_digest(submitted_csrf, csrf.encode("utf-8")):
                return no_store(render_template("error.html", error="请求校验失败，请刷新页面后重试")), 400
            action = request.form.get("action")
            if action == "clear":
                password_vault.clear(browser_token, key)
            elif action in ("password", "save_password"):
                password = (request.form.get("password", "") if action == "password"
                            else password_vault.get(browser_token, key))
                if not password:
                    return no_store(render_template("error.html", error="请输入压缩包密码")), 400
                if len(password) > 1024:
                    return no_store(render_template("error.html", error="压缩包密码不能超过 1024 个字符")), 400
                save_password = action == "save_password" or request.form.get("save_password") == "1"
                if save_password:
                    if not can_save_password:
                        return no_store(render_template("error.html", error="此压缩格式暂不支持保存密码文件")), 400
                    current_app.extensions["archive_cache"].run(
                        key, kind,
                        lambda: ((discover_volumes(client, file), "split") if split
                                 else client.get_final_download_url(file_id, prefer_webdav=False)),
                        validate_archive_password, password=password,
                        resolve_part=lambda part_id: client.get_final_download_url(part_id, prefer_webdav=False),
                    )
                password_vault.set(browser_token, key, password)
                if save_password:
                    try:
                        client.save_archive_password(file_id, password, archive_kind=kind)
                    except (Pan123APIError, requests.RequestException, OSError):
                        current_app.logger.warning("压缩包密码文件保存失败，文件 ID: %s", file_id)
                        flash("密码已验证，但保存到网盘失败。仍可在网页浏览；可点击“保存密码到网盘”重试。", "error")
                    else:
                        flash(f"已保存同级密码文件 {split[1] if split else file.filename}.pwd。", "success")
                    return no_store(redirect(url_for("zip.browse", file_id=file_id,
                                                     path=request.form.get("path", ""))))
            else:
                return no_store(render_template("error.html", error="无效的密码操作")), 400
            target_member = request.form.get("member_id", "")
            target_path = request.form.get("path", "")
            if target_member.isdigit():
                return no_store(
                    redirect(
                        url_for(
                            "zip.browse",
                            file_id=file_id,
                            member_id=int(target_member),
                            v=request.form.get("v", ""),
                            path=target_path,
                            expected_path=request.form.get("expected_path"),
                            download=request.form.get("download", ""),
                        )
                    )
                )
            return no_store(redirect(url_for("zip.browse", file_id=file_id, path=target_path)))
        password = password_vault.get(browser_token, key)
        resolve_password = getattr(client, "resolve_archive_password", None)
        if not password and callable(resolve_password):
            try:
                resolved = resolve_password(file)
            except (Pan123APIError, requests.RequestException, OSError):
                current_app.logger.warning("读取压缩包密码侧车失败，文件 ID: %s", file_id, exc_info=True)
                resolved = None
            if resolved:
                password = resolved
                password_vault.set(browser_token, key, password)
        response = current_app.extensions["archive_cache"].run(
            key,
            kind,
            lambda: (
                (discover_volumes(client, file), "split")
                if split
                else client.get_final_download_url(file_id, prefer_webdav=False)
            ),
            render_archive,
            refresh=refresh,
            password=password,
            resolve_part=lambda part_id: client.get_final_download_url(part_id, prefer_webdav=False),
        )
        return no_store(response)
    except ArchivePasswordRequired as exc:
        if file is None:
            raise
        browser_token = session.setdefault("archive_session", secrets.token_urlsafe(32))
        csrf = session.setdefault("archive_csrf", secrets.token_urlsafe(32))
        key = (
            ("split", file.get("parentFileId", 0), split[1])
            if split
            else (file_id, kind, file.get("etag"), file.get("size"), file.get("updateAt"))
        )
        prefix = request.args.get("path", "")
        if prefix and not prefix.endswith("/"):
            prefix += "/"
        archive_crumbs = [
            {"name": part, "path": "/".join(prefix.rstrip("/").split("/")[: i + 1]) + "/"}
            for i, part in enumerate(prefix.rstrip("/").split("/"))
            if part
        ]
        return no_store(
            render_template(
                "zip_browser.html",
                file=file,
                prefix=prefix,
                parent=prefix.rstrip("/").rsplit("/", 1)[0] + "/" if "/" in prefix.rstrip("/") else "",
                folders=[],
                files=[],
                total=0,
                solid=False,
                archive_version=request.args.get("v", ""),
                breadcrumbs=folder_breadcrumbs(client, file.get("parentFileId", 0)),
                archive_crumbs=archive_crumbs,
                password_required=True,
                can_save_password=kind in (".zip", ".7z", ".rar"),
                password_filename=(split[1] if split else file.filename) + ".pwd",
                password_set=bool(current_app.extensions["archive_passwords"].get(browser_token, key)),
                has_encrypted=True,
                csrf_token=csrf,
                password_error=str(exc),
                member_id=member_id,
            )
        ), 401
    except ChangedArchive as exc:
        return no_store(render_template("archive_changed.html", error=str(exc), file_id=file_id)), 400
    except (ZipPreviewError, zipfile.BadZipFile, NotImplementedError, RuntimeError, zlib.error, EOFError) as exc:
        return no_store(render_template("error.html", error=f"压缩包无法打开：{exc}")), 400
    except (requests.RequestException, Pan123APIError):
        return no_store(render_template("error.html", error="读取压缩包失败，请稍后重试或重新获取下载地址")), 502
    except OSError:
        return no_store(render_template("error.html", error="图片暂存失败，请检查服务器磁盘空间后重试")), 507
