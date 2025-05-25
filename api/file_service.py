"""
文件操作服务
"""
from typing import List, Dict, Any, Tuple, Optional
from .http_client import RequestHandler
from .cache import FileCacheManager
from .exceptions import ValidationError
from .models import File, FileList


class FileService:
    """文件操作服务"""

    def __init__(self, http_client: RequestHandler, cache_manager: FileCacheManager = None):
        self.http_client = http_client
        self.cache_manager = cache_manager

    def list_files(self,
                   parent_id: int = 0,
                   limit: int = 100,
                   search_data: str = None,
                   search_mode: int = None,
                   last_file_id: int = None) -> Tuple[FileList, Optional[int]]:
        """
        列出文件并返回FileList对象

        :param parent_id: 父目录ID，默认为0（根目录）
        :param limit: 返回的文件数量限制，默认100
        :param search_data: 搜索关键词
        :param search_mode: 搜索模式
        :param last_file_id: 分页参数
        :return: (FileList对象, next_last_file_id)
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
            return FileList([]), None

        file_list = result['data'].get('fileList', [])
        next_last_file_id = result['data'].get('lastFileId')

        return FileList(file_list), next_last_file_id

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
