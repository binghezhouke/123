"""批处理进度显示（两行 HUD）。

一份清单要经历"准备目录 → 判重 → 并发秒传 → 失败重试"几个阶段。以前判重是
两行纯数字、秒传阶段又换成一个从 0 起算的 tqdm 条，阶段一换进度就像被重置了。
现在整个生命周期共用一块两行进度区：

    🔍 判重 1,203/5,000 · 12.3/s · 剩 05:12 · 📂 读取目录快照 /图片（第 2 页）
    ▶ 1,180/5,000 [██████░░░░░░░░░░] 24% · ✓ 1,157 ⏭ 23

要点：第二行的分母始终是清单总条数，分子只认"终态"——判重阶段解决一条 +1、
秒传完成一条 +1，所以进度条跨阶段单调递增，不会归零重来；✓/⚠/❌/⏭ 的分类
计数直接读 UploadStats，与最终汇总同口径。按阶段推进速率估的"剩 X"在判重/
建目录阶段显示在第一行（那个阶段进度条基本不动），秒传/重试阶段显示在第二行
（和进度条同一个口径）。

终端下原地刷新；输出重定向到文件/管道时退化成"换目录/换阶段时打印一行、
长时间无变化按 5 秒补一次"。
"""

import shutil
import sys
import time
import unicodedata

CLEAR_LINE = "\x1b[2K"
UP_ONE = "\x1b[1A"
# 非终端环境下两次输出之间的最小间隔，避免写日志时刷屏
NON_TTY_INTERVAL = 5.0
# 进度条宽度上下限：低于下限就先丢弃速率/剩余时间、再丢弃分类计数
BAR_MIN_WIDTH = 8
BAR_MAX_WIDTH = 40
# 速率统计的时间窗口（秒），窗口内不足 1 秒不显示速率
RATE_WINDOW = 12.0


def display_width(text: str) -> int:
    """按终端显示宽度计算（中日韩字符占两列）"""
    return sum(2 if unicodedata.east_asian_width(ch) in 'WF' else 1 for ch in text)


def fit(text: str, width: int) -> str:
    """把文本裁剪到指定显示宽度，超出部分用省略号"""
    if width <= 0 or display_width(text) <= width:
        return text
    out, used = "", 0
    for ch in text:
        char_width = 2 if unicodedata.east_asian_width(ch) in 'WF' else 1
        if used + char_width > width - 1:
            break
        out += ch
        used += char_width
    return out + "…"


