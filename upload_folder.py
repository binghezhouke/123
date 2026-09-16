#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""递归上传本地文件夹到远程路径。

用法：
    python upload_folder.py /path/to/local/folder remote/path/on/server
    python upload_folder.py /path/to/local/folder --dry-run

脚本会：
- 使用仓库中的 ``Pan123Client`` 客户端进行认证
- 在远程创建对应目录结构（逐级创建并缓存目录ID）
- 逐文件上传，同名同大小文件跳过，先试 SHA1 秒传再走分片上传

实际逻辑在 upload_core.upload_directory。
"""

import argparse
import os
import sys
import traceback

from api import Pan123Client
from upload_core import DEFAULT_RATE, upload_directory


def main():
    parser = argparse.ArgumentParser(description="递归上传本地文件夹到远端路径")
    parser.add_argument('local_path', help='本地目录路径')
    parser.add_argument('remote_path', nargs='?', default='',
                        help='远程目标路径（相对于根目录），例如 "foo/bar"，不传表示根目录')
    parser.add_argument('--dry-run', action='store_true', help='仅打印计划操作，不实际上传')
    parser.add_argument('--rate', type=float, default=DEFAULT_RATE,
                        help=f'每秒请求数上限 (默认: {DEFAULT_RATE:g}；0 表示不限速)')
    parser.add_argument('-v', '--verbose', action='store_true',
                        help='逐条打印每个文件的结果（默认只打印失败项）')
    args = parser.parse_args()

    local_path = os.path.abspath(args.local_path)
    if not os.path.exists(local_path):
        print(f"错误：本地路径不存在: {local_path}")
        sys.exit(1)
    if not os.path.isdir(local_path):
        print(f"错误：本地路径不是目录: {local_path}")
        sys.exit(1)

    try:
        with Pan123Client() as client:
            stats = upload_directory(client, local_path, args.remote_path,
                                     dry_run=args.dry_run,
                                     rate=None if args.rate <= 0 else args.rate,
                                     verbose=args.verbose)
    except KeyboardInterrupt:
        print("\n用户中断")
        sys.exit(3)
    except Exception as e:
        print(f"执行上传时发生错误: {e}")
        traceback.print_exc()
        sys.exit(4)

    if args.dry_run:
        print("\n（--dry-run：以上仅为计划，没有实际上传）")
        return

    print("\n上传完成 Summary:")
    print(f"  本地文件数: {stats.total}")
    print(f"  成功:       {stats.hit}（其中秒传 {stats.reuse}）")
    print(f"  跳过:       {stats.skip}")
    print(f"  失败:       {stats.fail}")
    for path, detail in stats.failures:
        print(f"    ❌ {path}: {detail}")


if __name__ == '__main__':
    main()
