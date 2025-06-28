import argparse
import json
import os
from api.client import Pan123Client
import tqdm


def upload_from_json(json_file_path, remote_dir):
    """
    从 JSON 文件读取文件列表并上传到指定的远程目录。
    """
    if not os.path.exists(json_file_path):
        print(f"错误: JSON 文件不存在: {json_file_path}")
        return

    with open(json_file_path, 'r', encoding='utf-8') as f:
        data = json.load(f)

    client = Pan123Client()
    usesBase62EtagsInExport = data.get('usesBase62EtagsInExport', False)

    # 确定根目录
    base_path = remote_dir if remote_dir else data.get('commonPath', '')
    if not base_path:
        print("错误: 未在JSON中找到 commonPath，并且未提供远程目录。")
        return

    print(f"将在远程路径 '{base_path}' 中创建文件结构...")

    # 创建根目录
    try:
        root_id = client.file_service.mkdir_recursive(base_path)
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

        # 如果文件在子目录中，则创建子目录
        if dir_path:
            try:
                current_parent_id = client.file_service.mkdir_recursive(
                    os.path.join(base_path, dir_path))
                # print(f"成功创建或找到子目录: {dir_path}, ID: {current_parent_id}")
            except Exception as e:
                tqdm.tqdm.write(f"创建子目录 '{dir_path}' 失败: {e}")
                continue

        import base62
        if usesBase62EtagsInExport:
            etag = base62.base62_to_hex(etag)
        # 创建文件
        try:
            # tqdm.tqdm.write(f"正在创建文件 '{filename}' 在目录 ID {current_parent_id}...")
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
