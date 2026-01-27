#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
递归上传本地文件夹到远程路径的脚本
用法：
    python upload_folder.py /path/to/local/folder remote/path/on/server

参数：
    local_path: 本地待上传的目录路径
    remote_path: 远程目标路径（相对根目录），例如 "foo/bar" 或 "/foo/bar"

脚本会：
- 使用仓库中的 `Pan123Client` 客户端进行认证
- 在远程创建对应目录结构（使用 `mkdir_recursive`）
- 逐文件调用 `file_service.upload_file` 上传文件

可选参数：
    --dry-run: 仅打印将要执行的操作，不实际上传

"""

import argparse
import os
import sys
import traceback
from api import Pan123Client, Pan123APIError
try:
    from tqdm import tqdm
except Exception:
    tqdm = None


def normalize_remote_path(path: str) -> str:
    """去除首尾斜杠并将本地分隔符转换为 '/'"""
    if path is None:
        return ""
    p = path.strip()
    if not p:
        return ""
    # 保证使用 '/' 作为远程路径分隔符
    p = p.strip('/')
    p = p.replace('\\', '/').replace(os.sep, '/')
    return p


def upload_folder(local_path: str, remote_path: str, client: Pan123Client, dry_run: bool = False):
    """递归上传 local_path 到 remote_path（remote_path 是相对于远程根目录的路径）"""
    local_path = os.path.abspath(local_path)

    if not os.path.exists(local_path):
        print(f"错误：本地路径不存在: {local_path}")
        return 1

    if not os.path.isdir(local_path):
        print(f"错误：本地路径不是目录: {local_path}")
        return 1

    base_remote = normalize_remote_path(remote_path)

    # 创建/获取远程根目录 ID
    try:
        if base_remote:
            print(f"在远程创建或获取根目录: {base_remote}")
            if dry_run:
                root_id = None
            else:
                root_id = client.file_service.mkdir_recursive(base_remote)
                print(f"远程根目录 ID: {root_id}")
        else:
            root_id = 0
            print("使用远程根目录: / (ID=0)")
    except Exception as e:
        print(f"创建远程根目录失败: {e}")
        traceback.print_exc()
        return 1

    # 缓存已创建的远程目录 id（key 为相对于 base_remote 的相对路径）
    dir_cache = {"": root_id}

    # 先统计本地文件总数（用于进度显示）
    total_files = 0
    for _, _, filenames in os.walk(local_path):
        total_files += len(filenames)

    if total_files == 0:
        print("本地目录中没有要上传的文件。")
        return 0

    uploaded = 0
    failed = 0

    use_tqdm = tqdm is not None
    pbar = None
    try:
        if use_tqdm:
            pbar = tqdm(total=total_files, unit='file',
                        desc=f"上传到: /{base_remote}" if base_remote else "上传到: /")

        for dirpath, dirnames, filenames in os.walk(local_path):
            rel = os.path.relpath(dirpath, local_path)
            if rel == '.':
                rel = ''

            # 远程相对路径（相对于 base_remote）使用 '/'
            rel_posix = '' if not rel else rel.replace(os.sep, '/')

            # 获取或创建远程目录 ID
            try:
                if rel_posix in dir_cache:
                    parent_id = dir_cache[rel_posix]
                else:
                    # 在 base_remote 下创建相对路径 rel_posix
                    print(f"创建远程子目录: {rel_posix} (基于 {base_remote or '/'} )")
                    if dry_run:
                        parent_id = None
                    else:
                        # 将相对路径传给 mkdir_recursive，并指定 parent_id 为 root_id
                        parent_id = client.file_service.mkdir_recursive(
                            rel_posix, parent_id=root_id)
                    dir_cache[rel_posix] = parent_id
            except Exception as e:
                print(f"创建远程目录 '{rel_posix}' 失败: {e}")
                traceback.print_exc()
                # 跳过本目录下的文件
                # 仍需为进度条消费这些文件数量
                if use_tqdm:
                    for _ in filenames:
                        pbar.update(1)
                continue

            for fname in filenames:
                local_file = os.path.join(dirpath, fname)

                if dry_run:
                    print(
                        f"[DRY-RUN] 会上传: {local_file} -> {os.path.join(base_remote, rel_posix, fname) if base_remote else os.path.join('/', rel_posix, fname)}")
                    if use_tqdm:
                        pbar.update(1)
                    continue

                try:
                    # print(f"上传: {local_file} -> 远程目录ID {parent_id}")
                    result = client.file_service.upload_file(
                        local_path=local_file,
                        parent_id=parent_id,
                        filename=fname,
                        skip_if_exists=True,
                        try_sha1_reuse=True  # 启用SHA1秒传
                    )
                    if result and not result.get("skipped"):
                        uploaded += 1
                        upload_method = ""
                        if result.get('method') == 'sha1_reuse':
                            upload_method = " (SHA1秒传)"
                        elif result.get('reuse'):
                            upload_method = " (秒传)"
                        print(f"  ✓ 上传成功: {fname}{upload_method} -> {result}")
                    elif result and result.get("skipped"):
                        # 文件被跳过，也算作“成功”处理
                        uploaded += 1
                    else:
                        failed += 1
                        print(f"  ✗ 上传返回失败: {fname}")
                except Pan123APIError as e:
                    failed += 1
                    print(f"  ✗ API 错误 上传文件 {fname}: {e}")
                except Exception as e:
                    failed += 1
                    print(f"  ✗ 未知错误 上传文件 {fname}: {e}")
                    traceback.print_exc()
                finally:
                    if use_tqdm:
                        pbar.update(1)
    finally:
        if pbar:
            pbar.close()

    print("\n上传完成 Summary:")
    print(f"  本地总文件: {total_files}")
    print(f"  上传成功:   {uploaded}")
    print(f"  上传失败:   {failed}")

    return 0


def main():
    parser = argparse.ArgumentParser(description="递归上传本地文件夹到远端路径")
    parser.add_argument('local_path', help='本地目录路径')
    parser.add_argument('remote_path', nargs='?', default='',
                        help='远程目标路径（相对于根目录），例如 "foo/bar"，不传表示根目录')
    parser.add_argument('--dry-run', action='store_true', help='仅打印计划操作，不实际上传')

    args = parser.parse_args()

    try:
        client = Pan123Client()
    except Exception as e:
        print(f"初始化 Pan123Client 失败: {e}")
        traceback.print_exc()
        sys.exit(2)

    try:
        with client:
            code = upload_folder(
                args.local_path, args.remote_path, client, dry_run=args.dry_run)
            sys.exit(code or 0)
    except KeyboardInterrupt:
        print("\n用户中断")
        sys.exit(3)
    except Exception as e:
        print(f"执行上传时发生错误: {e}")
        traceback.print_exc()
        sys.exit(4)


if __name__ == '__main__':
    main()
