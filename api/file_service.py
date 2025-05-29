"""
文件操作服务
"""
import time
from typing import List, Dict, Any, Tuple, Optional
from urllib.parse import quote  # 添加导入
from .http_client import RequestHandler
from .cache import FileCacheManager
from .exceptions import ValidationError
from .models import File, FileList


class FileService:
    """文件操作服务"""

    def __init__(self, http_client: RequestHandler, cache_manager: FileCacheManager = None, config: Dict[str, Any] = None):
        self.http_client = http_client
        self.cache_manager = cache_manager
        self.config = config or {}

    def list_files(self,
                   parent_id: int = 0,
                   limit: int = 100,
                   search_data: str = None,
                   search_mode: int = None,
                   last_file_id: int = None,
                   auto_fetch_all: bool = False,
                   qps_limit: float = 1.0,
                   max_pages: int = 100) -> Tuple[FileList, Optional[int]]:
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
        if auto_fetch_all:
            return self._fetch_all_pages(
                parent_id=parent_id,
                limit=limit,
                search_data=search_data,
                search_mode=search_mode,
                qps_limit=qps_limit,
                max_pages=max_pages
            )
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

        file_list = result['data'].get('fileList', [])
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

    def get_file_path(self, file_id: int, use_cache: bool = True) -> Optional[str]:
        """
        获取文件的完整路径

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :return: 文件的完整路径，如果文件不存在返回None
        """
        try:
            path_parts = []
            current_file_id = file_id

            print(f"开始构建文件路径，文件ID: {file_id}")

            while current_file_id is not None and current_file_id != 0:
                # 获取当前文件信息
                file_info = self.get_file_info_single(
                    current_file_id, use_cache=use_cache)

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

    def get_file_path_with_details(self, file_id: int, use_cache: bool = True) -> Optional[Dict[str, Any]]:
        """
        获取文件的完整路径及详细信息

        :param file_id: 文件ID
        :param use_cache: 是否使用缓存，默认为True
        :return: 包含路径和详细信息的字典，如果文件不存在返回None
        """
        try:
            path_components = []
            current_file_id = file_id

            print(f"开始构建详细路径信息，文件ID: {file_id}")

            while current_file_id is not None and current_file_id != 0:
                # 获取当前文件信息
                file_info = self.get_file_info_single(
                    current_file_id, use_cache=use_cache)

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
                response = requests.head(current_url, allow_redirects=False, timeout=30)
                
                print(f"响应状态码: {response.status_code}")
                
                # 检查是否是跳转响应
                if response.status_code in [301, 302, 303, 307, 308]:
                    redirect_url = response.headers.get('Location')
                    if not redirect_url:
                        print(f"{response.status_code}响应中没有找到Location头")
                        return None
                    
                    print(f"获取到{response.status_code}跳转URL: {redirect_url}")
                    current_url = redirect_url
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
            webdav_url = self.get_webdav_redirect_url(file_id, use_cache=use_cache)
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
