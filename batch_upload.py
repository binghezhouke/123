"""批量从 JSON 文件秒传到 123 云盘。

遍历指定目录下的所有 .json 文件，逐个调用 upload_from_json 秒传。
支持断点续传：已成功的 JSON 会记录在日志文件中，重跑时自动跳过。
"""

import argparse
import json
import os
import sys
import time

from api import Pan123Client
from upload_from_json import upload_from_json, file2json


def load_done_set(log_path: str) -> set:
    """从日志文件加载已完成的 JSON 文件名集合。"""
    if not os.path.exists(log_path):
        return set()
    with open(log_path, "r", encoding="utf-8") as f:
        return {line.strip() for line in f if line.strip()}


def append_done(log_path: str, filename: str):
    with open(log_path, "a", encoding="utf-8") as f:
        f.write(filename + "\n")


def main():
    parser = argparse.ArgumentParser(description="批量从 JSON 文件秒传到 123 云盘")
    parser.add_argument(
        "json_dir",
        help="包含 JSON 文件的目录路径",
    )
    parser.add_argument(
        "-d", "--directory",
        help="远程前缀目录路径 (可选, 每个 JSON 的 commonPath 会放在此目录下)",
        default=None,
    )
    parser.add_argument(
        "--log",
        help="已完成记录文件路径 (默认: <json_dir>/batch_done.log)",
        default=None,
    )
    parser.add_argument(
        "-w", "--workers",
        type=int, default=8,
        help="每个 JSON 并发秒传线程数 (默认: 8)",
    )
    args = parser.parse_args()

    json_dir = os.path.abspath(args.json_dir)
    if not os.path.isdir(json_dir):
        print(f"错误: 目录不存在: {json_dir}")
        sys.exit(1)

    log_path = args.log or os.path.join(json_dir, "batch_done.log")

    # 收集所有 JSON 文件并排序
    json_files = sorted(f for f in os.listdir(json_dir) if f.lower().endswith(".json"))
    total = len(json_files)
    if total == 0:
        print("目录中没有找到 .json 文件。")
        sys.exit(0)

    done_set = load_done_set(log_path)
    skipped = len(done_set & set(json_files))
    print(f"共 {total} 个 JSON 文件, 已完成 {skipped} 个, 待处理 {total - skipped} 个\n")

    success = 0
    failed = 0
    totals = {'hit': 0, 'miss': 0, 'fail': 0, 'skip': 0}
    shared_dirs = {}

    with Pan123Client() as client:
        for idx, fname in enumerate(json_files, 1):
            if fname in done_set:
                continue

            json_path = os.path.join(json_dir, fname)
            print(f"\n[{idx}/{total}] 处理: {fname}")
            t0 = time.time()

            try:
                # 如果指定了前缀目录，拼接 commonPath
                remote_dir = args.directory
                if remote_dir:
                    try:
                        with open(json_path, 'r', encoding='utf-8') as f:
                            data = json.load(f)
                    except json.JSONDecodeError:
                        data = file2json(json_path)
                    common_path = data.get('commonPath', '').strip('/')
                    if common_path:
                        remote_dir = os.path.join(remote_dir, common_path)

                counts = upload_from_json(
                    client, json_path, remote_dir,
                    shared_dir_map=shared_dirs, max_workers=args.workers)
                for k in totals:
                    totals[k] += counts[k]
                append_done(log_path, fname)
                success += 1
                elapsed = time.time() - t0
                print(f"  ✅ 完成 ({elapsed:.1f}s, 命中 {counts['hit']}, "
                      f"未命中 {counts['miss']}, 失败 {counts['fail']}, 跳过 {counts['skip']})")
            except KeyboardInterrupt:
                print("\n\n用户中断，已安全退出。")
                break
            except Exception as e:
                failed += 1
                print(f"  ❌ 失败: {e}")

    print(f"\n===== 汇总 =====")
    print(f"成功: {success + skipped}, 失败: {failed}, 总计: {total}")
    print(f"秒传命中: {totals['hit']}, 未命中: {totals['miss']}, "
          f"失败: {totals['fail']}, 跳过: {totals['skip']}")


if __name__ == "__main__":
    main()
