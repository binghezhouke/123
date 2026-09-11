import argparse
import json
import os
import re
from concurrent.futures import ThreadPoolExecutor, as_completed

from api import Pan123Client
import tqdm


def file2json(json_file_path):
    with open(json_file_path, encoding='utf-8') as f:
        data = f.read()
    cnts = data.strip().split("$")
    out = {}
    out["usesBase62EtagsInExport"] = True
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


def upload_from_json(client: Pan123Client, json_file_path: str, remote_dir: str,
                     shared_dir_map: dict = None, max_workers: int = 8) -> dict:
    """
    从 JSON 文件读取文件列表并并发秒传到指定的远程目录。

    秒传接口（sha1_reuse/create）不在官方限流表内，可以并发；
    目录创建（mkdir 限流 20 QPS）保持串行，目录ID通过 shared_dir_map 跨调用复用。

    :param shared_dir_map: 跨调用共享的 {目录路径: 目录ID} 映射，batch 场景传入以复用目录树
    :param max_workers: 并发秒传线程数
    :return: {'hit': 秒传命中, 'miss': 未命中, 'fail': 失败, 'skip': 跳过}
    """
    counts = {'hit': 0, 'miss': 0, 'fail': 0, 'skip': 0}

    if not os.path.exists(json_file_path):
        print(f"错误: JSON 文件不存在: {json_file_path}")
        return counts

    with open(json_file_path, 'r', encoding='utf-8') as f:
        try:
            data = json.load(f)
        except json.JSONDecodeError:
            data = file2json(json_file_path)
    uses_base62 = data.get('usesBase62EtagsInExport', False)

    base_path = remote_dir if remote_dir else data.get('commonPath', '')
    if not base_path:
        print("错误: 未在JSON中找到 commonPath，并且未提供远程目录。")
        return counts

    dir_map = shared_dir_map if shared_dir_map is not None else {}

    print(f"将在远程路径 '{base_path}' 中创建文件结构...")

    try:
        root_id = dir_map.get(base_path)
        if root_id is None:
            root_id = client.file_service.mkdir_recursive(base_path)
            dir_map[base_path] = root_id
    except Exception as e:
        print(f"创建根目录 '{base_path}' 失败: {e}")
        return counts

    files_to_upload = data.get('files', [])

    # 阶段1：串行建目录树
    for file_info in files_to_upload:
        dir_path = os.path.split(file_info.get('path') or '')[0]
        if not dir_path:
            continue
        full_dir_path = os.path.join(base_path, dir_path)
        if full_dir_path in dir_map:
            continue
        try:
            dir_map[full_dir_path] = client.file_service.mkdir_recursive(full_dir_path)
        except Exception as e:
            tqdm.tqdm.write(f"创建子目录 '{dir_path}' 失败: {e}")

    # 阶段2：线程池并发秒传
    def reuse_one(file_info):
        file_path = file_info.get('path')
        size = file_info.get('size')
        etag = file_info.get('sha1') or file_info.get('etag')
        if not all([file_path, size, etag]):
            return 'skip', str(file_info), '记录不完整'

        dir_path, filename = os.path.split(file_path)
        display_path = os.path.join(dir_path, filename)

        hash_hex, hash_type = _decode_hash(etag, uses_base62)
        if not hash_hex:
            return 'skip', display_path, f'缺少有效的哈希值 (原始值: {etag})'

        if dir_path:
            parent_id = dir_map.get(os.path.join(base_path, dir_path))
            if parent_id is None:
                return 'skip', display_path, '目录创建失败'
        else:
            parent_id = root_id

        try:
            if hash_type == 'sha1':
                result = client.file_service.try_sha1_reuse(
                    local_path=None, filename=filename, parent_id=parent_id,
                    duplicate=1, sha1=hash_hex, size=int(size))
                if result and result.get('reuse'):
                    return 'hit', display_path, f"SHA1秒传成功 (fileID={result.get('fileID')})"
                return 'miss', display_path, 'SHA1未命中 (云端无此文件)'

            if hash_type == 'md5':
                result = client.file_service.create_file(
                    parent_id=parent_id, filename=filename, etag=hash_hex,
                    size=int(size), duplicate=1)
                if result and result.get('reuse'):
                    return 'hit', display_path, f"MD5秒传成功 (fileID={result.get('fileID')})"
                return 'miss', display_path, 'MD5未命中 (云端无此文件)'

            return 'skip', display_path, f'未知哈希类型: {hash_type}'
        except Exception as e:
            return 'fail', display_path, str(e)

    icons = {'hit': '✓', 'miss': '⚠', 'fail': '❌', 'skip': '⏭'}
    executor = ThreadPoolExecutor(max_workers=max_workers)
    try:
        futures = [executor.submit(reuse_one, fi) for fi in files_to_upload]
        with tqdm.tqdm(total=len(futures), desc="并发秒传") as bar:
            for fut in as_completed(futures):
                try:
                    status, display_path, detail = fut.result()
                except Exception as e:
                    counts['fail'] += 1
                    tqdm.tqdm.write(f"  ❌ 任务异常: {e}")
                else:
                    counts[status] += 1
                    tqdm.tqdm.write(f"  {icons[status]} {display_path}: {detail}")
                bar.update(1)
    except KeyboardInterrupt:
        executor.shutdown(wait=False, cancel_futures=True)
        raise
    executor.shutdown(wait=True)

    return counts


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description='从JSON文件上传文件到123云盘')
    parser.add_argument('json_file', help='包含文件信息的JSON文件路径')
    parser.add_argument('-d', '--directory',
                        help='要上传到的远程根目录路径 (可选, 如果未提供则使用JSON中的commonPath)')
    parser.add_argument('-w', '--workers', type=int, default=8,
                        help='并发秒传线程数 (默认: 8)')
    args = parser.parse_args()

    with Pan123Client() as client:
        counts = upload_from_json(client, args.json_file, args.directory,
                                  max_workers=args.workers)
    print("\n===== 汇总 =====")
    print(f"秒传成功: {counts['hit']}, 未命中: {counts['miss']}, "
          f"失败: {counts['fail']}, 跳过: {counts['skip']}")
