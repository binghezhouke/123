"""从清单文件秒传到 123 云盘。

清单可以是 JSON（含 files/commonPath 字段），也可以是 123 云盘导出的
``etag#size#path$...`` 文本。实际逻辑在 upload_core.upload_manifest。

默认行为：
- 先查目标目录快照，云端已有同一文件就不发请求（也不会造出重复文件）
- 剩下的按 8 请求/秒限速并发秒传，失败的串行重试一轮

传 --tree 目录树导出文件（网页端"导出目录树"生成的 txt）可以先按文件名
零请求地筛掉已上传的文件，连目录都不用列；目录树里没有的才走上面的流程。
判重完全以云端为准，不依赖任何本地"已完成"记录。

用法：
    python upload_from_json.py files.json
    python upload_from_json.py files.json -d 目标目录 -w 16 --rate 10
"""

import argparse

from api import Pan123Client
from upload_core import (
    DEFAULT_RATE,
    RemoteDirTree,
    load_tree_index,
    upload_manifest,
)


def main():
    parser = argparse.ArgumentParser(description='从JSON/导出文本秒传到123云盘')
    parser.add_argument('json_file', help='包含文件信息的清单文件路径')
    parser.add_argument('-d', '--directory',
                        help='要上传到的远程根目录路径 (可选, 如果未提供则使用清单中的commonPath)')
    parser.add_argument('--tree',
                        help='目录树导出文件（网页端"导出目录树"的 txt），'
                             '先按文件名零请求地筛掉已上传的文件')
    parser.add_argument('--tree-root',
                        help='目录树根对应的远程路径（默认：树文件首行，"我的文件" 视为网盘根）')
    parser.add_argument('--tree-verify', action='store_true',
                        help='目录树按名字命中后仍列目录核实大小/MD5（默认直接跳过，不校验内容）')
    parser.add_argument('-w', '--workers', type=int, default=8,
                        help='并发秒传线程数 (默认: 8, 实际速率受 --rate 限制)')
    parser.add_argument('--rate', type=float, default=DEFAULT_RATE,
                        help=f'每秒请求数上限 (默认: {DEFAULT_RATE:g}，账号级限流约 8/s；0 表示不限速)')
    parser.add_argument('--no-dedup', action='store_true',
                        help='不列目标目录做判重（目标目录非常大时可能更划算）')
    parser.add_argument('--verify', action='store_true',
                        help='清单只有 SHA1 时也发秒传请求确认，而不是按"同名同大小"跳过')
    parser.add_argument('-v', '--verbose', action='store_true',
                        help='逐条打印每个文件的结果（默认只打印失败项）')
    args = parser.parse_args()

    rate = None if args.rate <= 0 else args.rate

    tree_index = None
    if args.tree:
        # 目录树是上传前刚导出的"云端现状"：先按它筛掉已上传的文件，
        # 连目标目录都不用列（目录树里没有的条目才需要解析目录）
        tree_index = load_tree_index(args.tree, args.tree_root)
        print(f"📁 目录树: {tree_index.file_count:,} 个文件 / "
              f"{tree_index.dir_count:,} 个目录，先按它判重")

    with Pan123Client() as client:
        tree = RemoteDirTree(client, args.directory or '')
        stats = upload_manifest(
            client, args.json_file, args.directory,
            dir_tree=tree, max_workers=args.workers, rate=rate,
            dedup=not args.no_dedup, verify_sha1=args.verify,
            tree_index=tree_index, tree_verify=args.tree_verify,
            verbose=args.verbose)

    print("\n===== 汇总 =====")
    for line in stats.summary_lines():
        print(line)


if __name__ == '__main__':
    main()
