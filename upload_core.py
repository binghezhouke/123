"""上传相关的公共逻辑。

upload_folder.py / upload_from_json.py / batch_upload.py 三个脚本共用这里的实现：
清单解析、哈希归一化、远程目录树与目录快照、目录树索引、判重、秒传执行、限速与统计。
这样"什么算成功、什么算失败"只有一套口径。

判重完全以云端为准：要么用刚导出的目录树文件零请求预筛，要么现列目标目录核对，
不依赖任何本地"已完成"记录（本地记录出现过没传完就被判"已传完"的问题，已移除）。

关于限流：开放平台的限流是账号级的，实测短时间内在飞的请求超过 8 个就会返回
业务码 1（"请慢一点"），而且加并发不会提高吞吐（上限约 8 请求/秒）。所以这里
按速率节流，而不是靠加线程。
"""

import json
import os
import re
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import Any, Dict, Iterable, List, Optional, Tuple

from api.ratelimit import TokenBucket
from progress import RunProgress

try:
    import tqdm
except ImportError:  # tqdm 是可选依赖，没有就退回普通打印
    tqdm = None

# 秒传/跳过的几种结果
STATUS_HIT = 'hit'      # 命中：云端已有（秒传命中，或判重认定为同一文件）
STATUS_MISS = 'miss'    # 接口正常返回，但云端没有可复用的文件
STATUS_FAIL = 'fail'    # 调用失败（异常），不代表云端没有该文件
STATUS_SKIP = 'skip'    # 记录不完整、目录缺失等，未发起请求

STATUS_ORDER = (STATUS_HIT, STATUS_MISS, STATUS_FAIL, STATUS_SKIP)
STATUS_ICONS = {STATUS_HIT: '✓', STATUS_MISS: '⚠', STATUS_FAIL: '❌', STATUS_SKIP: '⏭'}
STATUS_LABELS = {
    STATUS_HIT: '命中',
    STATUS_MISS: '未命中',
    STATUS_FAIL: '失败',
    STATUS_SKIP: '跳过',
}

# 默认限速：实测账号级上限约 8 请求/秒
DEFAULT_RATE = 8.0


def emit(message: str) -> None:
    """在有进度条的情况下也要保证输出不被覆盖。"""
    if tqdm is not None:
        tqdm.tqdm.write(message)
    else:
        print(message)


# ---------------------------------------------------------------- 统计

@dataclass
class UploadStats:
    """一次上传的结果计数"""

    hit: int = 0
    miss: int = 0
    fail: int = 0
    skip: int = 0
    # hit 里有多少是秒传命中的（本地目录上传时用得上）
    reuse: int = 0
    # hit 里有多少是靠目标目录快照判重跳过的
    deduped: int = 0
    # hit 里有多少是靠目录树导出文件按名字判掉、未校验大小/内容的
    tree: int = 0
    # 云端已有而未发请求的条数（目录树按名 / MD5 一致 / 同名同大小），
    # 跨"命中"和"跳过"两个口径，所以单独计数
    existed: int = 0
    # 出错的明细，便于最后排查（路径: 原因）
    failures: List[Tuple[str, str]] = field(default_factory=list)
    # 同名同大小但内容不一致的冲突，需要人工确认
    conflicts: List[Tuple[str, str]] = field(default_factory=list)
    # 未命中的文件（云端没有、需要另行常规上传）
    misses: List[str] = field(default_factory=list)

    def add(self, status: str, path: str = '', detail: str = '') -> None:
        setattr(self, status, getattr(self, status) + 1)
        if status == STATUS_FAIL:
            self.failures.append((path, detail))
        elif status == STATUS_MISS:
            self.misses.append(path)

    def add_reuse(self, path: str = '', detail: str = '') -> None:
        """记一次秒传命中（同时也计入 hit）"""
        self.hit += 1
        self.reuse += 1

    def add_deduped(self, path: str = '', detail: str = '') -> None:
        """记一次因目标目录已有同一文件而跳过（同时也计入 hit 和 existed）"""
        self.hit += 1
        self.deduped += 1
        self.existed += 1

    def add_tree(self, path: str = '', detail: str = '') -> None:
        """记一次目录树按名字判掉（同时也计入 hit 和 existed，未校验大小/内容）"""
        self.hit += 1
        self.tree += 1
        self.existed += 1

    def add_loose(self, path: str = '') -> None:
        """记一次"同名同大小（未校验内容）"跳过（计入 skip 和 existed）"""
        self.skip += 1
        self.existed += 1

    def merge(self, other: "UploadStats") -> None:
        self.hit += other.hit
        self.miss += other.miss
        self.fail += other.fail
        self.skip += other.skip
        self.reuse += other.reuse
        self.deduped += other.deduped
        self.tree += other.tree
        self.existed += other.existed
        self.failures.extend(other.failures)
        self.conflicts.extend(other.conflicts)
        self.misses.extend(other.misses)

    @property
    def total(self) -> int:
        return self.hit + self.miss + self.fail + self.skip

    # 汇总里最多列这么多条明细，避免刷屏（-v 会逐条实时打印）
    MAX_LISTED = 10

    def _listed(self, paths: List[str]) -> List[str]:
        lines = [f"    {path}" for path in paths[:self.MAX_LISTED]]
        if len(paths) > self.MAX_LISTED:
            lines.append(f"    ... 其余 {len(paths) - self.MAX_LISTED} 条见 -v 输出")
        return lines

    def breakdown_line(self) -> str:
        """一行结果构成（汇总首行和 batch 的逐清单输出共用）：
        总数、已有（含构成：目录树/MD5 一致/同名同大小）、秒传、未命中、失败、其他跳过
        """
        loose = self.existed - self.tree - self.deduped
        detail = []
        if self.tree:
            detail.append(f"目录树 {self.tree:,}")
        if self.deduped:
            detail.append(f"MD5 一致 {self.deduped:,}")
        if loose:
            detail.append(f"同名同大小 {loose:,}")
        existed = (f"已有 {self.existed:,}（{'，'.join(detail)}）" if detail
                   else f"已有 {self.existed:,}")
        parts = [existed]
        if self.reuse:
            parts.append(f"秒传 {self.reuse:,}")
        if self.miss:
            parts.append(f"未命中 {self.miss:,}")
        if self.fail:
            parts.append(f"失败 {self.fail:,}")
        other_skip = self.skip - loose
        if other_skip:
            parts.append(f"其他跳过 {other_skip:,}")
        return f"合计 {self.total:,} 条：" + "，".join(parts)

    def summary_lines(self) -> List[str]:
        lines = [self.breakdown_line()]
        for path, detail in self.conflicts:
            lines.append(f"  ⚠ 内容冲突 {path}: {detail}")
        for path, detail in self.failures:
            lines.append(f"  ❌ {path}: {detail}")
        if self.misses:
            lines.append(f"  ⚠ 未命中（云端没有，需要另行上传）{self.miss} 条:")
            lines.extend(self._listed(self.misses))
        return lines


