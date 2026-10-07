"""Read-only archive pages. Member IDs are central-directory positions, not disk paths."""

import io
import zipfile
import zlib
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
)

from api import Pan123APIError
from api.split_archive import SPLIT_7Z, discover_volumes
from api.zip_preview import ZipPreviewError, ChangedArchive, ArchivePasswordRequired
from api.archive_preview import read_archive_member
from .utils import get_client, folder_breadcrumbs

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
        file = client.get_file_info_single(file_id, use_cache=not refresh)
        if not file:
            abort(404)
        split = SPLIT_7Z.fullmatch(file.filename)
        kind = ".7z" if split else PurePosixPath(file.filename).suffix.lower()
        if file.is_folder or kind not in (".zip", ".7z", ".rar"):
            raise ZipPreviewError("请选择 ZIP、7z 或 RAR 文件")

        def render_archive(archive, source):
            if request.args.get("v") and request.args["v"] != source.index_version:
                raise ChangedArchive("压缩包目录已变化，请刷新目录后重新选择文件")
            entries = archive.infolist()
            if member_id is not None:
                if member_id >= len(entries):
                    abort(404)
                entry = entries[member_id]
                data = read_archive_member(archive, source, entry)
                filename = entry.filename.replace("\\", "/").rsplit("/", 1)[-1]
                suffix = PurePosixPath(filename).suffix.lower()
                download = request.args.get("download") == "1"
                mimetype = PREVIEW_TYPES.get(suffix)
                if suffix in TEXT_EXTENSIONS and not download:
                    try:
                        data = data.decode("utf-8-sig").encode("utf-8")
                    except UnicodeDecodeError:
                        data = data.decode("gb18030", errors="replace").encode("utf-8")
                    mimetype = "text/plain; charset=utf-8"
                response = send_file(
                    io.BytesIO(data),
                    mimetype=mimetype or "application/octet-stream",
                    as_attachment=download or not mimetype,
                    download_name=filename,
                    conditional=False,
                    max_age=0,
                )
                response.headers["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
                response.headers["X-Content-Type-Options"] = "nosniff"
                response.headers["Cache-Control"] = "no-store"
                return response

            prefix = request.args.get("path", "")
            if prefix and not prefix.endswith("/"):
                prefix += "/"
            folders, files = set(), []
            for index, entry in enumerate(entries):
                name = entry.filename
                if not name.startswith(prefix):
                    continue
                remainder = name[len(prefix) :]
                if not remainder:
                    continue
                if "/" in remainder:
                    folders.add(remainder.split("/", 1)[0])
                elif not entry.is_dir():
                    suffix = PurePosixPath(name).suffix.lower()
                    files.append(
                        {
                            "id": index,
                            "name": remainder,
                            "size": entry.file_size,
                            "encrypted": bool(entry.flag_bits & 1),
                            "image": PREVIEW_TYPES.get(suffix, "").startswith("image/"),
                            "preview": suffix in TEXT_EXTENSIONS or suffix in PREVIEW_TYPES,
                        }
                    )
            if prefix and not folders and not files and not any(e.filename == prefix for e in entries):
                abort(404)
            parent = prefix.rstrip("/").rsplit("/", 1)[0] + "/" if "/" in prefix.rstrip("/") else ""
            return render_template(
                "zip_browser.html",
                file=file,
                prefix=prefix,
                parent=parent,
                folders=sorted(folders),
                files=files,
                total=len(entries),
                solid=getattr(archive, "solid", False),
                password_set=bool(password),
                csrf_token=csrf,
                password_required=False,
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
            elif action == "password":
                password = request.form.get("password", "")
                if not password:
                    return no_store(render_template("error.html", error="请输入压缩包密码")), 400
                if len(password) > 1024:
                    return no_store(render_template("error.html", error="压缩包密码不能超过 1024 个字符")), 400
                password_vault.set(browser_token, key, password)
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
                            download=request.form.get("download", ""),
                        )
                    )
                )
            return no_store(redirect(url_for("zip.browse", file_id=file_id, path=target_path)))
        password = password_vault.get(browser_token, key)
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
                password_set=bool(current_app.extensions["archive_passwords"].get(browser_token, key)),
                has_encrypted=True,
                csrf_token=csrf,
                password_error=str(exc),
                member_id=member_id,
            )
        ), 401
    except (ZipPreviewError, zipfile.BadZipFile, NotImplementedError, RuntimeError, zlib.error, EOFError) as exc:
        return no_store(render_template("error.html", error=f"压缩包无法打开：{exc}")), 400
    except (requests.RequestException, Pan123APIError):
        return no_store(render_template("error.html", error="读取压缩包失败，请稍后重试或重新获取下载地址")), 502
