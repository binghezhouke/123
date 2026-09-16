"""批量从清单文件秒传到 123 云盘。

遍历指定目录下的所有 .json / .txt 清单文件，逐个调用 upload_core 秒传。
判重完全以云端为准：传 --tree 时所有清单共用它先按文件名零请求预筛
（口径与 upload_from_json 一致），没传时列目标目录核对；不依赖任何
本地"已完成"记录（batch_done.log / journal.jsonl 那套已移除）。

用法：
    python batch_upload.py /path/to/json_dir
    python batch_upload.py /path/to/json_dir -d 远程前缀目录 -w 16
"""

import argparse
import os
import sys
import time

from api import Pan123Client
from upload_core import (
    DEFAULT_RATE,
    RemoteDirTree,
    RemoteIndex,
    load_manifest,
    load_tree_index,
    normalize_remote_path,
    upload_manifest,
)

MANIFEST_SUFFIXES = (".json", ".txt")


def collect_manifests(json_dir: str):
    return sorted(f for f in os.listdir(json_dir)
                  if f.lower().endswith(MANIFEST_SUFFIXES))


def manifest_remote_dir(json_path: str, base_dir: str) -> str:
    """把 -d 前缀目录和清单里的 commonPath 拼成最终的远程目录。"""
    common_path = normalize_remote_path(load_manifest(json_path).get('commonPath', ''))
    if base_dir and common_path:
        return f"{normalize_remote_path(base_dir)}/{common_path}"
    return base_dir or common_path


def main():
    parser = argparse.ArgumentParser(description="批量从清单文件秒传到 123 云盘")
    parser.add_argument(
        "json_dir",
        help="包含清单文件的目录路径",
    )
    parser.add_argument(
        "-d", "--directory",
        help="远程前缀目录路径 (可选, 每个清单的 commonPath 会放在此目录下)",
        default=None,
    )
    parser.add_argument(
        "-w", "--workers",
        type=int, default=8,
        help="每个清单并发秒传线程数 (默认: 8, 实际速率受 --rate 限制)",
    )
    parser.add_argument(
        "--rate",
        type=float, default=DEFAULT_RATE,
        help=f"每秒请求数上限 (默认: {DEFAULT_RATE:g}，账号级限流约 8/s；0 表示不限速)",
    )
    parser.add_argument(
        "--tree",
        help="目录树导出文件（网页端\"导出目录树\"的 txt），"
             "所有清单共用，先按文件名零请求地筛掉已上传的文件",
    )
    parser.add_argument(
        "--tree-root",
        help="目录树根对应的远程路径（默认：树文件首行，\"我的文件\" 视为网盘根）",
    )
    parser.add_argument(
        "--tree-verify", action="store_true",
        help="目录树按名字命中后仍列目录核实大小/MD5（默认直接跳过，不校验内容）",
    )
    parser.add_argument(
        "--no-dedup", action="store_true",
        help="不列目标目录做判重（目标目录非常大时可能更划算）",
    )
    parser.add_argument(
        "-v", "--verbose", action="store_true",
        help="逐条打印每个文件的结果（默认只打印失败项）",
    )
    args = parser.parse_args()

    json_dir = os.path.abspath(args.json_dir)
    if not os.path.isdir(json_dir):
        print(f"错误: 目录不存在: {json_dir}")
        sys.exit(1)

    manifests = collect_manifests(json_dir)
    total = len(manifests)
    if total == 0:
        print("目录中没有找到清单文件。")
        sys.exit(0)

    print(f"共 {total} 个清单文件")

    tree_index = None
    if args.tree:
        # 目录树跨清单共用：读一次就够，判重口径和 upload_from_json 完全一致
        tree_index = load_tree_index(args.tree, args.tree_root)
        print(f"📁 目录树: {tree_index.file_count:,} 个文件 / "
              f"{tree_index.dir_count:,} 个目录，先按它判重")
    print()

    succeeded = 0
    failed = 0
    totals = None

    with Pan123Client() as client:
        # 目录树缓存与目录快照跨清单共享：同一棵树只建一次、同一个目录只列一次；
        # 有目录树时即使 --no-dedup 也要快照（待传条目要靠它解析目录ID）
        index = None if (args.no_dedup and tree_index is None) else RemoteIndex(client)
        dir_tree = RemoteDirTree(client, args.directory or '', index)
        rate = None if args.rate <= 0 else args.rate

        for idx, fname in enumerate(manifests, 1):
            json_path = os.path.join(json_dir, fname)
            print(f"\n[{idx}/{total}] 处理: {fname}")
            t0 = time.time()

            try:
                remote_dir = manifest_remote_dir(json_path, args.directory)
                stats = upload_manifest(
                    client, json_path, remote_dir,
                    dir_tree=dir_tree, max_workers=args.workers, rate=rate,
                    index=index, dedup=not args.no_dedup,
                    tree_index=tree_index, tree_verify=args.tree_verify,
                    verbose=args.verbose)
            except KeyboardInterrupt:
                print("\n\n用户中断，已安全退出。")
                break
            except Exception as e:
                failed += 1
                print(f"  ❌ 失败: {e}")
                continue

            if totals is None:
                totals = stats
            else:
                totals.merge(stats)

            succeeded += 1
            elapsed = time.time() - t0
            print(f"  ✅ 完成 ({elapsed:.1f}s) {stats.breakdown_line()}")

    print("\n===== 汇总 =====")
    print(f"清单成功: {succeeded}, 清单失败: {failed}, 总计: {total}")
    if totals is not None:
        for line in totals.summary_lines():
            print(line)


if __name__ == "__main__":
    main()