class RunProgress:
    """两行进度区：上面是阶段与当前动作，下面是跨阶段的整体进度条。"""

    def __init__(self, total: int, enabled: bool = True, stream=None):
        self.stream = stream if stream is not None else sys.stderr
        self.total = total
        # 不是终端就没法原地刷新，走"换目录/换阶段时打印一次"的退化路径
        self.tty = bool(getattr(self.stream, 'isatty', lambda: False)())
        self.enabled = bool(enabled)
        # 分类计数（✓/⚠/❌/⏭）从这里读，和最终汇总同一个口径
        self.stats = None

        self.done = 0        # 已到终态的条目数：判重解决的 + 秒传完成的
        self.judged = 0      # 判重阶段已看过的条目数（只上第一行）
        self.dirs_total = 0  # 准备阶段的目标目录总数
        self.dirs_done = 0

        self.phase = ""      # 如 "🔍 判重"
        self.note = ""       # 阶段旁边的计数，如 "1,203/5,000"
        self.activity = ""   # 当前动作，如 "📂 读取目录快照 /图片（第 2 页）"

        # 速率采样：记录当前阶段进度指标（'dirs'/'judged'/'done'）的
        # (monotonic, 值) 序列，用来估"按现在这个速度还要多久"
        self._rate_metric = None
        self._samples = []
        self._rendered = False   # 终端上进度区是否还画着
        self._last_top = None    # 上次画出来的第一行（没变就只重画第二行）
        self._dir_key = ""       # 非终端打印的触发键之一：当前目录
        self._printed_key = None
        self._printed_line = ""  # 非终端上次打印的内容（close 时避免重复行）
        self._last_print = 0.0
        self._closed = False

    # ------------------------------------------------------------ 阶段切换

    def set_phase(self, phase: str, note: str = "") -> None:
        """切换阶段（准备目录 / 判重 / 秒传 / 重试），note 是阶段旁的计数"""
        self.phase = phase
        self.note = note
        # 上一阶段的动作已经结束，别让它以残影的形式带进新阶段；
        # 旧阶段的速率也不再有意义，等新阶段重新采样
        self.activity = ""
        self._rate_metric = None
        self._samples = []
        self._refresh()

    def set_total_dirs(self, total: int) -> None:
        """准备阶段的目标目录总数（用来显示阶段进度）"""
        self.dirs_total = total
        if total:
            self._sample_rate('dirs', self.dirs_done)
        self._update_dir_note()

    def dir_resolved(self) -> None:
        """一个目标目录已经就位（找到或新建），准备阶段往前走一格"""
        self.dirs_done += 1
        self._sample_rate('dirs', self.dirs_done)
        self._update_dir_note()

    def _update_dir_note(self) -> None:
        if self.dirs_total:
            self.note = f"目录 {self.dirs_done}/{self.dirs_total:,}"
        self._refresh()

    def begin_judging(self) -> None:
        """进入逐条判重（第一行换成判重口径）"""
        self.judged = 0
        self.set_phase("🔍 判重")
        self._sample_rate('judged', self.judged)

    def entry_judged(self, kind: str) -> None:
        """
        记一条清单记录的判重结果。

        :param kind: 'existing' 云端已有 / 'pending' 需要处理 / 'dropped' 记录有问题被丢弃
        """
        self.judged += 1
        if kind != 'pending':
            # 只有判重阶段就解决的条目才算进整体进度，待处理的留给秒传阶段
            self.done += 1
        self.note = f"{self.judged:,}/{self.total:,}"
        self._sample_rate('judged', self.judged)
        self._refresh()

    def begin_upload(self, workers: int) -> None:
        """进入并发秒传，开始统计速率"""
        self.set_phase("⚡ 秒传", f"{workers} 并发")
        self._sample_rate('done', self.done)

    def begin_retry(self, count: int) -> None:
        """进入失败重试（进度条延续，不重开）"""
        self.set_phase("↻ 重试", f"{count} 项")
        self._sample_rate('done', self.done)

    def entry_done(self) -> None:
        """一个条目到达终态（秒传/重试完成一条），整体进度 +1"""
        self.done += 1
        self._sample_rate('done', self.done)
        self._refresh()

    def entry_settled(self) -> None:
        """判重时还挂着的记录在后面阶段到了终态（如目录建失败），整体进度 +1"""
        self.done += 1
        self._refresh()

    def attach_stats(self, stats) -> None:
        """挂上 UploadStats，第二行直接显示 ✓/⚠/❌/⏭ 分类计数"""
        self.stats = stats

    # ------------------------------------------------------------ 目录快照

    def dir_started(self, label: str) -> None:
        self._dir_key = label
        self.activity = f"📂 读取目录快照 {label}"
        self._refresh()

    def dir_page(self, label: str, page_no: int, total_items: int) -> None:
        """翻了一页：更新当前动作，让读大目录时也能看出在动"""
        self.activity = f"📂 读取目录快照 {label}（第 {page_no} 页 · 共 {total_items:,} 条）"
        self._refresh()

    def dir_finished(self, label: str, files: int, subdirs: int) -> None:
        self.activity = f"快照 {label} 就绪（{files + subdirs:,} 条）"
        self._refresh()

    # ------------------------------------------------------------ 输出

    def log(self, message: str, always: bool = False) -> None:
        """
        在进度区上方打印一条普通消息（不影响两行进度）。

        :param always: 失败项/冲突这类必须让人看到的消息，进度区关掉时也照打
        """
        if not (always or self.enabled):
            return
        if self.tty and self._rendered:
            self.stream.write("\r" + CLEAR_LINE + f"{UP_ONE}\r" + CLEAR_LINE)
        self.stream.write(f"{message}\n")
        self._rendered = False
        self.stream.flush()

    def close(self) -> None:
        """结束进度区：先补一次最终状态，再让后续输出从新的一行开始"""
        if self._closed:
            return
        # 注意顺序：_closed 置位后 _refresh 就不再输出，所以最终状态要先补；
        # 和上一行完全相同就不重复打印
        if self.enabled and not self.tty and self._snapshot_line() != self._printed_line:
            self._refresh(force=True)
        if self.enabled and self.tty and self._rendered:
            self.stream.write("\n")
            self.stream.flush()
        self._rendered = False
        self._closed = True

    # ------------------------------------------------------------ 内部渲染

    def _percent(self) -> int:
        if not self.total:
            return 100
        return min(100, self.done * 100 // self.total)

    def _top_line(self) -> str:
        head = f"{self.phase} {self.note}".strip()
        parts = [part for part in (head, self._phase_eta_suffix()) if part]
        if self.activity:
            parts.append(self.activity)
        return " · ".join(parts)

    def _counts_suffix(self) -> str:
        """✓/⚠/❌/⏭ 分类计数（只列非零项，读的是 UploadStats）"""
        if self.stats is None:
            return ""
        parts = []
        for attr, icon in (('hit', '✓'), ('miss', '⚠'), ('fail', '❌'), ('skip', '⏭')):
            count = getattr(self.stats, attr, 0)
            if count:
                parts.append(f"{icon} {count:,}")
        return " ".join(parts)

    def _sample_rate(self, metric: str, value: int) -> None:
        """记一次阶段进度采样；指标换了（阶段切换）就重新开始统计"""
        if self._rate_metric != metric:
            self._rate_metric = metric
            self._samples = []
        self._samples.append((time.monotonic(), value))

    def _rate(self):
        """近 RATE_WINDOW 秒内的推进速率；样本跨度不足 1 秒时返回 None"""
        now = time.monotonic()
        self._samples = [(t, v) for t, v in self._samples if now - t <= RATE_WINDOW]
        if len(self._samples) < 2:
            return None
        t0, v0 = self._samples[0]
        t1, v1 = self._samples[-1]
        if t1 - t0 < 1.0:
            return None
        return (v1 - v0) / (t1 - t0)

    def _eta_parts(self):
        """(速率, 剩余秒数)：按当前阶段指标估算；估不出来返回 None"""
        if self._rate_metric is None:
            return None
        rate = self._rate()
        if rate is None or rate <= 0:
            return None
        remaining = {
            'dirs': self.dirs_total - self.dirs_done,
            'judged': self.total - self.judged,
            'done': self.total - self.done,
        }[self._rate_metric]
        if remaining <= 0:
            return None
        return rate, remaining / rate

    def _phase_eta_suffix(self) -> str:
        """第一行尾部的阶段速率/剩余时间（判重、建目录阶段用这里）"""
        if self._rate_metric not in ('dirs', 'judged'):
            return ""
        eta = self._eta_parts()
        if eta is None:
            return ""
        rate, seconds = eta
        return f"{rate:.1f}/s · 剩 {self._fmt_duration(seconds)}"

    def _rate_suffix(self) -> str:
        """第二行尾部的速率/剩余时间（秒传/重试阶段，和进度条同一个口径）"""
        if self._rate_metric != 'done':
            return ""
        eta = self._eta_parts()
        if eta is None:
            return ""
        rate, seconds = eta
        return f"{rate:.1f}/s · 剩 {self._fmt_duration(seconds)}"

    @staticmethod
    def _fmt_duration(seconds: float) -> str:
        seconds = max(0, int(seconds))
        if seconds >= 3600:
            return f"{seconds // 3600}:{seconds % 3600 // 60:02d}:{seconds % 60:02d}"
        return f"{seconds // 60:02d}:{seconds % 60:02d}"

    def _bottom_line(self, width: int) -> str:
        left = f"▶ {self.done:,}/{self.total:,}"
        groups = [g for g in (f"{self._percent()}%",
                              self._counts_suffix(),
                              self._rate_suffix()) if g]
        while groups:
            suffix = " · ".join(groups)
            # 宽度预算："▶ " + " [" + "] " + 后缀
            bar_width = width - display_width(left) - display_width(suffix) - 5
            if bar_width >= BAR_MIN_WIDTH:
                break
            groups.pop()   # 终端太窄：先丢速率/剩余时间，再丢分类计数
        else:
            suffix = ""
            bar_width = BAR_MIN_WIDTH
        bar_width = min(bar_width, BAR_MAX_WIDTH)
        filled = bar_width * self.done // self.total if self.total else bar_width
        bar = '█' * filled + '░' * (bar_width - filled)
        line = f"{left} [{bar}]"
        if suffix:
            line += f" {suffix}"
        return fit(line, width)

    def _snapshot_line(self) -> str:
        """非终端下的一行快照（没有进度条，只留关键数字）"""
        parts = []
        top = self._top_line()
        if top:
            parts.append(f"[{top}]")
        parts.append(f"{self.done:,}/{self.total:,} ({self._percent()}%)")
        for extra in (self._counts_suffix(), self._rate_suffix()):
            if extra:
                parts.append(extra)
        return " · ".join(parts)

    def _refresh(self, force: bool = False) -> None:
        if not self.enabled or self._closed:
            return

        if not self.tty:
            # 非终端：换目录/换阶段时打一次，长时间没变化按间隔补一次
            now = time.monotonic()
            key = (self.phase, self._dir_key)
            if not (force
                    or key != self._printed_key
                    or now - self._last_print >= NON_TTY_INTERVAL):
                return
            line = self._snapshot_line()
            self.stream.write(line + "\n")
            self.stream.flush()
            self._printed_key = key
            self._printed_line = line
            self._last_print = now
            return

        width = max(40, shutil.get_terminal_size(fallback=(100, 24)).columns - 1)
        bottom = self._bottom_line(width)

        if self._rendered and self._top_line() == self._last_top:
            # 第一行没变：只重画下一行，上一行保持原样
            self.stream.write(f"\r{CLEAR_LINE}{bottom}")
        else:
            top = fit(self._top_line(), width)
            if self._rendered:
                self.stream.write(UP_ONE)
            self.stream.write(f"\r{CLEAR_LINE}{top}\n\r{CLEAR_LINE}{bottom}")
            self._last_top = self._top_line()
        self.stream.flush()
        self._rendered = True