# ---------------------------------------------------------------- 清单解析

def parse_export_text(text: str) -> Dict[str, Any]:
    """解析 123 云盘导出的清单文本（``etag#size#path$etag#size#path$...``）。"""
    out: Dict[str, Any] = {"usesBase62EtagsInExport": True, "files": []}
    for chunk in text.strip().split("$"):
        parts = chunk.strip().split("#")
        if len(parts) == 3:
            out["files"].append({
                "etag": parts[0],
                "size": parts[1],
                "path": parts[2],
            })
    return out


def load_manifest(json_file_path: str) -> Dict[str, Any]:
    """读取清单文件：优先按 JSON 解析，失败则按导出文本解析。"""
    with open(json_file_path, encoding='utf-8') as f:
        content = f.read()
    try:
        return json.loads(content)
    except json.JSONDecodeError:
        return parse_export_text(content)


# ---------------------------------------------------------------- 哈希

def decode_hash(raw_value: str, uses_base62: bool = False) -> Tuple[str, str]:
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


# ---------------------------------------------------------------- 远程目录

def normalize_remote_path(path: Optional[str]) -> str:
    """统一远程路径：去掉首尾斜杠、分隔符转成 '/'。"""
    if not path:
        return ""
    normalized = path.strip().strip('/')
    while '//' in normalized:
        normalized = normalized.replace('//', '/')
    return normalized


class RemoteIndex:
    """目标目录内容快照，用于判重和复用目录ID。

    快照按目录懒加载：只有清单里真的会用到的目录才会被列出来。
    列表接口返回的文件记录带 32 位 MD5 etag，目录记录带目录ID，
    所以判重和"免建目录"都靠它。
    """

    def __init__(self, client, display=None):
        self.client = client
        self.display = display
        self._files: Dict[int, Dict[Tuple[str, int], str]] = {}
        self._dirs: Dict[int, Dict[str, int]] = {}
        self._paths: Dict[int, str] = {}   # 目录ID -> 展示用路径

    def remember_path(self, dir_id: int, path: str) -> None:
        """记住目录对应的路径，读快照时能打出人能看懂的名字"""
        self._paths[dir_id] = f"/{normalize_remote_path(path)}" if normalize_remote_path(path) else "/"

    def _ensure_loaded(self, dir_id: int) -> None:
        if dir_id in self._files:
            return

        label = self._paths.get(dir_id) or f"目录 {dir_id}"
        # 列大目录可能要几十秒（每页 100 条 + 限速），这一步必须有进度反馈
        if self.display is not None:
            self.display.dir_started(label)

        def on_page(page_no: int, page_items: int, total_items: int) -> None:
            if self.display is not None:
                self.display.dir_page(label, page_no, total_items)

        file_list, _ = self.client.file_service.list_files(
            parent_id=dir_id, auto_fetch_all=True, on_page=on_page)

        files: Dict[Tuple[str, int], str] = {}
        dirs: Dict[str, int] = {}
        for item in file_list:
            if item.is_folder:
                dirs[item.filename] = item.file_id
            else:
                files[(item.filename, item.size)] = item.etag or ""
        self._files[dir_id] = files
        self._dirs[dir_id] = dirs

        if self.display is not None:
            self.display.dir_finished(label, len(files), len(dirs))

    def find_file(self, dir_id: int, name: str, size) -> Optional[str]:
        """
        查目标目录里有没有同名同大小的文件。

        :return: 命中时返回远端 etag（可能是空字符串），没有则返回 None
        """
        self._ensure_loaded(dir_id)
        return self._files[dir_id].get((name, size))

    def find_dir(self, parent_id: int, name: str) -> Optional[int]:
        """查子目录ID，避免为了一个已存在的目录再发 mkdir。"""
        self._ensure_loaded(parent_id)
        return self._dirs[parent_id].get(name)

    def remember_file(self, dir_id: int, name: str, size, etag: str = "") -> None:
        """上传成功后更新快照，避免同一批次里的重复路径被处理两次。"""
        if dir_id in self._files:
            self._files[dir_id][(name, size)] = etag

    def remember_dir(self, parent_id: int, name: str, dir_id: int) -> None:
        """新建子目录后补进父目录的快照，同一次运行里不必再列/再建。"""
        if parent_id in self._dirs:
            self._dirs[parent_id][name] = dir_id


