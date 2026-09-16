"""两行进度区 RunProgress 的渲染测试"""

from types import SimpleNamespace

from progress import CLEAR_LINE, UP_ONE, RunProgress, display_width, fit


class FakeStream:
    """可指定是否为终端的内存流"""

    def __init__(self, tty=False):
        self.chunks = []
        self.tty = tty

    def isatty(self):
        return self.tty

    def write(self, text):
        self.chunks.append(text)

    def flush(self):
        pass

    @property
    def text(self):
        return "".join(self.chunks)


def make_stats(hit=0, miss=0, fail=0, skip=0):
    return SimpleNamespace(hit=hit, miss=miss, fail=fail, skip=skip)


# ---------------------------------------------------------------- 宽度处理

def test_display_width_counts_cjk_as_two():
    assert display_width("abc") == 3
    assert display_width("中文") == 4
    assert display_width("中a") == 3


def test_fit_truncates_by_display_width():
    assert fit("abcdef", 4) == "abc…"
    assert fit("中文中文", 5) == "中文…"
    assert fit("短", 10) == "短"


# ---------------------------------------------------------------- 非终端

def test_non_tty_prints_one_line_per_directory_or_phase():
    stream = FakeStream(tty=False)
    progress = RunProgress(3, stream=stream)

    progress.set_phase("📂 准备目录")
    progress.dir_started("/图片")
    progress.dir_page("/图片", 2, 150)
    progress.dir_finished("/图片", 12, 3)
    progress.dir_started("/音乐")
    progress.close()

    text = stream.text
    assert "📂 读取目录快照 /图片" in text
    assert "📂 读取目录快照 /音乐" in text
    # 换阶段 1 次 + 换目录 2 次 = 3 行；同一目录内不重复打印，close 时状态
    # 没变也不再补一行
    assert text.count("\n") == 3


def test_non_tty_prints_final_state_on_close():
    stream = FakeStream(tty=False)
    progress = RunProgress(2, stream=stream)

    progress.set_phase("🔍 判重")
    progress.entry_judged('existing')
    progress.entry_judged('pending')
    progress.close()

    assert "判重 2/2" in stream.text
    assert "1/2 (50%)" in stream.text


def test_prep_phase_shows_directory_progress():
    """准备阶段（建目录树）看的是"目录就位了多少"，而不是判重数"""
    stream = FakeStream(tty=False)
    progress = RunProgress(71809, stream=stream)

    progress.set_phase("📂 准备目录")
    progress.set_total_dirs(3812)
    progress.dir_resolved()
    progress.dir_resolved()
    progress.close()

    assert "目录 2/3,812" in stream.text
    assert "判重" not in stream.text, "还没开始判重就不该显示判重数"


def test_judging_only_counts_resolved_entries():
    """判重阶段：已在云端/被丢弃的算进整体进度，待处理的留给秒传阶段"""
    stream = FakeStream(tty=False)
    progress = RunProgress(10, stream=stream)

    progress.begin_judging()
    for _ in range(4):
        progress.entry_judged('pending')
    progress.entry_judged('existing')
    progress.entry_judged('dropped')
    progress.close()

    assert "判重 6/10" in stream.text
    assert "2/10 (20%)" in stream.text


def test_upload_bar_continues_from_judging_without_reset():
    """秒传阶段的进度条接着判重阶段往下走，不归零重来"""
    stream = FakeStream(tty=False)
    progress = RunProgress(10, stream=stream)

    progress.begin_judging()
    progress.entry_judged('existing')
    progress.entry_judged('pending')
    progress.begin_upload(8)
    progress.entry_done()
    progress.close()

    assert "2/10 (20%)" in stream.text


def test_stats_counts_show_only_nonzero():
    stream = FakeStream(tty=False)
    progress = RunProgress(5, stream=stream)
    progress.attach_stats(make_stats(hit=3, skip=1))

    progress.set_phase("🔍 判重")
    progress.close()

    text = stream.text
    assert "✓ 3" in text and "⏭ 1" in text
    assert "⚠" not in text and "❌" not in text


def test_upload_rate_and_eta_shown(monkeypatch):
    clock = {"t": 0.0}
    monkeypatch.setattr("progress.time.monotonic", lambda: clock["t"])
    stream = FakeStream(tty=False)
    progress = RunProgress(10, stream=stream)

    progress.begin_upload(8)
    for i in range(1, 5):
        clock["t"] = float(i)
        progress.entry_done()
    progress.close()

    assert "4/10 (40%)" in stream.text
    assert "1.0/s" in stream.text
    assert "剩 00:06" in stream.text


