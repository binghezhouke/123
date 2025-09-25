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

    # 遍历并创建文件
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

        # 如果文件在子目录中，尝试直接传递带相对路径的 filename（contain_dir=True），
        # 条件：整体字节长度 <= 255，且路径不以斜杠开头且不包含非法字符（\ <>: " | ? * 反斜杠除外）
        use_contain_dir = False
        full_relative_path = file_path.replace('\\', '/')  # 规范为正斜杠

        try:
            if usesBase62EtagsInExport and len(etag) != 32:
                etag = base62.decode(
                    etag, charset=base62.CHARSET_INVERTED).to_bytes(16).hex()

            # 判断是否可以使用 contain_dir 直接创建（避免额外创建目录）
            if dir_path:
                byte_len = len(full_relative_path.encode('utf-8'))
                # 不能以 '/' 开头，并且总长度限制为255字节
                if byte_len <= 255 and not full_relative_path.startswith('/'):
                    # 禁止反斜杠和一些特殊字符
                    if not re.search(r'[\\:\\*\?\"<>\|]', full_relative_path):
                        use_contain_dir = True

            if use_contain_dir:
                # 直接把包含相对路径的 filename 传给 create_file
                client.file_service.create_file(
                    parent_id=current_parent_id,
                    filename=full_relative_path,
                    size=int(size),
                    etag=etag,
                    contain_dir=True
                )
            else:
                # 原有逻辑：确保目录存在并使用纯文件名创建
                if dir_path:
                    full_dir_path = os.path.join(base_path, dir_path)

                    # 检查映射中是否已存在该目录路径
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

                client.file_service.create_file(
                    parent_id=current_parent_id,
                    filename=filename,
                    size=int(size),
                    etag=etag
                )
            # tqdm.tqdm.write(f"成功为 '{filename}' 创建文件条目。")
        except Exception as e:
            tqdm.tqdm.write(f"为 '{filename}' 创建文件条目失败: {e}")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='从JSON文件上传文件到123云盘')
    parser.add_argument('json_file', help='包含文件信息的JSON文件路径')
    parser.add_argument('-d', '--directory',
                        help='要上传到的远程根目录路径 (可选, 如果未提供则使用JSON中的commonPath)')
    args = parser.parse_args()

    upload_from_json(args.json_file, args.directory)
