"""Explicit preparation and browsing of nested ZIP archives."""

import hashlib
import hmac
import os
import shutil
import zipfile
import mimetypes
import time
from pathlib import PurePosixPath

import requests
from flask import Blueprint, abort, current_app, jsonify, make_response, redirect, render_template, request, send_file, session, url_for

from api.archive_jobs import ArchiveJobError
from api.archive_preview import read_archive_member_file
from api.archive_probe import detected_kind
from api.archive_names import member_name
from api.nested_archive import extract_nested_zip, extract_nested_7z, extract_nested_rar
from api.split_archive import SPLIT_7Z, discover_volumes
from api.zip_preview import ArchivePasswordRequired, ZipPreviewError
from api import Pan123APIError, Pan123Client
from .utils import get_client

nested_archive_bp = Blueprint("nested_archive", __name__)


def _identity():
    return session.setdefault("archive_session", os.urandom(32).hex())


def _csrf():
    return session.setdefault("archive_csrf", os.urandom(32).hex())


def _outer(client, file_id):
    file = client.get_file_info_single(file_id, use_cache=True)
    if not file or file.is_folder:
        abort(404)
    split = SPLIT_7Z.fullmatch(file.filename)
    kind = ".7z" if split else (detected_kind(session, file) or PurePosixPath(file.filename).suffix.lower())
    if kind not in (".zip", ".7z", ".rar"):
        raise ZipPreviewError("外层文件不是支持的压缩包")
    key = (("split", file.get("parentFileId", 0), split[1]) if split else
           (file_id, kind, file.get("etag"), file.get("size"), file.get("updateAt")))
    return file, kind, key, split


@nested_archive_bp.route("/file/<int:file_id>/zip/nested/<int:member_id>", methods=["GET", "POST"])
def prepare(file_id, member_id):
    client = get_client()
    try:
        file, kind, outer_key, split = _outer(client, file_id)
        owner = _identity()
        token = _csrf()
        if request.method == "GET":
            return render_template("nested_archive.html", file=file, member_id=member_id, archive_version=request.args.get("v", ""), csrf_token=token,
                                   mode="prepare", status=None, browse_url=None)
        if not hmac.compare_digest(request.form.get("csrf_token", ""), token):
            return render_template("error.html", error="请求校验失败，请刷新页面后重试"), 400
        outer_password = request.form.get("outer_password") or None
        inner_password = request.form.get("inner_password") or None
        expected_version = request.form.get("v", "")
        if len(outer_password or "") > 1024 or len(inner_password or "") > 1024:
            return render_template("error.html", error="压缩包密码不能超过 1024 个字符"), 400
        result = (discover_volumes(client, file), "split") if split else client.get_final_download_url(file_id, prefer_webdav=False)
        if not result:
            raise ZipPreviewError("获取外层压缩包下载地址失败")
        password_scope = owner if (outer_password or inner_password) else "shared"
        pass_digest = hashlib.sha256((outer_password or "").encode() + b"\0" + (inner_password or "").encode()).hexdigest()
        cache_key = (outer_key, expected_version, member_id, password_scope, pass_digest)
        archive_cache = current_app.extensions["archive_cache"]
        jobs = current_app.extensions["archive_preparation_jobs"]
        prepare_timeout = int(current_app.config.get("ARCHIVE_PREP_TIMEOUT_SECONDS", 6 * 3600))
        max_member_bytes = min(64 * 1024**3, int(current_app.config.get("ARCHIVE_PREP_MAX_MEMBER_BYTES", 64 * 1024**3)))
        pan_config = current_app.config["PAN123_CONFIG"]

        def worker(staging, record):
            inner_path = staging / "inner.archive"

            def extract_member(archive, source):
                entries = archive.infolist()
                if member_id >= len(entries):
                    raise ArchiveJobError("内层压缩包成员已不存在，请刷新外层目录")
                entry = entries[member_id]
                if not expected_version or source.index_version != expected_version:
                    raise ArchiveJobError("外层压缩包目录已变化，请刷新后重新选择内层成员")
                name = member_name(entry)
                inner_kind = PurePosixPath(name).suffix.lower()
                if entry.is_dir() or inner_kind not in (".zip", ".7z", ".7zz", ".rar"):
                    raise ArchiveJobError("当前仅支持内层 ZIP、7z 或 RAR 文件")
                if entry.file_size > max_member_bytes:
                    raise ArchiveJobError("内层压缩包超过配置的暂存文件上限")
                if shutil.disk_usage(staging).free < entry.file_size:
                    raise ArchiveJobError("磁盘空间不足，无法暂存内层压缩包")
                jobs.reserve_staging(record, entry.file_size)
                if record["cancel"].is_set():
                    raise InterruptedError("已取消")
                jobs.set_phase(record, "正在读取内层压缩包", 0, entry.file_size)
                record["deadline"] = time.monotonic() + prepare_timeout
                source.deadline = record["deadline"]
                source.cancel_event = record["cancel"]
                read_archive_member_file(
                    archive, source, entry, output_path=inner_path,
                    on_progress=lambda done, total: jobs.set_phase(record, "正在读取内层压缩包", done, total),
                )
                return name

            try:
                if split:
                    with Pan123Client.from_app_config(pan_config) as worker_client:
                        inner_name = archive_cache.run(
                            outer_key, kind, lambda: result, extract_member, password=outer_password,
                            resolve_part=lambda part_id: worker_client.get_final_download_url(part_id, prefer_webdav=False),
                        )
                else:
                    inner_name = archive_cache.run(outer_key, kind, lambda: result, extract_member, password=outer_password)
                out = staging / "contents"
                suffix = PurePosixPath(inner_name).suffix.lower()
                extractor = {".zip": extract_nested_zip, ".rar": extract_nested_rar}.get(suffix, extract_nested_7z)
                return extractor(inner_path, out, inner_password, record, jobs)
            finally:
                try:
                    inner_path.unlink()
                except FileNotFoundError:
                    pass
                jobs.release_staging(record)

        started = current_app.extensions["archive_preparation_jobs"].start(cache_key, owner, worker)
        return redirect(url_for("nested_archive.job_page", job_id=started["id"]))
    except (ArchivePasswordRequired, ZipPreviewError, ArchiveJobError, zipfile.BadZipFile) as exc:
        return render_template("error.html", error=f"无法准备内层压缩包：{exc}"), 400
    except (requests.RequestException, OSError):
        return render_template("error.html", error="读取内层压缩包失败，请稍后重试"), 502
    except Pan123APIError:
        return render_template("error.html", error="获取压缩包信息失败，请稍后重试"), 502


