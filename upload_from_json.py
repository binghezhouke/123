import argparse
import json
import os
import re
from api.client import Pan123Client
import tqdm


def file2json(json_file_path):
    data = open(json_file_path).read()
    cnts = data = data.strip().split("$")
    out = {}
    out["usesBase62EtagsInExport"] = True,
    files = []
    out["files"] = files

    for x in cnts:
        parts = x.strip().split("#")
        if len(parts) == 3:
            one_file = {}
            one_file["path"] = parts[2]
            one_file["size"] = parts[1]
            one_file["etag"] = parts[0]
            files.append(one_file)
    return out


def _decode_hash(raw_value: str, uses_base62: bool = False) -> tuple:
    """
    从字符串或base62编码中解析出哈希值（SHA1 或 MD5/etag）。

    支持的格式:
    - 40位hex字符串: 识别为 SHA1
    - 32位hex字符串: 识别为 MD5/etag
    - base62编码的SHA1或MD5（优先解码为MD5）
    - 不足标准长度的hex字符串: 补零后按位数判断

    :param raw_value: 原始哈希值字符串
    :param uses_base62: 是否标记为base62编码
    :return: (hash_hex, hash_type) 其中 hash_type 为 'sha1'、'md5' 或 ''
    """
    if not raw_value:
        return "", ""

    raw = str(raw_value).strip()
    if not raw:
        return "", ""

    lower = raw.lower()

    # 1. 先检查是否为标准hex格式
    if re.fullmatch(r'[0-9a-f]{40}', lower):
        return lower, 'sha1'
    if re.fullmatch(r'[0-9a-f]{32}', lower):
        return lower, 'md5'

    # 2. 尝试base62解码（显式标记 或 含非hex字符的纯字母数字串）
    is_alnum = bool(re.fullmatch(r'[0-9A-Za-z]+', raw))
    is_pure_hex = bool(re.fullmatch(r'[0-9a-fA-F]+', raw))

    if is_alnum and (uses_base62 or not is_pure_hex):
        try:
            import base62
            num = base62.decode(raw, charset=base62.CHARSET_INVERTED)
            byte_len = max((num.bit_length() + 7) // 8, 1)
            hex_str = num.to_bytes(byte_len, 'big').hex().lower()

            # 优先解码为MD5（16字节 = 32位hex）
            if byte_len <= 16:
                return hex_str.zfill(32), 'md5'
            elif byte_len <= 20:
                return hex_str.zfill(40), 'sha1'
            # byte_len > 20 说明解码结果过长，不是有效哈希
        except Exception:
            pass

    # 3. 不足标准长度的纯hex字符串，按位数补零判断
    if is_pure_hex:
        if len(lower) <= 32:
            return lower.zfill(32), 'md5'
        elif len(lower) <= 40:
            return lower.zfill(40), 'sha1'

    return "", ""


def upload_from_json(json_file_path, remote_dir):
    """
    从 JSON 文件读取文件列表并上传到指定的远程目录。
    """
    if not os.path.exists(json_file_path):
        print(f"错误: JSON 文件不存在: {json_file_path}")
        return

    with open(json_file_path, 'r', encoding='utf-8') as f:
        try:
            data = json.load(f)
        except:
            data = file2json(json_file_path)

    client = Pan123Client()
    usesBase62EtagsInExport = data.get('usesBase62EtagsInExport', False)

    # 创建文件夹路径到ID的映射字典，避免重复创建
    dir_path_to_id_map = {}

    # 确定根目录
    base_path = remote_dir if remote_dir else data.get('commonPath', '')
    if not base_path:
        print("错误: 未在JSON中找到 commonPath，并且未提供远程目录。")
        return

    print(f"将在远程路径 '{base_path}' 中创建文件结构...")

    # 创建根目录
    try:
        root_id = client.file_service.mkdir_recursive(base_path)
        dir_path_to_id_map[base_path] = root_id
        print(f"成功创建或找到根目录 '{base_path}', ID: {root_id}")
    except Exception as e:
        print(f"创建根目录 '{base_path}' 失败: {e}")
        return

    # 遍历并秒传文件
    files_to_upload = data.get('files', [])
    for file_info in tqdm.tqdm(files_to_upload, desc="上传文件"):

        file_path = file_info.get('path')
        size = file_info.get('size')
        # 兼容 sha1 和 etag 两种字段名
        etag = file_info.get('sha1') or file_info.get('etag')

        if not all([file_path, size, etag]):
            print(f"跳过不完整的文件记录: {file_info}")
            continue

        # 提取文件名和相对目录
        dir_path, filename = os.path.split(file_path)
        current_parent_id = root_id

        # 解析哈希值（支持SHA1、MD5/etag、base62编码）
        hash_hex, hash_type = _decode_hash(etag, usesBase62EtagsInExport)
        size_int = int(size)

        if not hash_hex:
            tqdm.tqdm.write(f"跳过 '{filename}': 缺少有效的哈希值 (原始值: {etag})")
            continue

        try:
            # 确保目录存在
            if dir_path:
                full_dir_path = os.path.join(base_path, dir_path)

                if full_dir_path in dir_path_to_id_map:
                    current_parent_id = dir_path_to_id_map[full_dir_path]
                else:
                    try:
                        current_parent_id = client.file_service.mkdir_recursive(
                            full_dir_path)
                        dir_path_to_id_map[full_dir_path] = current_parent_id
                    except Exception as e:
                        tqdm.tqdm.write(f"创建子目录 '{dir_path}' 失败: {e}")
                        continue

            reuse_result = None
            display_path = os.path.join(dir_path, filename)

            if hash_type == 'sha1':
                # SHA1秒传
                reuse_result = client.file_service.try_sha1_reuse(
                    local_path=None,
                    filename=filename,
                    parent_id=current_parent_id,
                    duplicate=1,
                    sha1=hash_hex,
                    size=size_int
                )

                if reuse_result and reuse_result.get('reuse'):
                    file_id = reuse_result.get('fileID')
                    tqdm.tqdm.write(
                        f"  ✓ SHA1秒传成功: {display_path} (fileID={file_id})")
                else:
                    tqdm.tqdm.write(
                        f"  ⚠️ SHA1秒传未命中: {display_path} (云端无此文件)")

            elif hash_type == 'md5':
                # MD5/etag秒传（通过预上传接口）
                reuse_result = client.file_service.create_file(
                    parent_id=current_parent_id,
                    filename=filename,
                    etag=hash_hex,
                    size=size_int,
                    duplicate=1
                )

                if reuse_result and reuse_result.get('reuse'):
                    file_id = reuse_result.get('fileID')
                    tqdm.tqdm.write(
                        f"  ✓ MD5秒传成功: {display_path} (fileID={file_id})")
                else:
                    tqdm.tqdm.write(
                        f"  ⚠️ MD5秒传未命中: {display_path} (云端无此文件)")

        except Exception as e:
            tqdm.tqdm.write(f"处理 '{filename}' 失败: {e}")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='从JSON文件上传文件到123云盘')
    parser.add_argument('json_file', help='包含文件信息的JSON文件路径')
    parser.add_argument('-d', '--directory',
                        help='要上传到的远程根目录路径 (可选, 如果未提供则使用JSON中的commonPath)')
    args = parser.parse_args()

    upload_from_json(args.json_file, args.directory)