class RemoteDirTree:
    """远程目录路径 -> 目录ID 的缓存，逐级创建。

    目录创建保持串行（官方 mkdir 限流 20 QPS），已建过的层级会复用；
    装了 RemoteIndex 时，已存在的目录直接从快照里取 ID，完全不发 mkdir。
    """

    def __init__(self, client, root_path: str = "", index: RemoteIndex = None, display=None):
        self.client = client
        self.root_path = normalize_remote_path(root_path)
        self.index = index
        self.display = display
        self._ids: Dict[str, int] = {"": 0}
        # 本次运行中由我们创建的目录：它们不可能出现在快照里，跳过查询
        self._created: set = set()

    @property
    def root_id(self) -> int:
        """根目录ID（首次访问时才真正创建，dry-run 场景不会调用到这里）"""
        return self.ensure(self.root_path)

    def ensure(self, path: str) -> int:
        """按需创建 path（及其父级），返回目录ID。"""
        key = normalize_remote_path(path)
        if key in self._ids:
            return self._ids[key]

        parent_path, _, name = key.rpartition('/')
        parent_id = self.ensure(parent_path)

        if self.index is not None:
            self.index.remember_path(parent_id, parent_path)
            if parent_path not in self._created:
                found = self.index.find_dir(parent_id, name)
                if found is not None:
                    self._ids[key] = found
                    self.index.remember_path(found, key)
                    return found

        message = f"  🏗 创建目录: /{key}"
        if self.display is not None:
            self.display.log(message)
        else:
            emit(message)
        dir_id = self.client.file_service.mkdir(name, parent_id)
        self._ids[key] = dir_id
        self._created.add(key)
        if self.index is not None:
            self.index.remember_path(dir_id, key)
            # 新建的目录不在旧快照里，补进父目录的快照，
            # 同一次运行里就不会为了它再列目录/再发 mkdir
            self.index.remember_dir(parent_id, name, dir_id)
        return dir_id

    def lookup(self, path: str) -> Optional[int]:
        """只查缓存，不创建目录。用于并发阶段避免多线程同时建同一个目录。"""
        return self._ids.get(normalize_remote_path(path))

    def was_created(self, path: str) -> bool:
        """这个目录是不是本次运行里由我们 mkdir 出来的（新目录必然是空的）。"""
        return normalize_remote_path(path) in self._created

    def ensure_dirs(self, paths: Iterable[str]) -> List[Tuple[str, str]]:
        """批量预建目录（去重后串行执行）；返回 [(失败路径, 错误信息), ...]"""
        targets = sorted({normalize_remote_path(p) for p in paths if normalize_remote_path(p)})
        if self.display is not None:
            # 这一步是真正耗时的部分（列父目录 + mkdir），要把总数报给进度区
            self.display.set_total_dirs(len(targets))
        failures: List[Tuple[str, str]] = []
        for path in targets:
            try:
                self.ensure(path)
            except Exception as e:
                failures.append((path, str(e)))
            finally:
                if self.display is not None:
                    self.display.dir_resolved()
        return failures


# ---------------------------------------------------------------- 目录树索引

# 网页端"导出目录树"文本的首行：导出的是整个网盘的根，对应远程路径 ""
TREE_ROOT_ALIAS = "我的文件"


def _size_matches(a, b) -> bool:
    """清单里的 size 可能是字符串（导出文本）也可能是数字，按数值比"""
    try:
        return int(a) == int(b)
    except (TypeError, ValueError):
        return a == b