@nested_archive_bp.route("/archive-jobs/<job_id>")
def job_page(job_id):
    jobs = current_app.extensions["archive_preparation_jobs"]
    status = jobs.status(job_id, _identity())
    if status is None:
        abort(404)
    response = make_response(render_template("nested_archive.html", file=None, member_id=None, csrf_token=_csrf(),
                           mode="status", status=status,
                           browse_url=url_for("nested_archive.browse", job_id=job_id) if status["state"] == "complete" else None))
    response.headers["Cache-Control"] = "no-store"
    return response


@nested_archive_bp.route("/api/archive-jobs/<job_id>", methods=["GET", "POST"])
def job_status(job_id):
    jobs = current_app.extensions["archive_preparation_jobs"]
    owner = _identity()
    if request.method == "POST":
        if not hmac.compare_digest(request.form.get("csrf_token", ""), _csrf()):
            response = jsonify(error="请求校验失败")
            response.headers["Cache-Control"] = "no-store"
            return response, 400
        if not jobs.cancel(job_id, owner):
            abort(404)
    status = jobs.status(job_id, owner)
    if status is None:
        abort(404)
    if status["state"] == "complete":
        status["browse_url"] = url_for("nested_archive.browse", job_id=job_id)
    response = jsonify(status)
    response.headers["Cache-Control"] = "no-store"
    return response, 200


@nested_archive_bp.route("/archive-jobs/<job_id>/browse")
def browse(job_id):
    jobs = current_app.extensions["archive_preparation_jobs"]
    root = jobs.prepared_path(job_id, _identity())
    if root is None:
        abort(404)
    root = os.path.realpath(root)
    prefix = request.args.get("path", "").strip("/")
    if prefix:
        parts = PurePosixPath(prefix).parts
        if any(p in (".", "..") for p in parts):
            abort(400)
        directory = os.path.realpath(os.path.join(root, *parts))
    else:
        directory = root
    if os.path.commonpath((root, directory)) != root or not os.path.isdir(directory):
        abort(404)
    dirs, files = [], []
    for name in sorted(os.listdir(directory), key=str.casefold):
        path = os.path.join(directory, name)
        if os.path.isdir(path):
            dirs.append(name)
        elif os.path.isfile(path):
            files.append({"name": name, "size": os.path.getsize(path), "url": url_for("nested_archive.member", job_id=job_id, member_path=(prefix + "/" if prefix else "") + name)})
            files[-1]["image"] = mimetypes.guess_type(name)[0] in ("image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp")
    parent = prefix.rpartition("/")[0] if "/" in prefix else ""
    return render_template("nested_archive.html", mode="browse", file=None, status=None, csrf_token=_csrf(),
                           job_id=job_id, prefix=prefix, parent=parent, dirs=dirs, files=files)


@nested_archive_bp.route("/archive-jobs/<job_id>/member/<path:member_path>")
def member(job_id, member_path):
    jobs = current_app.extensions["archive_preparation_jobs"]
    root = jobs.prepared_path(job_id, _identity())
    if root is None:
        abort(404)
    root = os.path.realpath(root)
    path = os.path.realpath(os.path.join(root, *PurePosixPath(member_path).parts))
    if os.path.commonpath((root, path)) != root or not os.path.isfile(path):
        abort(404)
    response = send_file(path, mimetype=mimetypes.guess_type(path)[0] or "application/octet-stream",
                         as_attachment=request.args.get("download") == "1", conditional=True, max_age=0)
    response.headers["X-Content-Type-Options"] = "nosniff"
    response.headers["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
    response.headers["Cache-Control"] = "private, no-store"
    return response
