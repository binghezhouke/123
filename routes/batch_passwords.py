"""Explicit, sequential per-file requests for a directory's shared ZIP password."""

import hmac
import secrets

from flask import Blueprint, current_app, jsonify, render_template, request, session, url_for
import requests

from api import Pan123APIError
from api.archive_password_batch import password_directory, zip_password_targets
from api.zip_password_validation import validate_zip_password
from api.zip_preview import ArchivePasswordRequired, ChangedArchive, ZipPreviewError
from .utils import folder_breadcrumbs, get_client

batch_passwords_bp = Blueprint("batch_passwords", __name__)


@batch_passwords_bp.after_request
def private_response(response):
    response.headers["Cache-Control"] = "no-store"
    return response


def checked_json():
    data = request.get_json(silent=True)
    expected = session.get("archive_csrf", "")
    submitted = data.get("csrf_token", "") if isinstance(data, dict) else ""
    if not expected or not isinstance(submitted, str) or not hmac.compare_digest(
            submitted.encode("utf-8"), expected.encode("utf-8")):
        return None
    return data


@batch_passwords_bp.get("/directory/<int:parent_id>/zip-passwords")
def page(parent_id):
    csrf = session.setdefault("archive_csrf", secrets.token_urlsafe(32))
    session.setdefault("archive_session", secrets.token_urlsafe(32))
    client = get_client()
    shared_password = None
    shared_password_error = None
    # Keep the page compatible with lightweight clients used by integrations and
    # older test fixtures while the shared-password capability is optional.
    read_shared_password = getattr(client, "get_shared_password", None)
    if callable(read_shared_password):
        try:
            shared_password = read_shared_password(parent_id)
        except (Pan123APIError, requests.RequestException, OSError, ValueError) as exc:
            current_app.logger.warning("读取目录共享密码失败，目录 ID: %s (%s)", parent_id, type(exc).__name__)
            shared_password_error = "当前共享密码暂时无法读取"
    return render_template("batch_passwords.html", parent_id=parent_id, csrf_token=csrf,
                           breadcrumbs=folder_breadcrumbs(client, parent_id),
                           shared_password=shared_password, shared_password_error=shared_password_error)


@batch_passwords_bp.post("/directory/<int:parent_id>/zip-passwords/plan")
def plan(parent_id):
    if checked_json() is None:
        return jsonify(error="请求校验失败，请刷新页面后重试"), 400
    try:
        targets = zip_password_targets(password_directory(get_client(), parent_id))
        for target in targets:
            target["url"] = url_for("batch_passwords.apply", parent_id=parent_id, file_id=target["id"])
        return jsonify(items=targets)
    except (Pan123APIError, requests.RequestException, OSError):
        return jsonify(error="无法完整读取目录，请稍后重试"), 502


@batch_passwords_bp.post("/directory/<int:parent_id>/zip-passwords/file/<int:file_id>")
def apply(parent_id, file_id):
    data = checked_json()
    if data is None:
        return jsonify(error="请求校验失败，请刷新页面后重试"), 400
    skip_validation = data.get("skip_validation", False)
    if not isinstance(skip_validation, bool):
        return jsonify(error="跳过验证选项必须为布尔值"), 400
    password = data.get("password")
    if not isinstance(password, str) or not password or len(password) > 1024:
        return jsonify(error="请输入 1 到 1024 个字符的密码"), 400
    try:
        if len(password.encode("utf-8")) > 4096:
            raise UnicodeError()
    except UnicodeError:
        return jsonify(error="密码必须是有效 UTF-8 文本，且不超过 4096 字节"), 400
    # Fixed stripes bound lock storage, including repeated/new directory IDs.
    lock = current_app.extensions["batch_password_locks"][parent_id % 32]
    if not lock.acquire(blocking=False):
        return jsonify(error="已有密码设置正在处理，请稍后重试"), 409
    try:
        client = get_client()
        file = client.get_file_detail(file_id)
        if (file is None or file.file_id != file_id or file.is_folder or
                str(file.trashed) in ("1", "True") or file.parent_file_id != parent_id or
                not file.filename.lower().endswith(".zip")):
            return jsonify(status="skipped", message="文件已变化或不属于当前目录的 ZIP")

        def validate(archive, source):
            if not any(not entry.is_dir() and entry.flag_bits & 1 for entry in archive.infolist()):
                return False
            validate_zip_password(archive, source)
            return True

        key = (file_id, ".zip", file.get("etag"), file.get("size"), file.get("updateAt"))
        if not skip_validation:
            encrypted = current_app.extensions["archive_cache"].run(
                key, ".zip", lambda: client.get_final_download_url(file_id, prefer_webdav=False),
                validate, password=password)
            if not encrypted:
                return jsonify(status="skipped", message="ZIP 未加密，无需密码文件")
        result = client.save_archive_password(
            file_id, password, archive_kind=".zip", skip_existing=True, expected_archive=file)
        if result.get("skipped"):
            return jsonify(status="skipped", message="已存在密码文件，未覆盖")
        current_app.extensions["directory_pages"].invalidate_directory(parent_id)
        owner = session.setdefault("archive_session", secrets.token_urlsafe(32))
        current_app.extensions["archive_passwords"].set(owner, key, password)
        message = "已直接保存同级 .pwd（未验证密码）" if skip_validation else "密码已验证，已保存同级 .pwd"
        return jsonify(status="saved", validated=not skip_validation, message=message)
    except ArchivePasswordRequired:
        return jsonify(status="failed", message="密码不正确或加密数据已损坏")
    except ChangedArchive:
        return jsonify(status="failed", message="压缩包已变化，请重新扫描目录")
    except ZipPreviewError:
        return jsonify(status="failed", message="无法验证此 ZIP，请单独打开检查格式或重试")
    except (Pan123APIError, requests.RequestException, OSError):
        current_app.logger.warning("批量 ZIP 密码验证或保存失败，文件 ID: %s", file_id)
        return jsonify(status="failed", message="读取或保存失败，请重试；已有密码文件不会被覆盖")
    finally:
        lock.release()


@batch_passwords_bp.post("/directory/<int:parent_id>/shared-password")
def apply_shared(parent_id):
    data = checked_json()
    if data is None:
        return jsonify(error="请求校验失败，请刷新页面后重试"), 400
    password = data.get("password")
    overwrite = data.get("overwrite", False)
    if not isinstance(password, str) or not password or len(password) > 1024:
        return jsonify(error="请输入 1 到 1024 个字符的密码"), 400
    if not isinstance(overwrite, bool):
        return jsonify(error="覆盖选项必须为布尔值"), 400
    try:
        if len(password.encode("utf-8")) > 4096:
            raise UnicodeError()
    except UnicodeError:
        return jsonify(error="密码必须是有效 UTF-8 文本，且不超过 4096 字节"), 400
    lock = current_app.extensions["batch_password_locks"][parent_id % 32]
    if not lock.acquire(blocking=False):
        return jsonify(error="已有密码设置正在处理，请稍后重试"), 409
    try:
        result = get_client().save_shared_password(parent_id, password, overwrite=overwrite)
        current_app.extensions["directory_pages"].invalidate_directory(parent_id)
        if result.get("skipped"):
            return jsonify(status="skipped", message="已存在共享密码，未覆盖")
        return jsonify(status="saved", message="已保存当前目录共享密码")
    except (Pan123APIError, requests.RequestException, OSError, ValueError):
        current_app.logger.warning("目录共享密码保存失败，目录 ID: %s", parent_id)
        return jsonify(status="failed", message="共享密码保存失败，请重试"), 502
    finally:
        lock.release()