class ExportTreeIndex:
    """从导出文件建的"远程路径 → 内容"索引，用来不发请求地判重。

    支持两种导出：
    - 目录树文本（网页端"导出目录树"，├─/└─ 画树那种）：**只有文件名**，
      判重只能按"同目录 + 同名"匹配，大小和内容校验不了；
    - 清单文本/JSON（etag#size#path$...）：带大小和 MD5，判重口径和列目录一致。

    目录树索引没有目录ID，所以只能用于判重；真的要传文件时，目标目录还是
    得通过列目录/mkdir 解析出 ID（只解析有待传文件的目录）。
    """

    def __init__(self):
        # 远程目录路径 -> {文件名 -> {'size': 数值或 None, 'etag': md5 或 ''}}
        self._files: Dict[str, Dict[str, dict]] = {}
        # 导出里出现过的目录路径（含中间层级），用来判断"这条路径归不归目录树管"
        self._dirs: set = set()
        self.root_path = ""   # 导出根对应的远程路径（不算进 dir_count）

    @property
    def file_count(self) -> int:
        return sum(len(entries) for entries in self._files.values())

    @property
    def dir_count(self) -> int:
        return len(self._dirs - {self.root_path})

    # ------------------------------------------------------------ 构建

    @classmethod
    def from_file(cls, path: str, tree_root: str = None) -> "ExportTreeIndex":
        """按内容识别导出格式（目录树文本 / 清单文本 / JSON）并建索引。"""
        with open(path, encoding='utf-8') as f:
            content = f.read()
        if content.lstrip().startswith('{'):
            return cls.from_manifest(json.loads(content), tree_root)
        if '├─' in content or '└─' in content:
            return cls.from_tree_text(content, tree_root)
        return cls.from_manifest(parse_export_text(content), tree_root,
                                 uses_base62=True)

    @classmethod
    def from_tree_text(cls, text: str, tree_root: str = None) -> "ExportTreeIndex":
        """解析网页端导出的目录树文本。

        目录行带 ├─/└─ 标记，标记前的每级缩进是 4 个字符（"│   " 或空格）；
        文件行没有标记，归属它上面最近的一个目录行。
        """
        lines = [line for line in text.splitlines() if line.strip()]
        if not lines:
            raise ValueError("目录树文件是空的")

        root_name = lines[0].strip()
        if tree_root is not None:
            root = normalize_remote_path(tree_root)
        else:
            root = "" if root_name == TREE_ROOT_ALIAS else root_name

        index = cls()
        index.root_path = root
        index._dirs.add(root)
        stack = [root_name]   # 只用来算相对层级，stack[0] 是导出的根
        current = root
        for line in lines[1:]:
            marker = line.find('├─')
            if marker < 0:
                marker = line.find('└─')
            if marker >= 0:
                depth = marker // 4
                if marker % 4 or depth > len(stack) - 1:
                    raise ValueError(f"目录树缩进对不上: {line!r}")
                stack = stack[:depth + 1] + [line[marker + 2:].strip()]
                current = normalize_remote_path('/'.join([root] + stack[1:]))
                index._dirs.add(current)
                index._files.setdefault(current, {})
            else:
                # 文件行没有标记，前缀是祖先分支的"│"+缩进，去掉后剩下的就是文件名
                index._files.setdefault(current, {})[line.strip('│ ')] = \
                    {'size': None, 'etag': ''}
        return index

    @classmethod
    def from_manifest(cls, data: Dict[str, Any], tree_root: str = None,
                      uses_base62: bool = False) -> "ExportTreeIndex":
        """从清单格式的导出（带大小/哈希）建索引，判重口径和列目录一致。"""
        root = normalize_remote_path(tree_root) if tree_root is not None \
            else normalize_remote_path(data.get('commonPath', ''))
        index = cls()
        index.root_path = root
        index._dirs.add(root)
        for record in data.get('files', []):
            path = normalize_remote_path(record.get('path') or '')
            if not path:
                continue
            dir_path, _, name = path.rpartition('/')
            hash_hex, hash_type = decode_hash(record.get('etag') or record.get('sha1'),
                                              uses_base62)
            full_dir = normalize_remote_path(f"{root}/{dir_path}") if dir_path else root
            # 中间层级都记上，covers() 才能判断这条路径归不归目录树管
            parts = full_dir.split('/') if full_dir else []
            for i in range(len(parts)):
                index._dirs.add('/'.join(parts[:i + 1]))
            index._files.setdefault(full_dir, {})[name] = {
                'size': record.get('size'),
                'etag': hash_hex if hash_type == 'md5' else '',
            }
        return index

    # ------------------------------------------------------------ 查询

    def covers(self, dir_path: str) -> bool:
        """这条远程路径在导出覆盖的子树里吗？不在的话判重结果作不得数。

        从路径往上找：命中导出里列过的目录就算覆盖；一路找到导出根都没
        命中（比如导出只含 fffff，问的是 图片/xx）就不算。
        """
        path = normalize_remote_path(dir_path)
        if path == self.root_path:
            return True
        while path and path != self.root_path:
            if path in self._dirs:
                return True
            path = path.rsplit('/', 1)[0] if '/' in path else ""
        return False

    def find_file(self, dir_path: str, name: str) -> Optional[dict]:
        """查目录树里这个位置有没有这个文件；返回 {'size', 'etag'}，没有返回 None"""
        return self._files.get(normalize_remote_path(dir_path), {}).get(name)


def load_tree_index(path: str, tree_root: str = None) -> ExportTreeIndex:
    """读导出文件建目录树索引（给 CLI 用，测试也可以直接用）。"""
    return ExportTreeIndex.from_file(path, tree_root)


# ---------------------------------------------------------------- 秒传

def reuse_hashes(client, filename: str, parent_id: int, hash_hex: str,
                 hash_type: str, size) -> Tuple[str, str]:
    """
    按已解析好的哈希尝试秒传，返回 (status, detail)。

    调用本身失败会抛出异常，由调用方记成 fail —— 这样"云端没有这个文件"
    和"这次调用挂了"不会混为一谈。
    """
    try:
        file_size = int(size)
    except (TypeError, ValueError):
        return STATUS_SKIP, f"文件大小无效: {size}"

    if hash_type == 'sha1':
        result = client.file_service.try_sha1_reuse(
            local_path=None, filename=filename, parent_id=parent_id,
            duplicate=1, sha1=hash_hex, size=file_size)
        if result and result.get('reuse'):
            return STATUS_HIT, f"SHA1秒传成功 (fileID={result.get('fileID')})"
        return STATUS_MISS, "SHA1秒传未命中 (云端无此文件)"

    if hash_type == 'md5':
        result = client.file_service.create_file(
            parent_id=parent_id, filename=filename, etag=hash_hex,
            size=file_size, duplicate=1)
        if result.get('reuse'):
            return STATUS_HIT, f"MD5秒传成功 (fileID={result.get('fileID')})"
        if result.get('skipped'):
            return STATUS_SKIP, "云端已存在同名同大小文件"
        return STATUS_MISS, "MD5秒传未命中 (需要常规上传)"

    return STATUS_SKIP, f"未知哈希类型: {hash_type}"


