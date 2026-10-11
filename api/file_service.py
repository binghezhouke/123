"""
文件操作服务
"""
import hashlib
import logging
import os
import re
import tempfile
import threading
import time
from typing import List, Dict, Any, Tuple, Optional
from urllib.parse import quote  # 添加导入
from .http_client import RequestHandler
from .cache import FileCacheManager
from .exceptions import ValidationError, Pan123APIError, FileUploadError
from .models import File, FileList

logger = logging.getLogger(__name__)

WEBDAV_DEFAULT_PATH = "/webdav"
# Conservative client-side payload bound; the API does not publish a maximum.
FILE_INFO_BATCH_SIZE = 100


def _mask_credentials(url: str) -> str:
    """把 URL 里的 user:password@ 替换为 ***@，避免凭据写进日志。"""
    return re.sub(r'//[^/@\s]+@', '//***@', url)


def _is_trashed(file_data: Dict[str, Any]) -> bool:
    """判断文件是否已移入回收站（trashed 可能是 int/bool/字符串）。"""
    value = file_data.get('trashed', 0)
    try:
        return int(value) == 1
    except (TypeError, ValueError):
        return value is True


class FileService:
    """文件操作服务"""

    def __init__(self, http_client: RequestHandler, cache_manager: FileCacheManager = None, config: Dict[str, Any] = None):
        self.http_client = http_client
        self.cache_manager = cache_manager
        self.config = config or {}
        # 目录列表缓存：key 为 (parent_id, limit, max_pages)，内容变化时按 parent_id 失效
        self._dir_cache: Dict[Tuple[int, int, int], Tuple[FileList, Optional[int]]] = {}
        self._mkdir_cache: Dict[Tuple[int, str], int] = {}
        self._cache_lock = threading.Lock()

    def _dir_cache_key(self, parent_id: int, limit: int, max_pages: int) -> Tuple[int, int, int]:
        """目录缓存键：limit/max_pages 会影响结果，必须参与键，否则被截断的结果会被复用。"""
        return (parent_id, limit, max_pages)

    def _invalidate_dir_cache(self, parent_id: int) -> None:
        """目录内容变化（新建目录/文件）后，作废该目录缓存的所有参数组合。"""
        with self._cache_lock:
            for key in [k for k in self._dir_cache if k[0] == parent_id]:
                del self._dir_cache[key]

    def list_files(self,
                   parent_id: int = 0,
                   limit: int = 100,
                   search_data: str = None,
                   search_mode: int = None,
                   last_file_id: int = None,
                   auto_fetch_all: bool = False,
                   qps_limit: float = 5.0,
                   max_pages: int = 100,
                   use_cache: bool = True,
                   on_page=None) -> Tuple[FileList, Optional[int]]:
        """
        列出文件并返回FileList对象

        :param parent_id: 父目录ID，默认为0（根目录）
        :param limit: 返回的文件数量限制，默认100
        :param search_data: 搜索关键词
        :param search_mode: 搜索模式
        :param last_file_id: 分页参数
        :param auto_fetch_all: 是否自动获取所有分页，默认False
        :param qps_limit: QPS限制（每秒请求数），默认5.0（官方限额 list 10/v2 15）
        :param max_pages: 最大页数限制，默认100页
        :param on_page: 每取完一页回调一次 on_page(页码, 本页条数, 累计条数)，
                        用于在拉取大目录时显示进度
        :return: (FileList对象, next_last_file_id)
        """
        # 仅在获取所有页面且不搜索时使用缓存
        use_dir_cache = auto_fetch_all and not search_data and use_cache
        dir_cache_key = self._dir_cache_key(parent_id, limit, max_pages)
        if use_dir_cache and dir_cache_key in self._dir_cache:
            logger.info(f"使用目录缓存: parent_id={parent_id}")
            return self._dir_cache[dir_cache_key]

        if auto_fetch_all:
            result = self._fetch_all_pages(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                qps_limit=qps_limit,
                max_pages=max_pages,
                on_page=on_page
            )
            if use_dir_cache:
                logger.info(f"缓存目录列表: parent_id={parent_id}")
                with self._cache_lock:
                    self._dir_cache[dir_cache_key] = result
            return result
        else:
            return self._fetch_single_page(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                last_file_id=last_file_id
            )

    def _fetch_single_page(self,
                           parent_id: int = 0,
                           limit: int = 100,
                           search_data: str = None,
                           search_mode: int = None,
                           last_file_id: int = None) -> Tuple[FileList, Optional[int]]:
        """获取单页数据（已过滤回收站记录）"""
        file_list, next_last_file_id, _ = self._fetch_page(
            parent_id=parent_id,
            limit=limit,
            search_data=search_data,
            search_mode=search_mode,
            last_file_id=last_file_id
        )
        return file_list, next_last_file_id

    def _fetch_page(self,
                    parent_id: int = 0,
                    limit: int = 100,
                    search_data: str = None,
                    search_mode: int = None,
                    last_file_id: int = None) -> Tuple[FileList, Optional[int], int]:
        """
        获取单页数据，返回 (过滤后的FileList, next_last_file_id, 服务端返回的原始条数)。

        原始条数用于分页结束判断：过滤掉回收站记录后某页可能为空，
        但这并不代表没有下一页。
        """
        endpoint = "/api/v2/file/list"
        params = {
            "limit": limit,
            "parentFileId": parent_id
        }

        if search_data:
            params["searchData"] = search_data
            if search_mode is not None:
                params["searchMode"] = search_mode

        if last_file_id is not None:
            params["lastFileId"] = last_file_id

        result = self.http_client.get(endpoint, params=params)

        if not result or 'data' not in result:
            return FileList([]), None, 0

        raw_file_list = result['data'].get('fileList') or []
        # 过滤掉已被移入垃圾桶的文件（trashed == 1）
        file_list = [f for f in raw_file_list if not _is_trashed(f)]

        next_last_file_id = result['data'].get('lastFileId')

        return FileList(file_list), next_last_file_id, len(raw_file_list)

    def _fetch_all_pages(self,
                         parent_id: int = 0,
                         limit: int = 100,
                         search_data: str = None,
                         search_mode: int = None,
                         qps_limit: float = 5.0,
                         max_pages: int = 100,
                         on_page=None) -> Tuple[FileList, Optional[int]]:
        """
        自动获取所有分页数据，带QPS限制

        :param parent_id: 父目录ID
        :param limit: 每页限制
        :param search_data: 搜索关键词
        :param search_mode: 搜索模式
        :param qps_limit: QPS限制（每秒请求数）
        :param max_pages: 最大页数限制，默认100页
        :param on_page: 每页回调 on_page(页码, 本页条数, 累计条数)
        :return: (合并的FileList对象, None)
        """
        all_files = []
        last_file_id = None
        page_count = 0
        last_request_time = 0

        logger.info(f"开始获取所有分页数据，QPS限制: {qps_limit} req/s，最大页数: {max_pages}")

        while True:
            # QPS 限制：确保请求间隔至少为 1/qps_limit 秒
            if page_count > 0:  # 第一次请求不需要等待
                min_interval = 1.0 / qps_limit
                current_time = time.time()
                elapsed = current_time - last_request_time

                if elapsed < min_interval:
                    wait_time = min_interval - elapsed
                    logger.info(f"QPS限制等待 {wait_time:.2f} 秒...")
                    time.sleep(wait_time)

            last_request_time = time.time()

            # 获取当前页数据
            file_list, next_last_file_id, raw_count = self._fetch_page(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                last_file_id=last_file_id
            )

            page_count += 1
            current_page_count = len(file_list.files)
            trashed_count = raw_count - current_page_count
            all_files.extend(file_list.files)

            trashed_note = f"（其中 {trashed_count} 条在回收站，已跳过）" if trashed_count else ""
            logger.info(
                f"第 {page_count} 页: 原始 {raw_count} 条，可用 {current_page_count} 条{trashed_note}，累计 {len(all_files)} 个")

            if on_page is not None:
                on_page(page_count, current_page_count, len(all_files))

            # 检查是否还有更多页。
            # 结束判断只看服务端返回的原始条数：整页都是回收站记录时过滤后为空，
            # 但这不代表没有下一页，用过滤后的条数判断会提前截断结果。
            if next_last_file_id is None or next_last_file_id == -1 or raw_count == 0:
                if next_last_file_id == -1:
                    logger.info(
                        f"已到达最后一页（next_file_id = -1），共 {page_count} 页，总计 {len(all_files)} 个文件")
                else:
                    logger.info(f"分页获取完成，共 {page_count} 页，总计 {len(all_files)} 个文件")
                break

            # 检查是否达到最大页数限制
            if page_count >= max_pages:
                logger.info(f"已达到最大页数限制（{max_pages} 页），共获取 {len(all_files)} 个文件")
                break

            last_file_id = next_last_file_id

        # 将所有文件数据转换为字典列表，然后创建合并的FileList
        all_files_data = [file.to_dict() for file in all_files]
        return FileList(all_files_data), None

    def create_file(self,
                    parent_id: int,
                    filename: str,
                    etag: str,
                    size: int,
                    duplicate: int = 1,
                    contain_dir: bool = False,
                    sensitive: bool = False) -> Dict[str, Any]:
        """
        创建文件（预上传）

        :param parent_id: 父目录ID
        :param filename: 文件名
        :param etag: 文件MD5
        :param size: 文件大小
        :param duplicate: 文件名冲突策略 (1: 保留两者, 2: 覆盖)
        :param contain_dir: 是否包含路径
        :return: API响应的JSON数据字典
        """
        # 验证文件名
        # 当 contain_dir 为 True 时，允许传入包含路径的 filename（使用正斜杠 '/'），
        # 但仍需限制总体字节长度不超过255，并且禁止其他非法字符。
        if len(filename.encode('utf-8')) > 255:
            raise ValidationError("文件名过长（超过255个字节）")

        # 如果不包含目录，则严格禁止任何路径分隔符或非法字符
        if not contain_dir:
            if re.search(r'[\\/:*?"<>|]', filename):
                raise ValidationError(r'文件名包含非法字符: \/:*?"<>|')
        else:
            # contain_dir == True 时，允许正斜杠 '/' 作为目录分隔符，
            # 但仍禁止反斜杠和其他非法字符。
            if re.search(r'[\\:*?"<>|]', filename):
                raise ValidationError('包含路径的文件名包含非法字符: \\:*?"<>|')

        if not filename.strip():
            raise ValidationError("文件名不能为空")

        try:
            endpoint = "/upload/v2/file/create"
            json_data = {
                "parentFileID": parent_id,
                "filename": filename,
                "etag": etag,
                "size": size,
                "duplicate": duplicate,
                "containDir": contain_dir
            }

            result = self.http_client.post(endpoint, json_data=json_data)

            if result and 'data' in result:
                return result['data']

            return {}
        except Exception as e:
            if sensitive:
                logger.warning("敏感文件预上传失败，检查文件是否已存在")
            else:
                logger.warning(f"预上传失败: {e}, 尝试检查文件是否已存在...")
            if duplicate != 1:
                # duplicate=2 是覆盖上传，不能把"已存在的同名文件"当成完成
                raise
            try:
                remote_files_list, _ = self.list_files(
                    parent_id=parent_id, auto_fetch_all=True, use_cache=False)
            except Exception as list_error:
                if sensitive:
                    logger.error("检查敏感文件是否已存在时失败")
                else:
                    logger.error(f"检查已存在文件时出错: {list_error}")
                raise e

            existing_file = remote_files_list.find_by_name(filename)
            if existing_file and not existing_file.is_folder and existing_file.size == size:
                logger.info(
                    f"  ✓ 找到已存在的文件 '{filename}' 且大小相同，按跳过处理。")
                return {
                    "fileID": existing_file.file_id,
                    "filename": filename,
                    "size": size,
                    "skipped": True,
                    "existing": True,
                }
            raise

    def upload_file(self,
                    local_path: str,
                    parent_id: int,
                    filename: str = None,
                    duplicate: int = 1,
                    skip_if_exists: bool = False,
                    try_sha1_reuse: bool = True,
                    sensitive: bool = False) -> Optional[Dict[str, Any]]:
        """
        上传完整文件，处理预上传、分片上传和完成上传的整个流程。

        :param local_path: 本地文件路径
        :param parent_id: 上传到的父目录ID
        :param filename: 在云端保存的文件名，如果为None则使用本地文件名
        :param duplicate: 文件名冲突策略 (1: 保留两者, 2: 覆盖)
        :param skip_if_exists: 如果为True，且远程存在同名同大小文件，则跳过上传
        :param try_sha1_reuse: 是否先尝试SHA1秒传，默认为True
        :return: 成功则返回文件信息字典，否则返回None
        """
        # 1. 检查文件是否存在
        if not os.path.exists(local_path):
            raise FileNotFoundError(f"文件不存在: {local_path}")

        # 2. 获取文件名和大小
        if filename is None:
            filename = os.path.basename(local_path)
        size = os.path.getsize(local_path)

        # 2.1. 如果设置了 skip_if_exists，检查远程文件
        if skip_if_exists:
            logger.info(f"检查远程文件是否存在: '{filename}' in parent {parent_id}")
            # 这里我们假设list_files能获取所有文件，对于大目录可能需要分页
            remote_files_list, _ = self.list_files(
                parent_id=parent_id, auto_fetch_all=True)
            existing_file = remote_files_list.find_by_name(filename)
            if existing_file and not existing_file.is_folder:
                if existing_file.size == size:
                    logger.info(f"  ✓ 文件 '{filename}' 已存在且大小相同，跳过上传。")
                    return {
                        "fileID": existing_file.file_id,
                        "filename": filename,
                        "size": size,
                        "skipped": True
                    }
                else:
                    logger.warning(
                        f"  ! 文件 '{filename}' 已存在但大小不同 (本地: {size}, 远程: {existing_file.size})，继续上传。")

        # 2.2. 如果启用SHA1秒传，先尝试秒传
        if try_sha1_reuse and not sensitive:
            logger.info(f"尝试SHA1秒传文件: '{filename}'...")
            try:
                sha1_result = self.try_sha1_reuse(
                    local_path, filename, parent_id, duplicate)
            except Pan123APIError as e:
                # 秒传接口本身出错不应该阻断常规上传流程
                logger.warning(f"SHA1秒传调用失败，回退到常规上传: {e}")
                sha1_result = None
            if sha1_result and sha1_result.get('reuse'):
                logger.info(f"✓ SHA1秒传成功！文件ID: {sha1_result.get('fileID')}")
                self._invalidate_dir_cache(parent_id)
                return {
                    "fileID": sha1_result.get('fileID'),
                    "filename": filename,
                    "size": size,
                    "reuse": True,
                    "method": "sha1_reuse"
                }
            logger.info("SHA1秒传未命中，继续常规上传流程...")

        # 3. 计算MD5
        etag = self._calculate_md5(local_path)
        if sensitive:
            logger.info(f"开始上传敏感文件: '{filename}', 大小: {size} bytes")
        else:
            logger.info(
                f"开始上传文件: '{filename}', 大小: {size} bytes, MD5: {etag}")

        # 4. 调用 create_file (预上传)
        try:
            pre_upload_info = self.create_file(
                parent_id=parent_id,
                filename=filename,
                etag=etag,
                size=size,
                duplicate=duplicate,
                sensitive=sensitive,
            )
        except ValidationError as e:
            if sensitive:
                logger.error("敏感文件预上传参数无效")
            else:
                logger.error(f"预上传失败: {e}")
            return None

        # 5. 检查是否秒传
        if pre_upload_info.get("reuse"):
            logger.info("文件秒传成功")
            self._invalidate_dir_cache(parent_id)
            return {
                "fileID": pre_upload_info.get("fileID"),
                "filename": filename,
                "size": size,
                "reuse": True
            }

        # 5.1. 预上传失败但云端已有同名同大小文件（create_file 的兜底分支），按跳过处理
        if pre_upload_info.get("skipped"):
            logger.info(f"云端已存在同名同大小文件，跳过: '{filename}'")
            return {
                "fileID": pre_upload_info.get("fileID"),
                "filename": filename,
                "size": size,
                "skipped": True,
                "existing": True,
            }

        # 6. 如果不是秒传，准备分片上传
        preupload_id = pre_upload_info.get("preuploadID")
        slice_size = pre_upload_info.get("sliceSize")
        servers = pre_upload_info.get("servers")

        if not all([preupload_id, slice_size, servers]):
            raise ValidationError(
                "预上传响应缺少必要信息 (preuploadID, sliceSize, servers)")

        # 估算分片数量（向上取整）
        try:
            estimated_parts = (size + int(slice_size) - 1) // int(slice_size)
        except Exception:
            estimated_parts = None

        if estimated_parts:
            if sensitive:
                logger.info(f"敏感文件需要分片上传，分片大小: {slice_size} bytes，预计分片数: {estimated_parts}")
            else:
                logger.info(
                    f"需要分片上传. Pre-upload ID: {preupload_id}, 分片大小: {slice_size} bytes, 预计分片数: {estimated_parts}")
        else:
            if sensitive:
                logger.info(f"敏感文件需要分片上传，分片大小: {slice_size}")
            else:
                logger.info(f"需要分片上传. Pre-upload ID: {preupload_id}, 分片大小: {slice_size}")

        # 7. 上传分片
        upload_success = self._upload_chunks(
            local_path, preupload_id, slice_size, servers, sensitive=sensitive)

        if not upload_success:
            logger.error("分片上传失败")
            return None

        # 8. 完成上传
        complete_info = self._complete_upload(preupload_id, sensitive=sensitive)

        if complete_info:
            logger.info("文件上传成功")
            self._invalidate_dir_cache(parent_id)
            return complete_info
        else:
            logger.error("完成上传步骤失败")
            return None

    # 哈希读盘块大小：太小（如 8KB）会让 Python 层循环成为瓶颈，1MiB 足够大又不会占太多内存
    HASH_CHUNK_SIZE = 1024 * 1024

    def _calculate_md5(self, file_path: str, chunk_size: int = HASH_CHUNK_SIZE) -> str:
        """计算文件的MD5值"""
        md5 = hashlib.md5()
        with open(file_path, 'rb') as f:
            while chunk := f.read(chunk_size):
                md5.update(chunk)
        return md5.hexdigest()

    def _calculate_sha1(self, file_path: str, chunk_size: int = HASH_CHUNK_SIZE) -> str:
        """计算文件的SHA1值"""
        sha1 = hashlib.sha1()
        with open(file_path, 'rb') as f:
            while chunk := f.read(chunk_size):
                sha1.update(chunk)
        return sha1.hexdigest()

    def try_sha1_reuse(self,
                       local_path: Optional[str],
                       filename: str,
                       parent_id: int,
                       duplicate: int = 1,
                       sha1: Optional[str] = None,
                       size: Optional[int] = None) -> Optional[Dict[str, Any]]:
        """
        尝试使用SHA1秒传文件，可接受本地文件或预先提供的sha1/size元数据。

        返回值语义（调用方据此区分"没命中"和"调用失败"）：
        - 返回含 ``reuse=True`` 的数据：秒传成功
        - 返回 None：接口正常响应，但云端没有这个文件（需要走常规上传）
        - 抛出 Pan123APIError / FileNotFoundError：这次调用本身失败，不代表云端没有该文件

        :param local_path: 本地文件路径；如果提供了 sha1 和 size，则可以为 None
        :param filename: 文件名
        :param parent_id: 父目录ID
        :param duplicate: 文件名冲突处理策略（1保留两者，2覆盖原文件）
        :param sha1: 预先计算好的SHA1（40位hex）
        :param size: 预先提供的文件大小（bytes）
        :return: 秒传成功时返回响应数据，未命中时返回None
        """
        sha1_hash = sha1
        file_size = size

        # 如未提供sha1/size，则基于本地文件计算
        if sha1_hash is None or file_size is None:
            if not local_path or not os.path.isfile(local_path):
                raise FileNotFoundError("需要本地文件来计算SHA1，但未提供有效路径")
            file_size = file_size or os.path.getsize(local_path)
            sha1_hash = sha1_hash or self._calculate_sha1(local_path)

        # 标准化sha1
        sha1_hash = sha1_hash.lower() if sha1_hash else sha1_hash

        logger.info(f"  计算/使用SHA1: {sha1_hash}, 大小: {file_size} bytes")

        endpoint = "/upload/v2/file/sha1_reuse"
        json_data = {
            "parentFileID": parent_id,
            "filename": filename,
            "sha1": sha1_hash,
            "size": file_size,
            "duplicate": duplicate
        }

        result = self.http_client.post(endpoint, json_data=json_data)

        if not result:
            raise Pan123APIError("sha1_reuse 接口返回空响应")

        data = result.get('data') or {}
        if not data.get('reuse'):
            logger.info("  SHA1未命中，需要常规上传")
            return None

        file_id = data.get('fileID')
        logger.info(f"  ✓ 文件秒传成功！文件ID: {file_id}")
        return data

    def _upload_chunks(self, local_path: str, preupload_id: str, slice_size: int,
                       servers: List[str], sensitive: bool = False) -> bool:
        """
        读取文件并上传所有分片。
        """
        logger.info("开始上传分片...")
        with open(local_path, 'rb') as f:
            part_number = 1
            server_count = len(servers)
            if server_count == 0:
                logger.error("错误：没有可用的上传服务器。")
                return False

            while True:
                chunk = f.read(slice_size)
                if not chunk:
                    break

                # 轮询使用上传服务器
                server = servers[(part_number - 1) % server_count]
                if not server.startswith(('http://', 'https://')):
                    server = 'http://' + server

                endpoint = f"{server}/upload/v2/file/slice"
                slice_md5 = hashlib.md5(chunk).hexdigest()

                form_data = {
                    "preuploadID": preupload_id,
                    "sliceNo": str(part_number),
                    "sliceMD5": slice_md5,
                }

                files_data = {
                    "slice": chunk
                }

                if sensitive:
                    logger.info(f"上传敏感文件分片 {part_number}（大小: {len(chunk)} bytes）")
                else:
                    logger.info(
                        f"  上传分片 {part_number} (大小: {len(chunk)} bytes, MD5: {slice_md5}) 到 {endpoint}...")

                try:
                    # 假设 http_client.post 可以通过 `data` 和 `files` 参数处理 multipart/form-data
                    result = self.http_client.post(
                        endpoint, data=form_data, files=files_data)

                    if not result:
                        logger.error(f"敏感文件分片 {part_number} 上传失败（无返回结果）" if sensitive else
                                     f"  上传分片 {part_number} 失败 (无返回结果)。")
                        return False

                    # 假设API成功时返回的json包含 code: 0
                    if result.get('code') != 0:
                        if sensitive:
                            logger.error(f"敏感文件分片 {part_number} 上传失败")
                        else:
                            logger.error(
                                f"  上传分片 {part_number} 失败: {result.get('message', '未知错误')}")
                        return False

                except Exception as e:
                    if sensitive:
                        logger.error(f"敏感文件分片 {part_number} 上传时发生网络或客户端错误")
                    else:
                        logger.error(f"  上传分片 {part_number} 时发生网络或客户端错误: {e}")
                    return False

                logger.info(f"  分片 {part_number} 上传成功。")
                part_number += 1

        logger.info("所有分片上传成功。")
        return True

    def _complete_upload(self, preupload_id: str, max_retries: int = 5,
                         retry_delay: int = 2, sensitive: bool = False) -> Optional[Dict[str, Any]]:
        """
        通知服务器所有分片已上传完毕。
        包含针对“文件校验中”错误的重试逻辑。
        """
        if sensitive:
            logger.info("正在完成敏感文件上传")
        else:
            logger.info(f"正在发送上传完成请求, preuploadID: {preupload_id}...")

        endpoint = "/upload/v2/file/upload_complete"
        json_data = {"preuploadID": preupload_id}

        # 将重试责任交给 http_client（网络层）。这里直接调用一次，
        # http_client 会根据配置对网络/业务码（如20103）进行重试。
        try:
            result = self.http_client.post(endpoint, json_data=json_data)
            data = result.get('data', {})
            if data.get('completed'):
                logger.info(f"文件上传成功! FileID: {data.get('fileID')}")
                return data
            logger.warning("完成上传请求返回未完成状态。")
            return None
        except Pan123APIError as e:
            if sensitive:
                logger.error("完成敏感文件上传请求失败")
            else:
                logger.error(f"完成上传请求失败: {e}")
            return None
        except Exception as e:
            if sensitive:
                logger.error("完成敏感文件上传请求时发生未知异常")
            else:
                logger.error(f"完成上传请求时发生未知异常: {e}")
            return None

    def get_download_info(self, file_id: int) -> Dict[str, Any]:
        """
        获取文件的下载信息，包括下载链接

        :param file_id: 文件ID，必须是一个有效的文件ID
        :return: API响应的JSON数据字典，包含downloadUrl等信息
        """
        endpoint = "/api/v1/file/download_info"
        params = {"fileId": file_id}

        return self.http_client.get(endpoint, params=params)

    def get_file_detail(self, file_id: int) -> Optional[File]:
        """Fetch one file directly from the detail endpoint (without cache)."""
        if not isinstance(file_id, int):
            raise ValidationError("文件ID必须是整数")
        result = self.http_client.get(
            "/api/v1/file/detail", params={"fileID": file_id})
        data = result.get("data") if isinstance(result, dict) else None
        if not isinstance(data, dict) or not data:
            return None
        normalized = dict(data)
        for source, target in (("fileID", "fileId"),
                               ("parentFileID", "parentFileId")):
            if target not in normalized and source in normalized:
                normalized[target] = normalized[source]
            value = normalized.get(target)
            if isinstance(value, str) and value.isdecimal():
                normalized[target] = int(value)
        return File(normalized)

    def save_zip_password(self, file_id: int, password: str) -> Dict[str, Any]:
        return self.save_archive_password(file_id, password)

    def save_shared_password(self, parent_id: int, password: str, *, overwrite=False) -> Dict[str, Any]:
        """Save the directory-wide password file used by mount123.

        The hidden ``.mount123.pwd`` (and legacy ``.123mount.pwd``) is resolved by the mount in the archive's
        directory and, when enabled there, its parent directories. This
        operation deliberately skips archive validation: it is intended for a
        directory whose archives are known to share a password.
        """
        if not isinstance(parent_id, int) or isinstance(parent_id, bool) or parent_id < 0:
            raise ValidationError("parent_id 必须是非负整数")
        if not isinstance(password, str):
            raise ValidationError("压缩包密码必须是文本")
        try:
            password_bytes = password.encode("utf-8", errors="strict")
        except UnicodeEncodeError as exc:
            raise ValidationError("压缩包密码必须是有效UTF-8文本") from exc
        if not password_bytes or len(password_bytes) > 4096:
            raise ValidationError("密码长度必须为1到4096个UTF-8字节")

        if parent_id:
            directory = self.get_file_detail(parent_id)
            if (directory is None or not directory.is_folder or
                    _is_trashed({"trashed": directory.trashed})):
                raise ValidationError("parent_id 必须指向有效目录")
        siblings, _ = self.list_files(parent_id=parent_id, auto_fetch_all=True, use_cache=False)
        matches = [item for item in siblings
                   if item.filename in (".mount123.pwd", ".123mount.pwd")]
        if any(item.is_folder for item in matches):
            raise ValidationError("同名共享密码路径是目录，拒绝覆盖")
        if len(matches) > 1:
            raise ValidationError("同目录存在多个共享密码文件，拒绝覆盖")
        if matches and not overwrite:
            return {"skipped": True, "filename": matches[0].filename, "fileID": matches[0].file_id}

        fd, temp_path = tempfile.mkstemp(prefix="pan123-shared-password-")
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "wb") as password_file:
                fd = -1
                password_file.write(password_bytes)
                password_file.flush()
            result = self.upload_file(
                temp_path, parent_id, filename=".mount123.pwd", duplicate=2 if overwrite else 1,
                skip_if_exists=False, try_sha1_reuse=False, sensitive=True)
            if not isinstance(result, dict) or result.get("fileID") is None:
                raise FileUploadError("共享密码文件上传失败")
            self._invalidate_dir_cache(parent_id)
            return result
        finally:
            if fd >= 0:
                os.close(fd)
            try:
                os.remove(temp_path)
            except FileNotFoundError:
                pass

    def get_shared_password(self, parent_id: int) -> Optional[str]:
        """Read the current directory's shared password for the web UI."""
        if not isinstance(parent_id, int) or isinstance(parent_id, bool) or parent_id < 0:
            raise ValidationError("parent_id 必须是非负整数")
        siblings, _ = self.list_files(parent_id=parent_id, auto_fetch_all=True, use_cache=False)
        matches = [item for item in siblings
                   if item.filename in (".mount123.pwd", ".123mount.pwd") and not item.is_folder]
        if not matches:
            return None
        if len(matches) > 1:
            raise ValidationError("同目录存在多个共享密码文件，拒绝读取")
        link = self.get_final_download_url(matches[0].file_id, prefer_webdav=False, use_cache=False)
        if not link:
            raise FileUploadError("无法获取共享密码文件下载地址")
        import requests
        response = None
        try:
            response = requests.get(link[0] if isinstance(link, tuple) else link,
                                    timeout=(10, 30), stream=True)
            response.raise_for_status()
            data = bytearray()
            for chunk in response.iter_content(chunk_size=4096):
                if not chunk:
                    continue
                data.extend(chunk)
                if len(data) > 4096:
                    raise ValidationError("共享密码文件超过 4096 字节")
            return bytes(data).decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValidationError("共享密码文件不是有效 UTF-8 文本") from exc
        finally:
            if response is not None:
                response.close()

    def save_archive_password(self, file_id: int, password: str, archive_kind=None,
                              *, skip_existing=False, expected_archive=None) -> Dict[str, Any]:
        """Save an archive password as a sibling `<archive>.pwd`.

        The caller validates the password or explicitly offers direct saving
        without validation. This method stores the exact UTF-8 bytes in a
        private temporary file, then uses the regular upload flow to replace
        any existing sibling sidecar. Batch callers pass skip_existing to
        preserve siblings and expected_archive to recheck the selected source.
        """
        if not isinstance(file_id, int) or isinstance(file_id, bool) or file_id <= 0:
            raise ValidationError("file_id 必须是正整数")
        if not isinstance(password, str):
            raise ValidationError("压缩包密码必须是文本")
        try:
            password_bytes = password.encode("utf-8", errors="strict")
        except UnicodeEncodeError as exc:
            raise ValidationError("压缩包密码必须是有效UTF-8文本") from exc
        if not password_bytes or len(password_bytes) > 4096:
            raise ValidationError("压缩包密码长度必须为1到4096个UTF-8字节")

        archive = self.get_file_detail(file_id)
        if expected_archive is not None:
            identity_fields = ("file_id", "parent_file_id", "filename", "size", "etag", "update_at")
            if archive is None or any(getattr(archive, key) != getattr(expected_archive, key)
                                      for key in identity_fields):
                raise ValidationError("压缩包在处理期间已移动或变化，请重新扫描目录")
        from .split_archive import SPLIT_7Z
        split = SPLIT_7Z.fullmatch(archive.filename) if archive is not None else None
        supported = archive is not None and (split or archive.filename.lower().endswith((".zip", ".7z", ".7zz", ".rar")))
        if archive_kind is not None:
            supported = archive_kind in (".zip", ".7z", ".rar")
        try:
            archive_type = (int(archive.type) if archive is not None and
                            not isinstance(archive.type, bool) else -1)
        except (TypeError, ValueError):
            archive_type = -1
        if (archive is None or archive.file_id != file_id or archive_type != 0 or
                _is_trashed({"trashed": archive.trashed}) or
                not supported):
            raise ValidationError("file_id 必须指向未删除的压缩包普通文件")
        parent_id = archive.parent_file_id
        if (not isinstance(parent_id, int) or isinstance(parent_id, bool) or
                parent_id < 0):
            raise ValidationError("压缩包文件缺少有效的父目录ID")
        archive_name = archive.filename
        if (not archive_name or os.path.basename(archive_name) != archive_name or
                re.search(r'[\\/:*?"<>|]', archive_name)):
            raise ValidationError("压缩包文件名无效")
        sidecar_name = (split[1] if split else archive_name) + ".pwd"
        if len(sidecar_name.encode("utf-8")) > 255:
            raise ValidationError("密码侧车文件名超过255个UTF-8字节")

        if skip_existing:
            from .archive_password_batch import password_directory
            siblings = password_directory(self, parent_id)
        else:
            siblings, _ = self.list_files(
                parent_id=parent_id, auto_fetch_all=True, use_cache=False)
        matches = [item for item in siblings if item.filename == sidecar_name]
        if skip_existing and matches:
            return {"skipped": True, "filename": sidecar_name}
        if skip_existing:
            archives = [item for item in siblings if item.filename == archive_name]
            if len(archives) != 1 or archives[0].file_id != file_id:
                raise ValidationError("压缩包名称不唯一或目录已变化，请重新扫描目录")
        if len(matches) > 1:
            raise ValidationError("同目录存在多个同名密码侧车，拒绝覆盖")
        if matches and matches[0].is_folder:
            raise ValidationError("同名密码侧车路径是目录，拒绝覆盖")

        fd, temp_path = tempfile.mkstemp(prefix="pan123-zip-password-")
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "wb") as password_file:
                fd = -1
                password_file.write(password_bytes)
                password_file.flush()
            result = self.upload_file(
                temp_path, parent_id, filename=sidecar_name, duplicate=1 if skip_existing else 2,
                skip_if_exists=False, try_sha1_reuse=False, sensitive=True)
            if not isinstance(result, dict) or result.get("fileID") is None:
                raise FileUploadError("密码侧车上传失败")
            self._invalidate_dir_cache(parent_id)
            return result
        finally:
            if fd >= 0:
                os.close(fd)
            try:
                os.remove(temp_path)
            except FileNotFoundError:
                pass

    def get_files_info(self, file_ids: List[int], use_cache: bool = True) -> FileList:
        """
        获取多个文件的详情信息，返回FileList对象

        :param file_ids: 文件ID列表
        :param use_cache: 是否使用缓存，默认为True
        :return: FileList对象
        """
        if not file_ids or not isinstance(file_ids, list):
            raise ValidationError("file_ids 必须是一个非空的列表")

        # 验证所有ID都是数字
        for file_id in file_ids:
            if not isinstance(file_id, int):
                raise ValidationError(f"文件ID必须是整数，获得: {type(file_id)}")

        # Deduplicate while preserving caller order; the infos endpoint accepts
        # batches, so large inputs are split into bounded requests.
        file_ids = list(dict.fromkeys(file_ids))

        # 如果不使用缓存或缓存不可用，直接调用API
        if not use_cache or not self.cache_manager:
            return self._fetch_files_info_from_api(file_ids)

        # 使用缓存逻辑
        cached_files = {}
        missing_file_ids = []

        # 检查每个文件ID的缓存状态
        for file_id in file_ids:
            should_use_cache, cached_data = self.cache_manager.should_use_cache(
                file_id)
            if should_use_cache and cached_data:
                cached_files[file_id] = cached_data
                logger.info(f"使用缓存获取文件信息: {file_id}")
            else:
                missing_file_ids.append(file_id)

        # 如果所有文件都有缓存，直接返回
        if not missing_file_ids:
            return FileList([cached_files[file_id] for file_id in file_ids])

        # 从API获取缺失的文件信息
        logger.info(f"从API获取文件信息: {missing_file_ids}")
        api_files = self._fetch_files_info_from_api(missing_file_ids)

        # 缓存新获取的文件信息
        for file_info in api_files.files:
            file_id = file_info.file_id
            if file_id:
                update_time = file_info.update_at
                should_use_cache, _ = self.cache_manager.should_use_cache(
                    file_id, update_time)

                if not should_use_cache:
                    self.cache_manager.set_cache(file_id, file_info.to_dict())
                    logger.info(f"文件信息已缓存: {file_id}")

        # 合并缓存和API结果
        api_by_id = {f.file_id: f.to_dict() for f in api_files.files}
        combined = {**cached_files, **api_by_id}
        return FileList([combined[file_id] for file_id in file_ids if file_id in combined])

    def _fetch_files_info_from_api(self, file_ids: List[int]) -> FileList:
        """从API获取文件信息的内部方法"""
        endpoint = "/api/v1/file/infos"
        files_by_id = {}
        for start in range(0, len(file_ids), FILE_INFO_BATCH_SIZE):
            json_data = {"fileIds": file_ids[start:start + FILE_INFO_BATCH_SIZE]}
            result = self.http_client.post(endpoint, json_data=json_data)
            if result and 'data' in result and 'fileList' in result['data']:
                for item in result['data']['fileList']:
                    file_id = item.get('fileId', item.get('fileID'))
                    if file_id is not None:
                        if isinstance(file_id, str) and file_id.isdigit():
                            file_id = int(file_id)
                        item = dict(item, fileId=file_id)
                        if "parentFileID" in item and "parentFileId" not in item:
                            item = dict(item, parentFileId=item["parentFileID"])
                        files_by_id[file_id] = item
        return FileList([files_by_id[file_id] for file_id in file_ids if file_id in files_by_id])

    def get_file_info_single(self, file_id: int, use_cache: bool = True) -> Optional[File]:
        """
        获取单个文件的详情信息，支持缓存

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :return: File对象，如果文件不存在返回None
        """
        file_list = self.get_files_info([file_id], use_cache=use_cache)

        if file_list and len(file_list) > 0:
            return file_list[0]

        return None

    def _collect_path_components(self, file_id: int, use_cache: bool = True, max_retries: int = 3) -> Optional[List[File]]:
        """获取从根到目标文件的路径组件。"""
        try:
            path_components: List[File] = []
            current_file_id = file_id

            logger.info(f"开始构建路径，文件ID: {file_id}")

            while current_file_id is not None and current_file_id != 0:
                file_info = self._get_file_info_with_retry(
                    current_file_id, use_cache=use_cache, max_retries=max_retries)

                if not file_info:
                    logger.warning(f"无法获取文件信息，文件ID: {current_file_id}")
                    return None

                path_components.append(file_info)
                logger.info(
                    f"添加路径组件: {file_info.filename} (ID: {current_file_id}, 父ID: {file_info.parent_file_id})")

                if file_info.parent_file_id == 0 or file_info.parent_file_id is None:
                    logger.info("已到达根目录")
                    break

                current_file_id = file_info.parent_file_id

            path_components.reverse()
            return path_components

        except Exception as e:
            logger.error(f"获取路径组件时发生错误: {e}")
            return None

    def get_file_path(self, file_id: int, use_cache: bool = True, max_retries: int = 3) -> Optional[str]:
        """
        获取文件的完整路径

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :param max_retries: 最大重试次数，默认为3次
        :return: 文件的完整路径，如果文件不存在返回None
        """
        try:
            path_components = self._collect_path_components(
                file_id, use_cache=use_cache, max_retries=max_retries)

            if path_components is None:
                return None

            if path_components:
                full_path = "/" + \
                    "/".join(comp.filename for comp in path_components)
                logger.info(f"构建完成的路径: {full_path}")
                return full_path

            logger.info("路径为空，返回根目录")
            return "/"

        except Exception as e:
            logger.error(f"获取文件路径时发生错误: {e}")
            return None

    def _get_file_info_with_retry(self, file_id: int, use_cache: bool = True, max_retries: int = 3) -> Optional[File]:
        """
        带重试功能的文件信息获取方法，专门处理429错误

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存
        :param max_retries: 最大重试次数
        :return: File对象，如果文件不存在返回None
        """
        # http_client 已经实现重试策略，对于 429 等业务码也会自动重试，
        # 所以这里直接调用单次接口并把异常向上抛出或返回结果。
        try:
            return self.get_file_info_single(file_id, use_cache=use_cache)
        except Pan123APIError:
            # 上层根据需要处理错误（日志/抛出）；这里返回 None 保持原来调用方行为
            raise

    def get_file_path_with_details(self, file_id: int, use_cache: bool = True, max_retries: int = 3) -> Optional[Dict[str, Any]]:
        """
        获取文件的完整路径及详细信息

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :param max_retries: 最大重试次数，默认为3次
        :return: 包含路径和详细信息的字典，如果文件不存在返回None
        """
        try:
            path_files = self._collect_path_components(
                file_id, use_cache=use_cache, max_retries=max_retries)

            if path_files is None:
                return None

            if not path_files:
                return {
                    "full_path": "/",
                    "path_components": [],
                    "depth": 0,
                    "target_file": None
                }

            path_components = [{
                "file_id": file_info.file_id,
                "name": file_info.filename,
                "is_folder": file_info.is_folder,
                "parent_id": file_info.parent_file_id,
                "size": file_info.size,
                "size_formatted": file_info.size_formatted
            } for file_info in path_files]

            path_names = [comp["name"] for comp in path_components]
            full_path = "/" + "/".join(path_names)

            result = {
                "full_path": full_path,
                "path_components": path_components,
                "depth": len(path_components),
                "target_file": path_components[-1] if path_components else None
            }

            logger.info(f"构建完成的详细路径: {full_path}")
            return result

        except Exception as e:
            logger.error(f"获取详细文件路径时发生错误: {e}")
            return None

    def clear_file_cache(self, file_id: int = None):
        """
        清除文件缓存

        :param file_id: 指定文件ID，如果为None则清除所有缓存
        """
        if not self.cache_manager:
            return

        if file_id is not None:
            self.cache_manager.delete_cache(file_id)
            logger.info(f"已清除文件 {file_id} 的缓存")
        else:
            self.cache_manager.clear_all_cache()
            logger.info("已清除所有文件缓存")

    def get_webdav_url(self, file_id: int, use_cache: bool = True) -> Optional[str]:
        """
        获取文件的WebDAV URL

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :return: WebDAV格式的URL，如果文件不存在或配置错误则返回None
        """
        # 获取文件路径
        file_path = self.get_file_path(file_id, use_cache=use_cache)
        if not file_path:
            logger.warning(f"无法获取文件路径，文件ID: {file_id}")
            return None

        # 检查配置是否包含WebDAV所需信息
        webdav_user = self.config.get('webdav_user')
        webdav_password = self.config.get('webdav_password')
        webdav_host = self.config.get(
            'webdav_host', 'webdav-1836076489.pd1.123pan.cn')

        if not webdav_user or not webdav_password:
            logger.warning("缺少WebDAV配置：用户名或密码未设置")
            return None

        # 构建WebDAV URL，去掉路径开头的斜杠
        if file_path.startswith('/'):
            file_path = file_path[1:]

        # 对文件路径进行URL编码
        encoded_file_path = quote(file_path)

        # 路径前缀来自配置，默认 /webdav
        path_prefix = (self.config.get('webdav_path_prefix')
                       or WEBDAV_DEFAULT_PATH).strip('/')
        path = f"/{path_prefix}/{encoded_file_path}" if path_prefix else f"/{encoded_file_path}"

        # 用户名/密码需要转义，否则密码里的 @ : / 会破坏 URL 结构
        credentials = f"{quote(webdav_user, safe='')}:{quote(webdav_password, safe='')}"

        webdav_url = f"https://{credentials}@{webdav_host}{path}"
        logger.info(
            f"已生成WebDAV URL，文件ID: {file_id}, URL: {_mask_credentials(webdav_url)}")

        return webdav_url

    def get_webdav_redirect_url(self, file_id: int, use_cache: bool = True, max_redirects: int = 5) -> Optional[str]:
        """
        获取文件的WebDAV URL并跟随302跳转，返回最终的下载URL

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :param max_redirects: 最大跳转次数，防止无限循环，默认5次
        :return: 跳转后的最终下载URL，如果文件不存在或配置错误则返回None
        """
        import requests
        from requests.exceptions import RequestException

        # 先获取WebDAV URL
        webdav_url = self.get_webdav_url(file_id, use_cache=use_cache)
        if not webdav_url:
            logger.warning(f"无法获取WebDAV URL，文件ID: {file_id}")
            return None

        current_url = webdav_url
        redirect_count = 0

        try:
            while redirect_count < max_redirects:
                logger.info(
                    f"探测WebDAV跳转 (跳转次数: {redirect_count}): {_mask_credentials(current_url)}")

                # 不允许自动跳转；stream=True 保证不把响应体读进内存
                # （文件较大且服务端直接返回 200 时，否则会整份缓冲到内存里）
                with requests.get(
                        current_url, allow_redirects=False, timeout=30, stream=True) as response:

                    logger.info(f"响应状态码: {response.status_code}")

                    # 检查是否是跳转响应
                    if response.status_code in [301, 302, 303, 307, 308]:
                        redirect_url = response.headers.get('Location')
                        if not redirect_url:
                            logger.warning(f"{response.status_code}响应中没有找到Location头")
                            return None

                        logger.info(f"获取到{response.status_code}跳转URL: {_mask_credentials(redirect_url)}")
                        return redirect_url

                    elif response.status_code == 200:
                        # 如果返回200，说明到达最终URL
                        logger.info(f"到达最终URL，状态码: {response.status_code}")
                        return current_url

                    elif response.status_code == 404:
                        logger.warning(f"文件未找到，状态码: {response.status_code}")
                        return None

                    else:
                        logger.error(f"WebDAV请求返回错误状态码: {response.status_code}")
                        # 对于其他状态码，尝试返回响应内容以便调试
                        if hasattr(response, 'text'):
                            logger.error(f"响应内容: {response.text[:500]}...")
                        return None

                redirect_count += 1

            logger.warning(
                f"达到最大跳转次数限制({max_redirects})，最终URL: {_mask_credentials(current_url)}")
            return current_url

        except RequestException as e:
            logger.error(f"请求WebDAV URL时发生网络错误: {e}")
            return None
        except Exception as e:
            logger.error(f"获取WebDAV跳转URL时发生未知错误: {e}")
            return None

    def get_final_download_url(self, file_id: int, prefer_webdav: bool = True, use_cache: bool = True) -> Optional[tuple[str, str]]:
        """
        获取文件的最终可下载URL，优先使用WebDAV或API下载链接

        :param file_id: 文件ID
        :param prefer_webdav: 是否优先使用WebDAV，默认为True
        :param use_cache: 是否使用缓存，默认为True
        :return: (url, url_type) 元组，url_type 为 'webdav' 或 'api'；获取失败则返回 None
        """
        if prefer_webdav:
            webdav_url = self.get_webdav_redirect_url(file_id, use_cache=use_cache)
            if webdav_url:
                logger.info(f"成功获取WebDAV下载URL，文件ID: {file_id}")
                return webdav_url, "webdav"

            logger.warning(f"WebDAV获取失败，尝试使用API下载链接，文件ID: {file_id}")

        try:
            download_info = self.get_download_info(file_id)
            if download_info and 'data' in download_info:
                download_url = download_info['data'].get('downloadUrl')
                if download_url:
                    logger.info(f"成功获取API下载URL，文件ID: {file_id}")
                    return download_url, "api"
        except Exception as e:
            logger.error(f"获取API下载链接时发生错误: {e}")

        logger.warning(f"无法获取任何下载URL，文件ID: {file_id}")
        return None

    def mkdir(self, name: str, parent_id: int) -> int:
        """
        创建目录（带进程内缓存，同一父目录下的同名目录只请求一次）
        :param name: 目录名(注:不能重名)
        :param parent_id: 父目录id，上传到根目录时填写 0
        :return: 创建的目录ID
        """
        cache_key = (parent_id, name)
        with self._cache_lock:
            if cache_key in self._mkdir_cache:
                return self._mkdir_cache[cache_key]

        try:
            endpoint = "/upload/v1/file/mkdir"
            json_data = {"name": name, "parentID": parent_id}
            result = self.http_client.post(endpoint, json_data=json_data)
            if result and 'data' in result:
                dir_id = result['data'].get('dirID')
                with self._cache_lock:
                    self._mkdir_cache[cache_key] = dir_id
                self._invalidate_dir_cache(parent_id)
                return dir_id
            raise Exception("mkdir API 未返回 dirID")
        except Exception as e:
            # 目录已存在时 mkdir 会失败（接口不能重名），从文件列表中查找同名目录
            try:
                # 先读缓存（刚建过目录时可能已经在别的调用里命中过），
                # 命中不了再用一次不读缓存的完整查询
                for use_cache in (True, False):
                    file_list, _ = self.list_files(
                        parent_id=parent_id, limit=100,
                        auto_fetch_all=True, use_cache=use_cache)
                    for file_item in file_list.files:
                        if file_item.filename == name and file_item.is_folder:
                            logger.info(f"找到已存在的目录: {name}, ID: {file_item.file_id}")
                            with self._cache_lock:
                                self._mkdir_cache[cache_key] = file_item.file_id
                            return file_item.file_id
                # 如果没有找到同名目录，重新抛出原始异常
                logger.warning(f"未找到同名目录: {name}")
                raise e

            except Exception as list_error:
                logger.error(f"获取文件列表失败: {list_error}")
                raise e

    def mkdir_recursive(self, path: str, parent_id: int = 0) -> int:
        """
        递归创建目录
        :param path: 目录路径，如 "foo/bar/baz"
        :param parent_id: 父目录id，上传到根目录时填写 0
        :return: 创建的最终目录ID
        """
        if not path or not path.strip():
            raise ValidationError("目录路径不能为空")

        # 移除开头和结尾的斜杠，并分割路径
        path = path.strip('/')
        if not path:
            return parent_id

        path_parts = path.split('/')
        current_parent_id = parent_id

        logger.info(f"开始递归创建目录: {path}, 父目录ID: {parent_id}")

        for i, dir_name in enumerate(path_parts):
            if not dir_name.strip():
                continue

            logger.info(f"创建目录: {dir_name} (父ID: {current_parent_id})")

            try:
                # 使用自己的 mkdir 方法创建目录
                current_parent_id = self.mkdir(dir_name, current_parent_id)
                logger.info(f"成功创建/找到目录: {dir_name}, ID: {current_parent_id}")
            except Exception as e:
                logger.error(f"创建目录失败: {dir_name}, 错误: {e}")
                raise e

        logger.info(f"递归创建目录完成，最终目录ID: {current_parent_id}")
        return current_parent_id
