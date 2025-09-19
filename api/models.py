#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
123云盘文件浏览器 - 数据模型
"""

import os
from datetime import datetime
from typing import Optional, Union


class File:
    """文件信息类"""

    def __init__(self, data: dict):
        """
        初始化文件对象
        :param data: 文件信息字典
        """
        self._data = data

        # 基本属性
        self.file_id = data.get('fileId')
        self.filename = data.get('filename', '')
        self.size = data.get('size', 0)
        self.type = data.get('type', 0)  # 0=文件, 1=文件夹
        self.category = data.get('category', 0)  # 0=未知, 1=音频, 2=视频, 3=图片

        # 时间属性
        self.create_at = data.get('createAt', '')
        self.update_at = data.get('updateAt', '')

        # 其他属性
        self.parent_file_id = data.get('parentFileId')
        self.etag = data.get('etag', '')
        self.storage_node = data.get('storageNode', '')
        self.status = data.get('status')
        self.hidden = data.get('hidden', False)
        self.starred = data.get('starred', False)
        self.trashed = data.get('trashed', False)

        # 处理后的属性
        self._size_formatted = None
        self._category_name = None
        self._icon = None
        self._is_folder = None

    @property
    def is_folder(self) -> bool:
        """是否为文件夹"""
        if self._is_folder is None:
            self._is_folder = self.type == 1
        return self._is_folder

    @property
    def size_formatted(self) -> str:
        """格式化的文件大小"""
        if self._size_formatted is None:
            self._size_formatted = self._format_file_size(self.size)
        return self._size_formatted

    @property
    def category_name(self) -> str:
        """文件分类名称"""
        if self._category_name is None:
            category_map = {0: "未知", 1: "音频", 2: "视频", 3: "图片"}
            self._category_name = category_map.get(self.category, "未知")
        return self._category_name

    @property
    def icon(self) -> str:
        """文件图标CSS类名"""
        if self._icon is None:
            self._icon = self._get_file_icon()
        return self._icon

    @property
    def file_extension(self) -> str:
        """文件扩展名"""
        if self.filename and '.' in self.filename:
            return os.path.splitext(self.filename)[1].lower()
        return ''

    def _format_file_size(self, size_bytes: int) -> str:
        """格式化文件大小"""
        if size_bytes >= 1024 * 1024 * 1024:  # GB
            return f"{size_bytes / (1024 * 1024 * 1024):.2f} GB"
        elif size_bytes >= 1024 * 1024:  # MB
            return f"{size_bytes / (1024 * 1024):.2f} MB"
        elif size_bytes >= 1024:  # KB
            return f"{size_bytes / 1024:.2f} KB"
        else:
            return f"{size_bytes} 字节"

    def _get_file_icon(self) -> str:
        """根据文件类型返回图标类名"""
        if self.is_folder:
            return "fas fa-folder"

        # 根据分类
        if self.category == 1:  # 音频
            return "fas fa-file-audio"
        elif self.category == 2:  # 视频
            return "fas fa-file-video"
        elif self.category == 3:  # 图片
            return "fas fa-file-image"

        # 根据文件扩展名
        ext = self.file_extension
        if ext in ['.txt', '.md', '.log']:
            return "fas fa-file-alt"
        elif ext in ['.pdf']:
            return "fas fa-file-pdf"
        elif ext in ['.doc', '.docx']:
            return "fas fa-file-word"
        elif ext in ['.xls', '.xlsx']:
            return "fas fa-file-excel"
        elif ext in ['.ppt', '.pptx']:
            return "fas fa-file-powerpoint"
        elif ext in ['.zip', '.rar', '.7z']:
            return "fas fa-file-archive"
        else:
            return "fas fa-file"

    def to_dict(self) -> dict:
        """
        转换为字典格式（兼容旧代码）
        """
        result = self._data.copy()

        # 添加处理后的属性
        result['size_formatted'] = self.size_formatted
        result['category_name'] = self.category_name
        result['icon'] = self.icon
        result['is_folder'] = self.is_folder

        return result

    def get(self, key: str, default=None):
        """
        获取属性值（兼容字典访问方式）
        """
        # 首先尝试从对象属性获取
        if hasattr(self, key):
            return getattr(self, key)

        # 然后从原始数据获取
        return self._data.get(key, default)

    def __getitem__(self, key: str):
        """支持字典式访问（兼容性）"""
        return self.get(key)

    def __setitem__(self, key: str, value):
        """支持字典式设置（兼容性）"""
        self._data[key] = value
        # 清除缓存的计算属性
        if key in ['size']:
            self._size_formatted = None
        elif key in ['category']:
            self._category_name = None
        elif key in ['type', 'filename', 'category']:
            self._icon = None
            self._is_folder = None

    def __contains__(self, key: str) -> bool:
        """支持 'in' 操作符"""
        return hasattr(self, key) or key in self._data

    def __str__(self) -> str:
        """字符串表示"""
        return f"File(id={self.file_id}, name='{self.filename}', type={'folder' if self.is_folder else 'file'})"

    def __repr__(self) -> str:
        """详细字符串表示"""
        return f"File(file_id={self.file_id}, filename='{self.filename}', size={self.size}, is_folder={self.is_folder})"


class FileList:
    """文件列表类"""

    def __init__(self, files_data: list):
        """
        初始化文件列表
        :param files_data: 文件信息字典列表
        """
        self.files = [File(file_data) for file_data in files_data]

    def __iter__(self):
        """支持迭代"""
        return iter(self.files)

    def __len__(self) -> int:
        """获取文件数量"""
        return len(self.files)

    def __getitem__(self, index: int) -> File:
        """支持索引访问"""
        return self.files[index]

    def filter_by_type(self, is_folder: bool) -> list:
        """按类型过滤文件"""
        return [f for f in self.files if f.is_folder == is_folder]

    def filter_by_category(self, category: int) -> list:
        """按分类过滤文件"""
        return [f for f in self.files if f.category == category]

    def to_dict_list(self) -> list:
        """转换为字典列表（兼容性）"""
        return [file.to_dict() for file in self.files]

    def find_by_name(self, filename: str) -> Optional[File]:
        """
        根据文件名查找文件
        :param filename: 要查找的文件名
        :return: 找到的File对象，如果未找到则返回None
        """
        for file in self.files:
            if file.filename == filename:
                return file
        return None