def reuse_file(client, filename: str, parent_id: int, raw_hash: str, size,
               uses_base62: bool = False) -> Tuple[str, str]:
    """按清单里的原始哈希值尝试秒传（先做哈希归一化）。"""
    hash_hex, hash_type = decode_hash(raw_hash, uses_base62)
    if not hash_hex:
        return STATUS_SKIP, f"缺少有效的哈希值 (原始值: {raw_hash})"
    return reuse_hashes(client, filename, parent_id, hash_hex, hash_type, size)


# ---------------------------------------------------------------- 清单上传

@dataclass
class _Pending:
    """一个待处理的清单条目（已完成目录解析与哈希归一化）"""

    rel_path: str          # 清单里的原始路径
    display_path: str      # 打印用路径
    filename: str
    parent_id: int         # 目标目录ID（有目录树时先判重、后解析，此时可能为 None）
    size: Any
    raw_hash: str
    hash_hex: str
    hash_type: str
    dir_path: str = ""     # 目标目录的完整远程路径（目录树判重用）


def _configure_rate(client, rate: Optional[float]) -> None:
    """给客户端装上账号级限速器（已经装过就不重复装）。"""
    if rate is None:
        return
    http_client = getattr(client, 'http_client', None)
    if http_client is not None and getattr(http_client, 'rate_limiter', None) is None:
        http_client.set_rate_limiter(TokenBucket(rate))


def upload_manifest(client, json_file_path: str, remote_dir: str = None,
                    dir_tree: RemoteDirTree = None, max_workers: int = 8,
                    progress: bool = True, rate: Optional[float] = DEFAULT_RATE,
                    index: RemoteIndex = None,
                    dedup: bool = True, tree_index: "ExportTreeIndex" = None,
                    tree_verify: bool = False, verify_sha1: bool = False,
                    verbose: bool = False) -> UploadStats:
    """
    从清单文件（JSON 或导出文本）秒传到远程目录。

    处理顺序（越靠前越省接口调用）：
    1. 目录树索引判重（传了 tree_index 时）-> 按名字零请求跳过
    2. 目标目录快照判重 -> 云端已有同一文件就不发请求（也避免造出重复文件）
    3. 并发秒传（受账号级限速约束）
    4. 失败的串行重试一轮

    判重只看云端现状（目录树导出 / 现列目录），不依赖任何本地"已完成"记录。

    :param remote_dir: 远程根目录；为空则使用清单里的 commonPath
    :param dir_tree: 跨清单共享的目录树缓存（batch 场景传入以复用整棵树）
    :param rate: 每秒请求数上限，None 表示不限速
    :param index: 跨清单共享的目录快照
    :param dedup: 是否启用目录快照判重
    :param tree_index: 目录树导出文件建的路径索引，先按它零请求判重（见 ExportTreeIndex）
    :param tree_verify: 目录树按名字命中后仍列目录核实大小/MD5（默认按名字跳过）
    :param verify_sha1: 清单只有 SHA1 时也强制发秒传请求确认，而不是按同名同大小跳过
    :param verbose: 逐条打印每个文件的结果（默认只打印失败项，其余看进度条与汇总）
    :return: UploadStats
    """
    stats = UploadStats()

    if not os.path.exists(json_file_path):
        raise FileNotFoundError(f"清单文件不存在: {json_file_path}")

    _configure_rate(client, rate)

    data = load_manifest(json_file_path)
    uses_base62 = data.get('usesBase62EtagsInExport', False)
    files_to_upload = data.get('files', [])

    base_path = normalize_remote_path(remote_dir) or \
        normalize_remote_path(data.get('commonPath', ''))
    if not base_path:
        raise ValueError("清单里没有 commonPath，且未指定远程目录")

    # 整个过程（准备目录 → 判重 → 秒传 → 重试）共用一块两行进度区：第一行是
    # 阶段与当前动作，第二行是整体进度条（一条记录到终态就 +1，不随阶段归零）
    display = RunProgress(len(files_to_upload), enabled=progress)
    try:
        stats = _upload_manifest_inner(
            client, files_to_upload, uses_base62, base_path,
            dir_tree, index, max_workers, dedup,
            verify_sha1, verbose, display,
            tree_index=tree_index, tree_verify=tree_verify)
    finally:
        display.close()
    return stats