def test_judging_rate_and_eta_shown_on_top_line(monkeypatch):
    """判重阶段进度条基本不动，速率/剩余时间显示在第一行"""
    clock = {"t": 0.0}
    monkeypatch.setattr("progress.time.monotonic", lambda: clock["t"])
    progress = RunProgress(10, stream=FakeStream())

    progress.begin_judging()          # t=0 播下种子样本
    for i in range(1, 6):
        clock["t"] = float(i)
        progress.entry_judged('pending')

    top = progress._top_line()
    assert "判重 5/10" in top
    assert "1.0/s" in top
    assert "剩 00:05" in top
    assert progress._rate_suffix() == "", "判重阶段第二行不带速率（口径是秒传的）"


def test_prep_dirs_rate_and_eta_shown_on_top_line(monkeypatch):
    clock = {"t": 0.0}
    monkeypatch.setattr("progress.time.monotonic", lambda: clock["t"])
    progress = RunProgress(500, stream=FakeStream())

    progress.set_phase("📂 准备目录")
    progress.set_total_dirs(10)
    for i in range(1, 4):
        clock["t"] = float(i)
        progress.dir_resolved()

    top = progress._top_line()
    assert "目录 3/10" in top
    assert "1.0/s" in top
    assert "剩 00:07" in top


def test_eta_hidden_when_rate_unknown():
    """样本跨度不足 1 秒（或推进不动）时不显示，不瞎猜"""
    progress = RunProgress(10, stream=FakeStream())

    progress.begin_judging()
    progress.entry_judged('pending')

    assert "剩" not in progress._top_line()


def test_disabled_is_silent_except_always_logs():
    stream = FakeStream(tty=False)
    progress = RunProgress(2, enabled=False, stream=stream)

    progress.set_phase("🔍 判重")
    progress.entry_judged('existing')
    progress.log("普通消息")
    progress.log("必须让人看到的消息", always=True)
    progress.close()

    assert stream.text == "必须让人看到的消息\n"


def test_close_is_idempotent():
    stream = FakeStream(tty=False)
    progress = RunProgress(1, stream=stream)

    progress.set_phase("🔍 判重")
    progress.close()
    progress.close()

    assert stream.text.count("\n") == 1


# ---------------------------------------------------------------- 终端

def test_tty_renders_two_lines_in_place():
    stream = FakeStream(tty=True)
    progress = RunProgress(10, stream=stream)

    progress.set_phase("🔍 判重")
    assert CLEAR_LINE in stream.text
    assert UP_ONE not in stream.text, "第一次渲染不需要上移"

    # 第一行变了（开始读目录）才回到上一行重画
    progress.dir_started("/a")
    assert UP_ONE in stream.text


def test_tty_top_unchanged_only_bottom_redraws():
    stream = FakeStream(tty=True)
    progress = RunProgress(4, stream=stream)
    progress.begin_upload(8)
    stream.chunks.clear()

    progress.entry_done()
    progress.entry_done()

    assert UP_ONE not in stream.text, "第一行没变时只重画第二行"
    assert "2/4" in stream.text
    assert "█" in stream.text


def test_tty_log_clears_progress_area_first():
    stream = FakeStream(tty=True)
    progress = RunProgress(1, stream=stream)
    progress.set_phase("🔍 判重")
    stream.chunks.clear()

    progress.log("⚠ 冲突", always=True)

    assert stream.text.startswith(f"\r{CLEAR_LINE}{UP_ONE}\r{CLEAR_LINE}")
    assert stream.text.endswith("⚠ 冲突\n")


def test_tty_close_ends_with_newline():
    stream = FakeStream(tty=True)
    progress = RunProgress(1, stream=stream)

    progress.set_phase("🔍 判重")
    progress.close()

    assert stream.text.endswith("\n")


def test_long_top_line_is_truncated_to_terminal_width(monkeypatch):
    monkeypatch.setattr("progress.shutil.get_terminal_size",
                        lambda fallback=None: __import__("os").terminal_size((60, 24)))
    stream = FakeStream(tty=True)
    progress = RunProgress(1, stream=stream)

    progress.dir_started("/很长的目录名" * 20)

    rendered = stream.text.split("\n")[0]
    assert "…" in rendered


# ---------------------------------------------------------------- 进度条布局

def test_bottom_line_keeps_bar_and_percentage_on_narrow_width():
    progress = RunProgress(10 ** 9, stream=FakeStream())
    progress.done = 123456

    line = progress._bottom_line(40)

    assert "[" in line and "]" in line
    assert "%" in line and "123,456" in line


def test_bottom_line_drops_counts_before_percentage():
    """终端太窄时先丢分类计数，百分比和进度数要保住"""
    progress = RunProgress(10 ** 9, stream=FakeStream())
    progress.done = 123456
    progress.attach_stats(make_stats(hit=123456, miss=12, fail=1, skip=45))

    line = progress._bottom_line(40)

    assert "✓" not in line
    assert "%" in line
