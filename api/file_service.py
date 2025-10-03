"""
文件操作服务
"""
import hashlib
import os
import re
import time
from typing import List, Dict, Any, Tuple, Optional
from urllib.parse import quote  # 添加导入
from .http_client import RequestHandler
from .cache import FileCacheManager
from .exceptions import ValidationError, Pan123APIError
from .models import File, FileList


class FileService:
    """文件操作服务"""

    def __init__(self, http_client: RequestHandler, cache_manager: FileCacheManager = None, config: Dict[str, Any] = None):
        self.http_client = http_client
        self.cache_manager = cache_manager
        self.config = config or {}
        self._dir_cache: Dict[int, Tuple[FileList, Optional[int]]] = {}

    @staticmethod
    def _format_file_size(size_bytes: int) -> str:
        """将字节数格式化为对人友好的字符串（GB/MB/KB/字节）。"""
        try:
            size = int(size_bytes)
        except Exception:
            return str(size_bytes)

        if size >= 1024 * 1024 * 1024:
            return f"{size / (1024 * 1024 * 1024):.2f} GB"
        elif size >= 1024 * 1024:
            return f"{size / (1024 * 1024):.2f} MB"
        elif size >= 1024:
            return f"{size / 1024:.2f} KB"
        else:
            return f"{size} 字节"

    def list_files(self,
                   parent_id: int = 0,
                   limit: int = 100,
                   search_data: str = None,
                   search_mode: int = None,
                   last_file_id: int = None,
                   auto_fetch_all: bool = False,
                   qps_limit: float = 1.0,
                   max_pages: int = 100,
                   use_cache: bool = True) -> Tuple[FileList, Optional[int]]:
        """
        列出文件并返回FileList对象

        :param parent_id: 父目录ID，默认为0（根目录）
        :param limit: 返回的文件数量限制，默认100
        :param search_data: 搜索关键词
        :param search_mode: 搜索模式
        :param last_file_id: 分页参数
        :param auto_fetch_all: 是否自动获取所有分页，默认False
        :param qps_limit: QPS限制（每秒请求数），默认1.0
        :param max_pages: 最大页数限制，默认100页
        :return: (FileList对象, next_last_file_id)
        """
        # 仅在获取所有页面且不搜索时使用缓存
        use_dir_cache = auto_fetch_all and not search_data and use_cache
        if use_dir_cache and parent_id in self._dir_cache:
            print(f"使用目录缓存: parent_id={parent_id}")
            return self._dir_cache[parent_id]

        if auto_fetch_all:
            result = self._fetch_all_pages(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                qps_limit=qps_limit,
                max_pages=max_pages
            )
            if use_dir_cache:
                print(f"缓存目录列表: parent_id={parent_id}")
                self._dir_cache[parent_id] = result
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
        """获取单页数据"""
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
            return FileList([]), None

        raw_file_list = result['data'].get('fileList', [])
        # 过滤掉已被移入垃圾桶的文件（trashed == 1）
        try:
            file_list = [f for f in raw_file_list if int(
                f.get('trashed', 0)) != 1]
        except Exception:
            # 如果数据格式异常，回退到不抛出错误的原始列表
            file_list = [f for f in raw_file_list if not (isinstance(
                f.get('trashed', None), int) and f.get('trashed') == 1)]

        next_last_file_id = result['data'].get('lastFileId')

        return FileList(file_list), next_last_file_id

    def _fetch_all_pages(self,
                         parent_id: int = 0,
                         limit: int = 100,
                         search_data: str = None,
                         search_mode: int = None,
                         qps_limit: float = 1.0,
                         max_pages: int = 100) -> Tuple[FileList, Optional[int]]:
        """
        自动获取所有分页数据，带QPS限制

        :param parent_id: 父目录ID
        :param limit: 每页限制
        :param search_data: 搜索关键词
        :param search_mode: 搜索模式
        :param qps_limit: QPS限制（每秒请求数）
        :param max_pages: 最大页数限制，默认100页
        :return: (合并的FileList对象, None)
        """
        all_files = []
        last_file_id = None
        page_count = 0
        last_request_time = 0

        print(f"开始获取所有分页数据，QPS限制: {qps_limit} req/s，最大页数: {max_pages}")

        while True:
            # QPS 限制：确保请求间隔至少为 1/qps_limit 秒
            if page_count > 0:  # 第一次请求不需要等待
                min_interval = 1.0 / qps_limit
                current_time = time.time()
                elapsed = current_time - last_request_time

                if elapsed < min_interval:
                    wait_time = min_interval - elapsed
                    print(f"QPS限制等待 {wait_time:.2f} 秒...")
                    time.sleep(wait_time)

            last_request_time = time.time()

            # 获取当前页数据
            file_list, next_last_file_id = self._fetch_single_page(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                last_file_id=last_file_id
            )

            page_count += 1
            current_page_count = len(file_list.files)
            all_files.extend(file_list.files)

            print(
                f"第 {page_count} 页: 获取 {current_page_count} 个文件，累计 {len(all_files)} 个")

            # 检查是否还有更多页
            # next_last_file_id 为 None、-1 或者当前页没有文件时停止分页
            if next_last_file_id is None or next_last_file_id == -1 or current_page_count == 0:
                if next_last_file_id == -1:
                    print(
                        f"已到达最后一页（next_file_id = -1），共 {page_count} 页，总计 {len(all_files)} 个文件")
                else:
                    print(f"分页获取完成，共 {page_count} 页，总计 {len(all_files)} 个文件")
                break

            # 检查是否达到最大页数限制
            if page_count >= max_pages:
                print(f"已达到最大页数限制（{max_pages} 页），共获取 {len(all_files)} 个文件")
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
                    contain_dir: bool = False) -> Dict[str, Any]:
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
                raise ValidationError('文件名包含非法字符: \\\/:*?"<>|')
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
            print(f"预上传失败: {e}, 尝试检查文件是否已存在...")
            try:
                remote_files_list, _ = self.list_files(
                    parent_id=parent_id, auto_fetch_all=True)
                existing_file = remote_files_list.find_by_name(filename)
                if existing_file and not existing_file.is_folder and existing_file.size == size:
                    print(
                        f"  ✓ 找到已存在的文件 '{filename}' 且大小相同，返回现有文件信息。")
                    return {
                        "fileID": existing_file.file_id,
                        "filename": filename,
                        "size": size,
                        "skipped": True,
                        "reuse": True  # 模拟秒传成功
                    }
                raise e
            except Exception as list_error:
                print(f"检查已存在文件时出错: {list_error}")
                raise e

    def upload_file(self,
                    local_path: str,
                    parent_id: int,
                    filename: str = None,
                    duplicate: int = 1,
                    skip_if_exists: bool = False) -> Optional[Dict[str, Any]]:
        """
        上传完整文件，处理预上传、分片上传和完成上传的整个流程。

        :param local_path: 本地文件路径
        :param parent_id: 上传到的父目录ID
        :param filename: 在云端保存的文件名，如果为None则使用本地文件名
        :param duplicate: 文件名冲突策略 (1: 保留两者, 2: 覆盖)
        :param skip_if_exists: 如果为True，且远程存在同名同大小文件，则跳过上传
        :return: 成功则返回文件信息字典，否则返回None
        """
        # 1. 检查文件是否存在
        if not os.path.exists(local_path):
            raise FileNotFoundError(f"文件不存在: {local_path}")

        # 2. 获取文件名和大小
        if filename is None:
            filename = os.path.basename(local_path)
        size = os.path.getsize(local_path)

        # 友好格式化大小
        size_friendly = self._format_file_size(size)

        # 2.1. 如果设置了 skip_if_exists，检查远程文件
        if skip_if_exists:
            print(f"检查远程文件是否存在: '{filename}' in parent {parent_id}")
            # 这里我们假设list_files能获取所有文件，对于大目录可能需要分页
            remote_files_list, _ = self.list_files(
                parent_id=parent_id, auto_fetch_all=True)
            existing_file = remote_files_list.find_by_name(filename)
            if existing_file and not existing_file.is_folder:
                if existing_file.size == size:
                    print(f"  ✓ 文件 '{filename}' 已存在且大小相同，跳过上传。")
                    return {
                        "fileID": existing_file.file_id,
                        "filename": filename,
                        "size": size,
                        "skipped": True
                    }
                else:
                    print(
                        f"  ! 文件 '{filename}' 已存在但大小不同 (本地: {size}, 远程: {existing_file.size})，继续上传。")

        # 3. 计算MD5
        etag = self._calculate_md5(local_path)
        print(
            f"开始上传文件: '{filename}', 大小: {size} bytes ({size_friendly}), MD5: {etag}")

        # 4. 调用 create_file (预上传)
        try:
            pre_upload_info = self.create_file(
                parent_id=parent_id,
                filename=filename,
                etag=etag,
                size=size,
                duplicate=duplicate
            )
        except ValidationError as e:
            print(f"预上传失败: {e}")
            return None

        # 5. 检查是否秒传
        if pre_upload_info.get("reuse"):
            print("文件秒传成功")
            return {
                "fileID": pre_upload_info.get("fileID"),
                "filename": filename,
                "size": size,
                "reuse": True
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
            print(
                f"需要分片上传. Pre-upload ID: {preupload_id}, 分片大小: {slice_size} bytes, 预计分片数: {estimated_parts}")
        else:
            print(f"需要分片上传. Pre-upload ID: {preupload_id}, 分片大小: {slice_size}")

        # 7. 上传分片
        upload_success = self._upload_chunks(
            local_path, preupload_id, slice_size, servers)

        if not upload_success:
            print("分片上传失败")
            return None

        # 8. 完成上传
        complete_info = self._complete_upload(preupload_id)

        if complete_info:
            print("文件上传成功")
            return complete_info
        else:
            print("完成上传步骤失败")
            return None

    def _calculate_md5(self, file_path: str, chunk_size: int = 8192) -> str:
        """计算文件的MD5值"""
        md5 = hashlib.md5()
        with open(file_path, 'rb') as f:
            while chunk := f.read(chunk_size):
                md5.update(chunk)
        return md5.hexdigest()

    def _upload_chunks(self, local_path: str, preupload_id: str, slice_size: int, servers: List[str]) -> bool:
        """
        读取文件并上传所有分片。
        """
        print("开始上传分片...")
        with open(local_path, 'rb') as f:
            part_number = 1
            server_count = len(servers)
            if server_count == 0:
                print("错误：没有可用的上传服务器。")
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

                print(
                    f"  上传分片 {part_number} (大小: {len(chunk)} bytes, MD5: {slice_md5}) 到 {endpoint}...")

                try:
                    # 假设 http_client.post 可以通过 `data` 和 `files` 参数处理 multipart/form-data
                    result = self.http_client.post(
                        endpoint, data=form_data, files=files_data)

                    if not result:
                        print(f"  上传分片 {part_number} 失败 (无返回结果)。")
                        return False

                    # 假设API成功时返回的json包含 code: 0
                    if result.get('code') != 0:
                        print(
                            f"  上传分片 {part_number} 失败: {result.get('message', '未知错误')}")
                        return False

                except Exception as e:
                    print(f"  上传分片 {part_number} 时发生网络或客户端错误: {e}")
                    return False

                print(f"  分片 {part_number} 上传成功。")
                part_number += 1

        print("所有分片上传成功。")
        return True

    def _complete_upload(self, preupload_id: str, max_retries: int = 5, retry_delay: int = 2) -> Optional[Dict[str, Any]]:
        """
        通知服务器所有分片已上传完毕。
        包含针对“文件校验中”错误的重试逻辑。
        """
        print(f"正在发送上传完成请求, preuploadID: {preupload_id}...")

        endpoint = "/upload/v2/file/upload_complete"
        json_data = {"preuploadID": preupload_id}

        # 将重试责任交给 http_client（网络层）。这里直接调用一次，
        # http_client 会根据配置对网络/业务码（如20103）进行重试。
        try:
            result = self.http_client.post(endpoint, json_data=json_data)
            data = result.get('data', {})
            if data.get('completed'):
                print(f"文件上传成功! FileID: {data.get('fileID')}")
                return data
            print("完成上传请求返回未完成状态。")
            return None
        except Pan123APIError as e:
            print(f"完成上传请求失败: {e}")
            return None
        except Exception as e:
            print(f"完成上传请求时发生未知异常: {e}")
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

        # 如果不使用缓存或缓存不可用，直接调用API
        if not use_cache or not self.cache_manager:
            return self._fetch_files_info_from_api(file_ids)

        # 使用缓存逻辑
        cached_files = []
        missing_file_ids = []

        # 检查每个文件ID的缓存状态
        for file_id in file_ids:
            should_use_cache, cached_data = self.cache_manager.should_use_cache(
                file_id)
            if should_use_cache and cached_data:
                cached_files.append(cached_data)
                print(f"使用缓存获取文件信息: {file_id}")
            else:
                missing_file_ids.append(file_id)

        # 如果所有文件都有缓存，直接返回
        if not missing_file_ids:
            return FileList(cached_files)

        # 从API获取缺失的文件信息
        print(f"从API获取文件信息: {missing_file_ids}")
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
                    print(f"文件信息已缓存: {file_id}")

        # 合并缓存和API结果
        all_files_data = [f.to_dict()
                          for f in cached_files] if cached_files else []
        all_files_data.extend([f.to_dict() for f in api_files.files])

        return FileList(all_files_data)

    def _fetch_files_info_from_api(self, file_ids: List[int]) -> FileList:
        """从API获取文件信息的内部方法"""
        endpoint = "/api/v1/file/infos"
        json_data = {"fileIds": file_ids}

        result = self.http_client.post(endpoint, json_data=json_data)

        if result and 'data' in result and 'fileList' in result['data']:
            files_data = result['data']['fileList']
            return FileList(files_data)

        return FileList([])

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

    def get_file_path(self, file_id: int, use_cache: bool = True, max_retries: int = 3) -> Optional[str]:
        """
        获取文件的完整路径

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :param max_retries: 最大重试次数，默认为3次
        :return: 文件的完整路径，如果文件不存在返回None
        """
        try:
            path_parts = []
            current_file_id = file_id

            print(f"开始构建文件路径，文件ID: {file_id}")

            while current_file_id is not None and current_file_id != 0:
                # 获取当前文件信息，带重试逻辑
                file_info = self._get_file_info_with_retry(
                    current_file_id, use_cache=use_cache, max_retries=max_retries)

                if not file_info:
                    print(f"无法获取文件信息，文件ID: {current_file_id}")
                    return None

                # 将当前文件名添加到路径组件中
                path_parts.append(file_info.filename)
                print(
                    f"添加路径组件: {file_info.filename} (ID: {current_file_id}, 父ID: {file_info.parent_file_id})")

                # 检查是否到达根目录
                if file_info.parent_file_id == 0 or file_info.parent_file_id is None:
                    print("已到达根目录")
                    break

                # 移动到父目录
                current_file_id = file_info.parent_file_id

            # 反转路径组件（因为我们是从叶子节点向根节点遍历的）
            path_parts.reverse()

            # 构建完整路径
            if path_parts:
                full_path = "/" + "/".join(path_parts)
                print(f"构建完成的路径: {full_path}")
                return full_path
            else:
                print("路径为空，返回根目录")
                return "/"

        except Exception as e:
            print(f"获取文件路径时发生错误: {e}")
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
            path_components = []
            current_file_id = file_id

            print(f"开始构建详细路径信息，文件ID: {file_id}")

            while current_file_id is not None and current_file_id != 0:
                # 获取当前文件信息，带重试逻辑
                file_info = self._get_file_info_with_retry(
                    current_file_id, use_cache=use_cache, max_retries=max_retries)

                if not file_info:
                    print(f"无法获取文件信息，文件ID: {current_file_id}")
                    return None

                # 添加路径组件信息
                component = {
                    "file_id": file_info.file_id,
                    "name": file_info.filename,
                    "is_folder": file_info.is_folder,
                    "parent_id": file_info.parent_file_id,
                    "size": file_info.size,
                    "size_formatted": file_info.size_formatted
                }
                path_components.append(component)

                print(
                    f"添加详细路径组件: {file_info.filename} (ID: {current_file_id})")

                # 检查是否到达根目录
                if file_info.parent_file_id == 0 or file_info.parent_file_id is None:
                    print("已到达根目录")
                    break

                # 移动到父目录
                current_file_id = file_info.parent_file_id

            # 反转路径组件
            path_components.reverse()

            # 构建结果
            if path_components:
                path_names = [comp["name"] for comp in path_components]
                full_path = "/" + "/".join(path_names)

                result = {
                    "full_path": full_path,
                    "path_components": path_components,
                    "depth": len(path_components),
                    "target_file": path_components[-1] if path_components else None
                }

                print(f"构建完成的详细路径: {full_path}")
                return result
            else:
                return {
                    "full_path": "/",
                    "path_components": [],
                    "depth": 0,
                    "target_file": None
                }

        except Exception as e:
            print(f"获取详细文件路径时发生错误: {e}")
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
            print(f"已清除文件 {file_id} 的缓存")
        else:
            self.cache_manager.clear_all_cache()
            print("已清除所有文件缓存")

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
            print(f"无法获取文件路径，文件ID: {file_id}")
            return None

        # 检查配置是否包含WebDAV所需信息
        webdav_user = self.config.get('webdav_user')
        webdav_password = self.config.get('webdav_password')
        webdav_host = self.config.get(
            'webdav_host', 'webdav-1836076489.pd1.123pan.cn')

        if not webdav_user or not webdav_password:
            print("缺少WebDAV配置：用户名或密码未设置")
            return None

        # 构建WebDAV URL，去掉路径开头的斜杠
        if file_path.startswith('/'):
            file_path = file_path[1:]

        # 对文件路径进行URL编码
        encoded_file_path = quote(file_path)

        webdav_url = f"https://{webdav_user}:{webdav_password}@{webdav_host}/webdav/{encoded_file_path}"
        print(f"已生成WebDAV URL，文件ID: {file_id}")

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
            print(f"无法获取WebDAV URL，文件ID: {file_id}")
            return None

        current_url = webdav_url
        redirect_count = 0

        try:
            while redirect_count < max_redirects:
                print(f"发送HEAD请求到URL (跳转次数: {redirect_count}): {current_url}")

                # 发送HEAD请求，不允许自动跳转
                response = requests.get(
                    current_url, allow_redirects=False, timeout=30)

                print(f"响应状态码: {response.status_code}")

                # 检查是否是跳转响应
                if response.status_code in [301, 302, 303, 307, 308]:
                    redirect_url = response.headers.get('Location')
                    if not redirect_url:
                        print(f"{response.status_code}响应中没有找到Location头")
                        return None

                    print(f"获取到{response.status_code}跳转URL: {redirect_url}")
                    current_url = redirect_url
                    return current_url
                    redirect_count += 1

                elif response.status_code == 200:
                    # 如果返回200，说明到达最终URL
                    print(f"到达最终URL，状态码: {response.status_code}")
                    return current_url

                elif response.status_code == 404:
                    print(f"文件未找到，状态码: {response.status_code}")
                    return None

                else:
                    print(f"WebDAV请求返回错误状态码: {response.status_code}")
                    # 对于其他状态码，尝试返回响应内容以便调试
                    if hasattr(response, 'text'):
                        print(f"响应内容: {response.text[:500]}...")
                    return None

            print(f"达到最大跳转次数限制({max_redirects})，最终URL: {current_url}")
            return current_url

        except RequestException as e:
            print(f"请求WebDAV URL时发生网络错误: {e}")
            return None
        except Exception as e:
            print(f"获取WebDAV跳转URL时发生未知错误: {e}")
            return None

    def get_final_download_url(self, file_id: int, prefer_webdav: bool = True, use_cache: bool = True) -> Optional[str]:
        """
        获取文件的最终可下载URL，优先使用WebDAV或API下载链接

        :param file_id: 文件ID
        :param prefer_webdav: 是否优先使用WebDAV，默认为True
        :param use_cache: 是否使用缓存，默认为True
        :return: 最终的下载URL，如果获取失败则返回None
        """
        if prefer_webdav:
            # 优先尝试WebDAV
            webdav_url = self.get_webdav_redirect_url(
                file_id, use_cache=use_cache)
            if webdav_url:
                print(f"成功获取WebDAV下载URL，文件ID: {file_id}")
                return webdav_url

            print(f"WebDAV获取失败，尝试使用API下载链接，文件ID: {file_id}")

        # 尝试使用API获取下载链接
        try:
            download_info = self.get_download_info(file_id)
            if download_info and 'data' in download_info:
                download_url = download_info['data'].get('downloadUrl')
                if download_url:
                    print(f"成功获取API下载URL，文件ID: {file_id}")
                    return download_url
        except Exception as e:
            print(f"获取API下载链接时发生错误: {e}")

        print(f"无法获取任何下载URL，文件ID: {file_id}")
        return None

    def mkdir(self, name: str, parent_id: int) -> int:
        """
        创建目录
        :param name: 目录名(注:不能重名)
        :param parent_id: 父目录id，上传到根目录时填写 0
        :return: 创建的目录ID
        """
        try:
            endpoint = "/upload/v1/file/mkdir"
            json_data = {"name": name, "parentID": parent_id}
            result = self.http_client.post(endpoint, json_data=json_data)
            if result and 'data' in result:
                return result['data'].get('dirID')
            raise Exception("mkdir API 未返回 dirID")
        except Exception as e:
            # print(f"创建目录失败: {e}, 尝试检查目录是否已存在...")

            # 获取父目录下的文件列表
            try:
                file_list, _ = self.list_files(
                    parent_id=parent_id, limit=100, auto_fetch_all=True, use_cache=True)

                # 查找同名的文件夹
                for file_item in file_list.files:
                    if file_item.filename == name and file_item.is_folder:
                        print(f"找到已存在的目录: {name}, ID: {file_item.file_id}")
                        return file_item.file_id

                file_list, _ = self.list_files(
                    parent_id=parent_id, limit=100, auto_fetch_all=True, use_cache=False)

                # 查找同名的文件夹
                for file_item in file_list.files:
                    if file_item.filename == name and file_item.is_folder:
                        print(f"找到已存在的目录: {name}, ID: {file_item.file_id}")
                        return file_item.file_id
                # 如果没有找到同名目录，重新抛出原始异常
                print(f"未找到同名目录: {name}")
                raise e

            except Exception as list_error:
                print(f"获取文件列表失败: {list_error}")
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

        print(f"开始递归创建目录: {path}, 父目录ID: {parent_id}")

        for i, dir_name in enumerate(path_parts):
            if not dir_name.strip():
                continue

            print(f"创建目录: {dir_name} (父ID: {current_parent_id})")

            try:
                # 使用自己的 mkdir 方法创建目录
                current_parent_id = self.mkdir(dir_name, current_parent_id)
                print(f"成功创建/找到目录: {dir_name}, ID: {current_parent_id}")
            except Exception as e:
                print(f"创建目录失败: {dir_name}, 错误: {e}")
                raise e

        print(f"递归创建目录完成，最终目录ID: {current_parent_id}")
        return current_parent_id