def _upload_manifest_inner(client, files_to_upload, uses_base62, base_path,
                           dir_tree, index, max_workers,
                           dedup, verify_sha1, verbose, display,
                           tree_index=None, tree_verify=False) -> UploadStats:
    stats = UploadStats()
    display.attach_stats(stats)

    # 有目录树时始终要目录快照：判重剩下的待传条目要靠它解析目录ID（免得重复 mkdir）
    if not dedup and tree_index is None:
        index = None
    elif index is None:
        index = RemoteIndex(client, display=display)
    else:
        index.display = display
    tree = dir_tree if dir_tree is not None else RemoteDirTree(client, base_path, index, display)
    tree.index = index
    tree.display = display

    # ------------------------------------------------------------ 判重公共件

    def parse_entry(file_info) -> Optional[_Pending]:
        """解析一条清单记录；记录本身有问题就记成跳过并返回 None"""
        rel_path = file_info.get('path') or ''
        dir_path, filename = os.path.split(rel_path)
        if not filename:
            stats.add(STATUS_SKIP, rel_path, "记录缺少文件路径")
            display.entry_judged('dropped')
            return None
        size = file_info.get('size')
        raw_hash = file_info.get('sha1') or file_info.get('etag')
        hash_hex, hash_type = decode_hash(raw_hash, uses_base62)
        return _Pending(
            rel_path=rel_path, display_path=rel_path, filename=filename,
            parent_id=None, size=size, raw_hash=raw_hash,
            hash_hex=hash_hex, hash_type=hash_type,
            dir_path=f"{base_path}/{dir_path}" if dir_path else base_path)

    def drop_bad_hash(item: _Pending) -> None:
        stats.add(STATUS_SKIP, item.rel_path,
                  f"缺少有效的哈希值 (原始值: {item.raw_hash})")
        display.entry_judged('dropped')

    def resolve_existing(item: _Pending, remote_etag: str) -> bool:
        """目标位置已有同名同大小文件：按 MD5/宽松规则判掉。True = 已解决。"""
        if item.hash_type == 'md5' and remote_etag and remote_etag == item.hash_hex:
            stats.add_deduped(item.rel_path, "云端已有相同文件")
            display.entry_judged('existing')
            if verbose:
                display.log(f"  ✓ {item.rel_path}: 云端已有相同文件 (MD5 一致)，跳过",
                            always=True)
            return True
        if item.hash_type == 'md5' and remote_etag and remote_etag != item.hash_hex:
            detail = (f"云端同名同大小但 MD5 不同"
                      f" (远端 {remote_etag[:8]}…, 清单 {item.hash_hex[:8]}…)")
            stats.skip += 1
            stats.conflicts.append((item.rel_path, detail))
            display.entry_judged('existing')
            display.log(f"⚠ {item.rel_path}: {detail}，已跳过，请人工确认",
                        always=True)
            return True
        if not verify_sha1:
            stats.add_loose(item.rel_path)
            display.entry_judged('existing')
            if verbose:
                display.log(f"  ⏭ {item.rel_path}: 云端已有同名同大小文件，跳过",
                            always=True)
            return True
        return False

    todo: List[_Pending] = []

    if tree_index is not None:
        # 有目录树：先按路径判重（零请求），判完只给真的要传的条目准备目录。
        # 全部命中时一个请求都不发
        display.begin_judging()
        display.log(f"📁 按目录树判重（{tree_index.file_count:,} 个文件，"
                    f"{tree_index.dir_count:,} 个目录）"
                    + ("，名字命中后仍会列目录核实" if tree_verify
                       else "，按文件名匹配") + "...")
        verify_items: List[_Pending] = []
        for file_info in files_to_upload:
            item = parse_entry(file_info)
            if item is None:
                continue
            if not item.hash_hex:
                drop_bad_hash(item)
                continue

            entry = None
            if tree_index.covers(item.dir_path):
                entry = tree_index.find_file(item.dir_path, item.filename)
                if entry is not None and entry.get('size') is not None \
                        and not _size_matches(entry['size'], item.size):
                    entry = None   # 同名但大小不同：不算已存在
            if entry is not None and entry.get('size') is not None \
                    and resolve_existing(item, entry.get('etag') or ''):
                continue
            if entry is not None:
                if tree_verify:
                    verify_items.append(item)
                    display.entry_judged('pending')
                    continue
                # 目录树只有名字：按名字跳过，不校验大小/内容；
                # 重跑会按最新的目录树导出重新判
                stats.add_tree(item.rel_path)
                display.entry_judged('existing')
                if verbose:
                    display.log(f"  ✓ {item.rel_path}: 目录树里已有同名文件，"
                                f"跳过（未校验大小/内容）", always=True)
                continue
            todo.append(item)
            display.entry_judged('pending')

        display.log(f"→ 需要处理的文件 {len(todo) + len(verify_items)} 条"
                    f"（已跳过 {stats.total} 条）"
                    if todo or verify_items
                    else f"→ {stats.total} 条记录已在云端，无需处理")

        if todo or verify_items:
            display.set_phase("📂 准备目录")
            display.log(f"将在远程路径 '{base_path}' 中创建文件结构...")
            wanted = sorted({it.dir_path for it in todo}
                            | {it.dir_path for it in verify_items})
            dir_failures = {path: err for path, err in tree.ensure_dirs(wanted)}
            for path, err in dir_failures.items():
                display.log(f"创建子目录 '{path}' 失败: {err}", always=True)

            def dir_failed(item: _Pending) -> None:
                """目录没建出来：这条记成跳过（判重时已按 pending 计过数）"""
                reason = dir_failures.get(item.dir_path, "目录未创建")
                stats.add(STATUS_SKIP, item.rel_path, f"目录创建失败: {reason}")
                display.entry_settled()

            # --tree-verify：名字命中的条目列目录核实，规则和经典判重一致
            for item in verify_items:
                parent_id = tree.lookup(item.dir_path)
                if parent_id is None:
                    dir_failed(item)
                    continue
                item.parent_id = parent_id
                remote_etag = index.find_file(parent_id, item.filename, item.size) \
                    if index is not None else None
                if remote_etag is not None and resolve_existing(item, remote_etag):
                    continue
                todo.append(item)

            ready = []
            for item in todo:
                parent_id = tree.lookup(item.dir_path)
                if parent_id is None:
                    dir_failed(item)
                    continue
                item.parent_id = parent_id
                ready.append(item)
            todo = ready
    else:
        display.set_phase("📂 准备目录")
        display.log(f"将在远程路径 '{base_path}' 中创建文件结构...")
        # 先确保根目录存在：失败要在这里就报出来，而不是等到并发阶段
        tree.ensure(base_path)

        # 阶段1：串行建目录树（只处理还没建过的目录）
        pending_dirs = set()
        for file_info in files_to_upload:
            dir_path = os.path.split(file_info.get('path') or '')[0]
            if dir_path:
                pending_dirs.add(f"{base_path}/{dir_path}")
        dir_failures = {path: err for path, err in tree.ensure_dirs(pending_dirs)}
        for path, err in dir_failures.items():
            display.log(f"创建子目录 '{path}' 失败: {err}", always=True)

        # 阶段2：逐条解析 + 判重（不发请求）
        display.begin_judging()
        if index is not None:
            display.log(f"🔍 检查 {len(files_to_upload)} 条记录是否已在云端"
                        + ("（逐条明细见下文）" if verbose else "..."))
        for file_info in files_to_upload:
            item = parse_entry(file_info)
            if item is None:
                continue

            parent_id = tree.lookup(item.dir_path)
            if parent_id is None:
                reason = dir_failures.get(item.dir_path, "目录未创建")
                stats.add(STATUS_SKIP, item.rel_path, f"目录创建失败: {reason}")
                display.entry_judged('dropped')
                continue
            item.parent_id = parent_id

            if not item.hash_hex:
                drop_bad_hash(item)
                continue

            if index is not None:
                remote_etag = index.find_file(parent_id, item.filename, item.size)
                if remote_etag is not None and resolve_existing(item, remote_etag):
                    continue

            todo.append(item)
            display.entry_judged('pending')

        # 此时 stats 里只统计了判重阶段解决的条目（还没发请求的那些）
        skipped = stats.total
        display.log(f"→ 需要处理的文件 {len(todo)} 条（已跳过 {skipped} 条）"
                    if todo else f"→ {skipped} 条记录已在云端，无需处理")

    # 阶段3：并发秒传（限速器会压住实际速率）
    def handle(item: _Pending) -> Tuple[str, str]:
        return reuse_hashes(client, item.filename, item.parent_id,
                            item.hash_hex, item.hash_type, item.size)

    def run(items: List[_Pending], workers: int) -> List[Tuple[_Pending, str]]:
        """跑一轮，返回仍然失败的 [(条目, 错误信息), ...]（不计入统计）"""
        if not items:
            return []
        failed: List[Tuple[_Pending, str]] = []
        executor = ThreadPoolExecutor(max_workers=workers)
        try:
            futures = {executor.submit(handle, it): it for it in items}
            for future in as_completed(futures):
                item = futures[future]
                try:
                    status, detail = future.result()
                except Exception as e:
                    failed.append((item, str(e)))
                    display.log(f"  {STATUS_ICONS[STATUS_FAIL]} {item.display_path}: {e}",
                                always=True)
                    continue

                if status == STATUS_HIT:
                    stats.add_reuse(item.display_path, detail)
                    if index is not None:
                        index.remember_file(item.parent_id, item.filename,
                                            item.size, item.hash_hex)
                elif status == STATUS_SKIP:
                    stats.add(status, item.display_path, detail)
                else:
                    stats.add(status, item.display_path, detail)
                display.entry_done()

                # 默认只逐条打印需要人处理的（失败），其余交给进度条和汇总
                if verbose or status == STATUS_FAIL:
                    display.log(f"  {STATUS_ICONS[status]} {item.display_path}: {detail}",
                                always=True)
        except KeyboardInterrupt:
            executor.shutdown(wait=False, cancel_futures=True)
            raise
        finally:
            executor.shutdown(wait=True)
        return failed

    # 没有待处理条目就不进入秒传阶段（进度区也保留在判重的最终状态）
    if todo:
        display.begin_upload(max_workers)
    failed = run(todo, max_workers)

    # 阶段4：失败的串行重试一轮（限速下再碰碰运气，仍失败就如实报出来）
    if failed:
        display.log(f"  ↻ {len(failed)} 个文件失败，串行重试一轮...", always=True)
        display.begin_retry(len(failed))
        still_failed = run([item for item, _ in failed], 1)
        reason_by_path = {item.rel_path: err for item, err in failed}
        for item, err in still_failed:
            stats.add(STATUS_FAIL, item.display_path,
                      reason_by_path.get(item.rel_path, err))

    # 收掉进度区再报"处理完成"，别让最终快照行落在它后面
    display.close()
    display.log("处理完成", always=True)
    return stats


