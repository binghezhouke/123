import argparse
import json
import os
import re
from api.client import Pan123Client
import tqdm
import base62


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


def _decode_sha1(raw_value: str, uses_base62: bool = False) -> str:
    """从字符串或base62编码中解析出40位SHA1 hex，解析失败返回空字符串。"""
    if not raw_value:
        return ""

    raw = str(raw_value).strip()
    candidate = raw

    # 尝试base62解码
    if uses_base62 and re.fullmatch(r"[0-9A-Za-z]+", raw):
        try:
            num = base62.decode(raw, charset=base62.CHARSET_INVERTED)
            byte_len = max((num.bit_length() + 7) // 8, 1)
            candidate = num.to_bytes(byte_len, 'big').hex()
        except Exception:
            candidate = raw

    candidate = candidate.lower()

    # 补齐前导零，确保长度检测准确
    if len(candidate) < 40:
        candidate = candidate.zfill(40)

    # 只接受40位sha1 hex
    if re.fullmatch(r"[0-9a-f]{40}", candidate):
        return candidate

    return ""


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
        etag = file_info.get('etag')

        if not all([file_path, size, etag]):
            print(f"跳过不完整的文件记录: {file_info}")
            continue

        # 提取文件名和相对目录
        dir_path, filename = os.path.split(file_path)
        current_parent_id = root_id

        # 规范化SHA1
        sha1_hex = _decode_sha1(etag, usesBase62EtagsInExport)
        size_int = int(size)

        if not sha1_hex:
            tqdm.tqdm.write(f"跳过 '{filename}': 缺少有效的SHA1哈希")
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

            # 调用SHA1秒传
            reuse_result = client.file_service.try_sha1_reuse(
                local_path=None,
                filename=filename,
                parent_id=current_parent_id,
                duplicate=1,
                sha1=sha1_hex,
                size=size_int
            )

            if reuse_result and reuse_result.get('reuse'):
                file_id = reuse_result.get('fileID')
                tqdm.tqdm.write(
                    f"  ✓ 秒传成功: {os.path.join(dir_path, filename)} (fileID={file_id})")
            else:
                tqdm.tqdm.write(
                    f"  ⚠️ 秒传未命中: {os.path.join(dir_path, filename)} (云端无此文件)")

        except Exception as e:
            tqdm.tqdm.write(f"处理 '{filename}' 失败: {e}")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='从JSON文件上传文件到123云盘')
    parser.add_argument('json_file', help='包含文件信息的JSON文件路径')
    parser.add_argument('-d', '--directory',
                        help='要上传到的远程根目录路径 (可选, 如果未提供则使用JSON中的commonPath)')
    args = parser.parse_args()

    upload_from_json(args.json_file, args.directory)