# ---------------------------------------------------------------- 本地目录上传

@dataclass
class _LocalFile:
    """一个待上传的本地文件（目录已解析、快照判重已通过）"""

    local_file: str
    remote_file: str      # 打印用完整远程路径
    filename: str
    parent_id: int
    size: int


def upload_directory(client, local_path: str, remote_path: str = "",
                     dry_run: bool = False, progress: bool = True,
                     rate: Optional[float] = DEFAULT_RATE, index: RemoteIndex = None,
                     verbose: bool = False, max_workers: int = 8) -> UploadStats:
    """
    递归上传本地目录到远程路径。

    处理顺序（与清单上传同一套思路，越靠前越省接口调用）：
    1. 串行准备目录：逐级创建并缓存，已存在的目录从快照取 ID，不发 mkdir
    2. 快照判重：同名同大小文件零请求跳过（目录在解析时已列过一次）
    3. 并发上传（受账号级限速约束）：先试 SHA1 秒传，未命中走分片上传
    4. 失败的串行重试一轮

    verbose=False 时只打印失败项，其余交给进度条与汇总。
    """
    stats = UploadStats()
    local_path = os.path.abspath(local_path)

    if not dry_run:
        _configure_rate(client, rate)
        if index is None:
            index = RemoteIndex(client)
    tree = RemoteDirTree(client, remote_path, index)

    # 只遍历一遍：既数总数供进度显示，也逐目录处理
    walk = list(os.walk(local_path))
    total_files = sum(len(filenames) for _, _, filenames in walk)
    if total_files == 0:
        emit("本地目录中没有要上传的文件。")
        return stats

    bar = tqdm.tqdm(total=total_files, unit='file',
                    desc=f"上传到: /{tree.root_path}" if tree.root_path else "上传到: /") \
        if (progress and tqdm) else None

    def tick():
        if bar:
            bar.update(1)

    def handle(item: _LocalFile):
        # skip_if_exists=False：判重已在准备阶段用快照做过，
        # 不再让 upload_file 为每个文件全量列一遍目标目录
        return client.file_service.upload_file(
            local_path=item.local_file,
            parent_id=item.parent_id,
            filename=item.filename,
            skip_if_exists=False,
            try_sha1_reuse=True)

    def count_result(item: _LocalFile, result) -> None:
        """按 upload_file 的返回记账（跳过/秒传/常规上传/失败口径不变）"""
        if result and result.get('skipped'):
            stats.add(STATUS_SKIP, item.remote_file, "已存在同名同大小文件")
            if verbose:
                emit(f"  {STATUS_ICONS[STATUS_SKIP]} 跳过已存在文件: {item.filename}")
        elif result:
            if result.get('method') == 'sha1_reuse' or result.get('reuse'):
                stats.add_reuse(item.remote_file, "SHA1秒传")
                if verbose:
                    emit(f"  {STATUS_ICONS[STATUS_HIT]} 秒传成功: {item.filename}")
            else:
                stats.add(STATUS_HIT, item.remote_file, "已上传")
                if verbose:
                    emit(f"  {STATUS_ICONS[STATUS_HIT]} 上传成功: {item.filename}")
            # 上传成功后补进快照，同批次里再遇到同一文件就不会传第二遍
            if index is not None:
                index.remember_file(item.parent_id, item.filename, item.size)
        else:
            stats.add(STATUS_FAIL, item.remote_file, "接口未返回结果")
            emit(f"  {STATUS_ICONS[STATUS_FAIL]} 上传返回失败: {item.filename}")

    def run(items: List[_LocalFile], workers: int,
            tick_done: bool = True) -> List[Tuple[_LocalFile, str]]:
        """跑一轮上传，返回仍然失败的 [(条目, 错误信息), ...]（不计入统计）"""
        failed: List[Tuple[_LocalFile, str]] = []
        if not items:
            return failed
        executor = ThreadPoolExecutor(max_workers=workers)
        try:
            futures = {executor.submit(handle, it): it for it in items}
            for future in as_completed(futures):
                item = futures[future]
                try:
                    result = future.result()
                except Exception as e:
                    failed.append((item, str(e)))
                    emit(f"  {STATUS_ICONS[STATUS_FAIL]} 上传失败 {item.filename}: {e}")
                else:
                    count_result(item, result)
                finally:
                    if tick_done:
                        tick()
        except KeyboardInterrupt:
            executor.shutdown(wait=False, cancel_futures=True)
            raise
        finally:
            executor.shutdown(wait=True)
        return failed

    todo: List[_LocalFile] = []
    try:
        # -------- 阶段1+2：串行建目录 + 快照判重（判重不发请求） --------
        for dirpath, _, filenames in walk:
            rel = os.path.relpath(dirpath, local_path)
            rel_posix = '' if rel == '.' else rel.replace(os.sep, '/')
            remote_dir = normalize_remote_path(
                f"{tree.root_path}/{rel_posix}" if rel_posix else tree.root_path)

            try:
                parent_id = None if dry_run else tree.ensure(remote_dir)
            except Exception as e:
                stats.add(STATUS_FAIL, f"/{remote_dir}", f"创建目录失败: {e}")
                emit(f"创建远程目录 '{remote_dir or '/'}' 失败: {e}")
                for _ in filenames:
                    tick()
                continue

            # 本次运行新建的目录必然是空的，不必列目录判重
            fresh_dir = not dry_run and tree.was_created(remote_dir)

            for fname in filenames:
                local_file = os.path.join(dirpath, fname)
                remote_file = f"/{remote_dir}/{fname}" if remote_dir else f"/{fname}"

                if dry_run:
                    emit(f"[DRY-RUN] 会上传: {local_file} -> {remote_file}")
                    tick()
                    continue

                size = os.path.getsize(local_file)
                if not fresh_dir and index is not None and \
                        index.find_file(parent_id, fname, size) is not None:
                    stats.add_loose(remote_file)
                    if verbose:
                        emit(f"  {STATUS_ICONS[STATUS_SKIP]} 跳过已存在文件: {fname}")
                    tick()
                    continue

                todo.append(_LocalFile(local_file, remote_file, fname, parent_id, size))

        # -------- 阶段3：并发上传（限速器压住实际速率） --------
        failed = run(todo, max_workers)

        # -------- 阶段4：失败的串行重试一轮（仍失败就如实报出来） --------
        if failed:
            emit(f"  ↻ {len(failed)} 个文件失败，串行重试一轮...")
            still_failed = run([item for item, _ in failed], 1, tick_done=False)
            reason_by_file = {item.local_file: err for item, err in failed}
            for item, err in still_failed:
                stats.add(STATUS_FAIL, item.remote_file,
                          reason_by_file.get(item.local_file, err))
    finally:
        if bar:
            bar.close()

    return stats
