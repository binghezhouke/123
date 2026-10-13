"""Offline acceptance fixtures: no cloud credentials, FUSE mount or service needed."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

MODULE = Path(__file__).resolve().parents[1] / "live_acceptance.py"
spec = importlib.util.spec_from_file_location("acceptance", MODULE)
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
DATA = b"\xff\xd8\xff" + bytes(range(256)) * 600


class StatsDelta(unittest.TestCase):
    @staticmethod
    def sample(values=None, groups=None, epoch="a" * 64, uptime=1):
        return {"available": True, "epoch": epoch,
                "values": {"uptime_seconds": uptime, **(values or {})}, "groups": groups or {}}

    def test_first_download_from_omitted_zero_is_in_case_delta(self):
        before = {"available": True, "epoch": "a" * 64,
                  "values": {"uptime_seconds": 1},
                  "groups": {"downloaded_bytes": "unknown"}}
        after = {"available": True, "epoch": "a" * 64,
                 "values": {"uptime_seconds": 2, "downloaded_bytes.events": 1,
                            "downloaded_bytes.bytes": 1048576},
                 "groups": {"downloaded_bytes": "measured"}}
        runner = a.Runner(SimpleNamespace(max_seconds=10))
        with patch.object(runner, "stats", side_effect=[before, after]):
            result = runner.case({"name": "synthetic", "order": "direct", "directories": []})
        self.assertEqual(result["stats_delta"].get("downloaded_bytes.events"), 1)
        self.assertEqual(result["stats_delta"].get("downloaded_bytes.bytes"), 1048576)
        self.assertEqual(result["stats_before"], before)
        self.assertEqual(result["stats_after"], after)

    def test_existing_counters_grow_and_gauges_are_snapshot_only(self):
        before = self.sample({"downloaded_bytes.events": 2, "downloaded_bytes.bytes": 10,
                              "cache.used_bytes": 50, "lookup.active_foreground": 2,
                              "foreground_read_latency.p95_nanos": 100, "future_counter": 2},
                             {"downloaded_bytes": "measured"})
        after = self.sample({"downloaded_bytes.events": 3, "downloaded_bytes.bytes": 30,
                             "cache.used_bytes": 1, "lookup.active_foreground": 1,
                             "foreground_read_latency.p95_nanos": 10, "future_counter": 99},
                            {"downloaded_bytes": "measured"}, uptime=2)
        delta, status = a.stats_delta(before, after)
        self.assertEqual(status, {"status": "measured"})
        self.assertEqual(delta, {"downloaded_bytes.events": 1, "downloaded_bytes.bytes": 20})
        self.assertEqual(before["values"]["cache.used_bytes"], 50)

    def test_only_defined_present_unobserved_group_is_zero(self):
        after = self.sample({"downloaded_bytes.events": 1, "downloaded_bytes.bytes": 10},
                            {"downloaded_bytes": "measured"}, uptime=2)
        for groups in ({}, {"downloaded_bytes": "measured"}, {"downloaded_bytes": "unsupported"}):
            with self.subTest(groups=groups):
                delta, status = a.stats_delta(self.sample(groups=groups), after)
                self.assertEqual(delta, {})
                self.assertEqual(status, {"status": "unavailable", "reason": "no_comparable_counters"})
        empty = self.sample(groups={"downloaded_bytes": "unknown"})
        self.assertEqual(a.stats_delta(empty, empty),
                         ({"downloaded_bytes.events": 0, "downloaded_bytes.bytes": 0}, {"status": "measured"}))

    def test_sampling_failure_missing_epoch_restart_or_time_reversal_is_not_comparable(self):
        before = self.sample({"downloaded_bytes.events": 1, "downloaded_bytes.bytes": 10},
                             {"downloaded_bytes": "measured"})
        for after, reason in (
            ({"available": False, "reason": "stats_response_invalid"}, "sample_unavailable"),
            (self.sample(epoch=None), "epoch_unknown"),
            (self.sample(epoch="b" * 64, uptime=999), "process_changed"),
            (self.sample(uptime=0), "sample_time_reversed"),
            (self.sample(uptime=None), "sample_time_unknown"),
        ):
            with self.subTest(reason=reason):
                delta, status = a.stats_delta(before, after)
                self.assertEqual(delta, {})
                self.assertEqual(status["reason"], reason)
                self.assertNotEqual(status["status"], "measured")
        delta, status = a.stats_delta({"available": False}, before)
        self.assertEqual(delta, {})
        self.assertEqual(status["reason"], "sample_unavailable")

    def test_counter_rollback_even_back_to_omitted_zero_invalidates_whole_window(self):
        before = self.sample({"downloaded_bytes.events": 2, "downloaded_bytes.bytes": 10},
                             {"downloaded_bytes": "measured"})
        for after in (self.sample({"downloaded_bytes.events": 3, "downloaded_bytes.bytes": 5},
                                 {"downloaded_bytes": "measured"}, uptime=2),
                      self.sample(groups={"downloaded_bytes": "unknown"}, uptime=2)):
            self.assertEqual(a.stats_delta(before, after),
                             ({}, {"status": "not_comparable", "reason": "counter_reset"}))

    def test_invalid_counter_is_not_a_zero_or_negative_delta(self):
        before = self.sample({"downloaded_bytes.events": 1, "downloaded_bytes.bytes": 10},
                             {"downloaded_bytes": "measured"})
        for value in (-1, 10.5, True):
            with self.subTest(value=value):
                after = self.sample({"downloaded_bytes.events": 2, "downloaded_bytes.bytes": value},
                                    {"downloaded_bytes": "measured"}, uptime=2)
                self.assertEqual(a.stats_delta(before, after),
                                 ({}, {"status": "not_comparable", "reason": "invalid_counter"}))

    def test_versioned_optional_counter_requires_version_on_both_samples(self):
        before = self.sample({"cache.fill_outcome_version": 1}, {"cache": "measured"})
        after = self.sample({"cache.fill_outcome_version": 1, "cache.fill_cancelled": 2},
                            {"cache": "measured"}, uptime=2)
        self.assertEqual(a.stats_delta(before, after)[0], {"cache.fill_cancelled": 2, "cache.fill_errors": 0})
        before["values"].pop("cache.fill_outcome_version")
        self.assertEqual(a.stats_delta(before, after)[0], {})

    def test_duration_cumulative_samples_and_time_not_percentiles(self):
        group = "stages.archive_index"
        before = self.sample({"stage_schema_version": 1}, {group: "unknown"})
        after = self.sample({"stage_schema_version": 1, group + ".samples": 1,
                             group + ".total_nanos": 30, group + ".p95_nanos": 63},
                            {group: "measured"}, uptime=2)
        self.assertEqual(a.stats_delta(before, after)[0], {group + ".samples": 1, group + ".total_nanos": 30})
        before["values"].pop("stage_schema_version")
        self.assertEqual(a.stats_delta(before, after)[0], {})

    def test_metadata_epoch_and_fixed_field_privacy_whitelist(self):
        raw = {"started_at": "2026-01-02T03:04:05.123456789Z", "uptime_seconds": 2,
               "downloaded_bytes": {"status": "unknown", "events": None, "bytes": None},
               "future_counter": 123, "sample_secret": {"bytes": 456},
               "cache": {"used_bytes": 12, "filename_secret": 789},
               "foreground_read_latency": {"p50_nanos": 20, "p95_nanos": 40},
               "configuration": {"port": 100, "password": "SECRET"}, "reason": "SECRET"}
        metadata = a.stats_metadata(raw)
        self.assertEqual(metadata, {"epoch": a.digest(raw["started_at"].encode()),
                                    "groups": {"downloaded_bytes": "unknown"}})
        self.assertEqual(a.numeric_stats(raw), {"uptime_seconds": 2, "cache.used_bytes": 12,
                                               "foreground_read_latency.p50_nanos": 20,
                                               "foreground_read_latency.p95_nanos": 40})
        raw["downloaded_bytes"]["bytes"] = True
        self.assertNotIn("downloaded_bytes", a.stats_metadata(raw)["groups"])
        for started in ("2026-01-02T11:04:05.123456789+08:00", "2026-01-01T22:04:05.123-05:00"):
            raw["started_at"] = started
            metadata = a.stats_metadata(raw)
            self.assertEqual(metadata["epoch"], a.digest(started.encode()))
            self.assertNotIn(started, json.dumps(metadata))
        for started in ("SECRET", "2026-99-01T00:00:00Z", "0001-01-01T00:00:00Z", None,
                        "2026-01-02T11:04:05+24:00", "2026-01-02T11:04:05-08:60",
                        "2026-01-02T11:04:05+08:00SECRET", "2026-01-02T11:04:05+8:00"):
            raw["started_at"] = started
            self.assertIsNone(a.stats_metadata(raw)["epoch"])

    def test_first_download_over_control_socket_worker_and_runner_case(self):
        started = "2026-01-02T11:04:05.123456789+08:00"
        snapshots = [
            {"started_at": started, "uptime_seconds": 1,
             "downloaded_bytes": {"status": "unknown", "events": None, "bytes": None}},
            {"started_at": started, "uptime_seconds": 2,
             "downloaded_bytes": {"status": "measured", "events": 1, "bytes": 1048576}},
        ]
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "control.sock")
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(path)
                server.listen(2)
                server.settimeout(3)
                commands, errors = [], []
                def respond():
                    try:
                        for snapshot in snapshots:
                            conn, _ = server.accept()
                            with conn:
                                commands.append(json.loads(conn.recv(1024)))
                                conn.sendall(json.dumps({"io_stats": snapshot}).encode() + b"\n")
                    except OSError as exc:
                        errors.append(type(exc).__name__)
                thread = threading.Thread(target=respond, daemon=True)
                thread.start()
                runner = a.Runner(SimpleNamespace(max_seconds=10, max_operations=4, timeout=2,
                                                  control_socket=path))
                result = runner.case({"name": "synthetic", "order": "direct", "directories": []})
                thread.join(timeout=4)
                self.assertFalse(thread.is_alive())
                self.assertEqual(errors, [])
                self.assertEqual(commands, [{"command": "io-stats"}] * 2)
        self.assertEqual(result["stats_delta_status"], {"status": "measured"})
        self.assertEqual(result["stats_delta"], {"downloaded_bytes.events": 1, "downloaded_bytes.bytes": 1048576})
        self.assertNotIn("downloaded_bytes.bytes", result["stats_before"]["values"])
        self.assertEqual(result["stats_after"]["values"]["downloaded_bytes.bytes"], 1048576)
        self.assertEqual(result["stats_before"]["epoch"], result["stats_after"]["epoch"])
        self.assertNotIn(started, json.dumps(result))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.server.seen.append(self.path)
        mode = self.server.mode
        parsed = urlsplit(self.path)
        q = parse_qs(parsed.query)
        if mode == "slow":
            time.sleep(0.3)
        if mode == "secret":
            self.send_response(403)
            self.end_headers()
            self.wfile.write(b"password=SECRET signed_url=https://secret.invalid/token")
            return
        if mode == "redirect":
            self.send_response(302)
            self.send_header("Location", "https://secret.invalid/?signature=SECRET")
            self.end_headers()
            return
        if parsed.path == "/browse":
            offset = int(q.get("offset", [0])[0])
            parent = q["parent_id"][0]
            name = ("inner", "extra")[min(offset, 1)]
            row = f'<div class="file-list-row file-item" data-file-id="{offset + 1}" data-favorite-name="{name}"></div>'
            more = f'<a data-load-more href="/browse?parent_id={parent}&amp;offset=1">next</a>' if offset == 0 else ""
            self.send_body(200, (f'<div id="fileListView">{row}</div>' + more).encode(), "text/html")
            return
        if parsed.path.endswith("/entries"):
            inner = q.get("path", [""])[0]
            names = ["inner", "extra"] if not inner else ["photo.jpg", "other.jpg", "last.jpg"]
            cursor = int(q.get("cursor", [0])[0])
            selected = names[cursor:cursor + 2]
            next_cursor = str(cursor + 2) if cursor + 2 < len(names) else ""
            if mode == "cycle":
                selected, next_cursor = [], "pending"
                if q.get("cursor"):
                    selected, next_cursor = [], "pending"
            if mode == "duplicate" and cursor:
                selected = [names[0]]
            value = {"items": [{"name": x} for x in selected], "next_cursor": next_cursor,
                     "total": len(names), "complete": mode != "incomplete"}
            if mode == "wrong_total":
                value["total"] += 1
            if mode == "html":
                self.send_body(200, b"<html>login SECRET</html>", "text/html")
            else:
                self.send_body(200, json.dumps(value).encode(), "application/json")
            return
        start, end = map(int, self.headers["Range"].split("=")[1].split("-"))
        body = DATA[start:end + 1]
        if mode == "short":
            body = body[:-1]
        if mode == "corrupt":
            body = b"X" + body[1:]
        self.send_response(200 if mode == "range200" else 206)
        self.send_header("Content-Type", "image/jpeg")
        self.send_header("Content-Range", "bytes 0-1/2" if mode == "wrong_range" else f"bytes {start}-{end}/{len(DATA)}")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass

    def send_body(self, status, body, kind):
        self.send_response(status)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass


class Fixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        (self.root / "outer" / "inner").mkdir(parents=True)
        (self.root / "outer" / "extra").mkdir()
        for name in ("photo.jpg", "other.jpg", "last.jpg"):
            (self.root / "outer" / "inner" / name).write_bytes(DATA)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.mode, self.server.seen = "good", []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.args = SimpleNamespace(base_url=f"http://127.0.0.1:{self.server.server_port}", mount=str(self.root),
            max_seconds=20, max_bytes=4 * 1024 * 1024, max_page_bytes=4096, max_pages=8,
            max_entries=10, max_operations=50, timeout=2, repeats=2, control_socket=None)
        self.case = {"name": "nested_split", "kind": "archive", "order": "outer-first", "file_id": 1,
            "mount_path": "outer", "directories": [{"path": "", "archive_depth": 0, "count": 2},
                {"path": "inner", "archive_depth": 1, "count": 3}],
            "reads": [{"path": "inner/photo.jpg", "archive_depth": 1, "size": len(DATA), "magic_hex": "ffd8ff"}]}

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.temp.cleanup()

    def test_nested_pagination_and_repeated_head_tail(self):
        runner = a.Runner(self.args)
        result = runner.case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual([p["count"] for p in result["steps"][1]["pages"]], [2, 1])
        self.assertEqual(len(result["reads"][0]["ranges"]), 4)
        report = json.dumps(result)
        self.assertNotIn("photo.jpg", report)
        self.assertNotIn(str(self.root), report)
        self.assertEqual(result["reads"][0]["ranges"][0]["signature_hex"][:6], "ffd8ff")

    def test_fuse_happens_before_web_at_each_step(self):
        runner, events = a.Runner(self.args), []
        real_scan, real_listing = runner.scan, runner.listing
        def scan(path):
            events.append(("fuse", path))
            return real_scan(path)
        def listing(case, step):
            events.append(("web", step["path"]))
            return real_listing(case, step)
        runner.scan, runner.listing = scan, listing
        self.case["reads"] = []
        self.assertEqual(runner.case(self.case)["status"], "passed")
        self.assertEqual(events, [("fuse", "outer"), ("web", ""), ("fuse", "outer/inner"), ("web", "inner")])

    def test_browse_html_paging(self):
        self.case.update(kind="browse", parent_id=2, directories=[self.case["directories"][0]], reads=[])
        result = a.Runner(self.args).case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual(len(result["steps"][0]["pages"]), 2)

    def test_browse_root_zero_id_and_empty_mount_path(self):
        self.case.update(kind="browse", parent_id=0, mount_path="",
                         directories=[self.case["directories"][0]], reads=[])
        a.validate_manifest({"version": 1, "cases": [self.case]}, 2)
        self.args.mount = str(self.root / "outer")
        result = a.Runner(self.args).case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual(result["steps"][0]["count"], 2)
        self.assertEqual(len(result["steps"][0]["pages"]), 2)
        self.assertTrue(all("parent_id=0" in path for path in self.server.seen))

    def test_lan_http_base_url_is_accepted_without_live_request(self):
        manifest, report = self.root / "samples.json", self.root / "report.json"
        manifest.write_text(json.dumps({"version": 1, "cases": [self.case]}))
        argv = ["--live", "--manifest", str(manifest), "--mount", str(self.root),
                "--base-url", "http://192.168.2.254:8082", "--report", str(report)]
        with patch.object(a.Runner, "case", return_value={"name": self.case["name"], "status": "passed"}) as case:
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(a.main(argv), 0)
            case.assert_called_once()
        self.assertEqual(self.server.seen, [])

    def test_bad_listing_never_passes_200(self):
        for mode, code in (("html", "listing_not_json"), ("wrong_total", "total_mismatch"),
                           ("incomplete", "incomplete_listing"), ("duplicate", "duplicate_entries")):
            with self.subTest(mode=mode):
                self.server.mode = mode
                result = a.Runner(self.args).case(self.case)
                self.assertEqual(result["status"], "failed")
                self.assertEqual(result["error"]["code"], code)

    def test_range_status_headers_short_and_corruption(self):
        for mode, code in (("range200", "range_not_partial"), ("wrong_range", "content_range_mismatch"),
                           ("short", "short_range"), ("corrupt", "range_bytes_mismatch")):
            with self.subTest(mode=mode):
                self.server.mode = mode
                result = a.Runner(self.args).case(self.case)
                self.assertEqual(result["status"], "failed")
                self.assertEqual(result["error"]["code"], code)

    def test_directory_mismatch(self):
        (self.root / "outer" / "inner" / "last.jpg").unlink()
        result = a.Runner(self.args).case(self.case)
        self.assertEqual(result["error"]["code"], "directory_mismatch")
        self.assertEqual(result["error"]["missing_count"], 1)

    def test_secret_and_redirect_not_reported_or_followed(self):
        for mode in ("secret", "redirect"):
            self.server.mode = mode
            result = a.Runner(self.args).case(self.case)
            self.assertEqual(result["status"], "failed")
            self.assertNotIn("SECRET", json.dumps(result))
            self.assertNotIn("secret.invalid", json.dumps(result))

    def test_page_entry_byte_and_time_budgets(self):
        for key, value, code in (("max_pages", 1, "page_budget"), ("max_entries", 1, "entry_budget"),
                                 ("max_bytes", 1, "suite_byte_budget"), ("max_seconds", -1, "suite_deadline")):
            with self.subTest(key=key):
                args = SimpleNamespace(**vars(self.args))
                setattr(args, key, value)
                result = a.Runner(args).case(self.case)
                self.assertEqual(result["error"]["code"], code)

    def test_resume_skips_passed_and_binds_manifest(self):
        manifest, report = self.root / "samples.json", self.root / "report.json"
        manifest.write_text(json.dumps({"version": 1, "cases": [self.case]}))
        argv = ["--live", "--manifest", str(manifest), "--mount", str(self.root),
                "--base-url", self.args.base_url, "--report", str(report)]
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(a.main(argv), 0)
            count = len(self.server.seen)
            self.assertEqual(a.main(argv + ["--resume"]), 0)
            self.assertEqual(len(self.server.seen), count)
            manifest.write_text(manifest.read_text() + " ")
            self.assertEqual(a.main(argv + ["--resume"]), 2)
        self.assertEqual(report.stat().st_mode & 0o777, 0o600)

    def test_deadline_reaps_process(self):
        self.server.mode = "slow"
        captured = []
        real_popen = subprocess.Popen
        def popen(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            captured.append(process)
            return process
        with patch.object(a.subprocess, "Popen", side_effect=popen):
            with self.assertRaises(a.Failure) as error:
                a.run_worker({"action": "http", "url": self.args.base_url + "/archive/1/entries", "limit": 4096, "timeout": 2}, 0.1)
        self.assertEqual(error.exception.code, "operation_deadline")
        self.assertIsNotNone(captured[0].poll())

    def test_control_socket_only_requests_stats_and_redacts_strings(self):
        path = str(self.root / "control.sock")
        server = socket.socket(socket.AF_UNIX)
        server.bind(path)
        server.listen(1)
        received = []
        def respond():
            conn, _ = server.accept()
            with conn:
                received.append(json.loads(conn.recv(1024)))
                conn.sendall(json.dumps({"io_stats": {"cache": {"foreground": {"status": "measured", "hit_bytes": 12}},
                    "configuration": {"password": "SECRET"}, "reason": "SECRET"}}).encode() + b"\n")
        thread = threading.Thread(target=respond, daemon=True)
        thread.start()
        try:
            self.args.control_socket = path
            result = a.Runner(self.args).stats()
            self.assertEqual(result, {"available": True, "values": {"cache.foreground.hit_bytes": 12},
                                      "epoch": None, "groups": {"cache.foreground": "measured"}})
            self.assertEqual(received, [{"command": "io-stats"}])
        finally:
            server.close()
            thread.join(timeout=2)

    def test_direct_does_not_scan_outer(self):
        self.case["order"] = "direct"
        self.case["directories"] = self.case["directories"][1:]
        runner = a.Runner(self.args)
        with patch.object(runner, "scan", wraps=runner.scan) as scan:
            self.assertEqual(runner.case(self.case)["status"], "passed")
            self.assertEqual([x.args[0] for x in scan.call_args_list], ["outer/inner"])

    def test_head_only_never_reads_tail(self):
        self.args.range_edge, self.args.repeats = ["head"], 1
        result = a.Runner(self.args).case(self.case)
        self.assertEqual(result["status"], "passed")
        self.assertEqual([x["edge"] for x in result["reads"][0]["ranges"]], ["head"])

    def test_unreaped_stats_worker_is_reported_without_more_operations(self):
        runner = a.Runner(self.args)
        with patch.object(runner, "stats", side_effect=a.Failure("worker_unreaped", "environment", pid=123)) as stats:
            with patch.object(runner, "scan") as scan:
                result = runner.case(self.case)
                self.assertEqual(result["error"]["code"], "worker_unreaped")
                self.assertEqual(result["error"]["pid"], 123)
                self.assertEqual(stats.call_count, 1)
                scan.assert_not_called()


class Unit(unittest.TestCase):
    def test_browse_next_marker_requires_nonempty_href(self):
        for link in ('<a data-load-more>next</a>', '<a data-load-more href="">next</a>',
                     '<a data-load-more href="   ">next</a>'):
            with self.subTest(link=link):
                parser = a.BrowseParser()
                with self.assertRaises(a.Failure) as failure:
                    parser.feed('<div id="fileListView"></div>' + link)
                self.assertEqual(failure.exception.code, "invalid_cursor")
                self.assertEqual(failure.exception.category, "protocol")

    def test_browse_without_next_marker_completes(self):
        args = SimpleNamespace(max_seconds=10, max_pages=4, max_entries=20, max_page_bytes=4096)
        runner = a.Runner(args)
        body = (b'<div id="fileListView"><div class="file-list-row file-item" '
                b'data-file-id="1" data-favorite-name="last"></div></div>')
        runner.http = lambda *x: {"status": 200, "content_type": "text/html", "data": a.base64.b64encode(body), "seconds": 0}
        names, pages = runner.listing({"kind": "browse", "parent_id": 1}, {"path": ""})
        self.assertEqual(names, ["last"])
        self.assertEqual(len(pages), 1)
        self.assertTrue(pages[0]["complete"])

    def test_browse_error_page_is_failure_even_with_valid_marker(self):
        parser = a.BrowseParser()
        with self.assertRaises(a.Failure) as failure:
            parser.feed('<div id="fileListView"></div><p class="error" role="alert">SECRET</p>')
        self.assertEqual(failure.exception.code, "browse_error_page")

    def test_offline_default_needs_nothing(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(a.main([]), 0)
        self.assertIn("skipped", output.getvalue())

    def test_only_loopback_bypasses_proxy(self):
        for host in ("127.0.0.1", "::1", "localhost"):
            self.assertTrue(a.local_host(host))
        for host in ("example.com", "localhost.evil", "192.168.1.1"):
            self.assertFalse(a.local_host(host))

    def test_stats_keep_numbers_drop_configuration_and_strings(self):
        value = {"cache_hit_bytes": {"bytes": 42, "status": "SECRET"},
                 "configuration": {"password": "SECRET", "port": 80}, "path": "SECRET"}
        self.assertEqual(a.numeric_stats(value), {"cache_hit_bytes.bytes": 42})

    def test_paths_and_depth_rejected_before_io(self):
        for path in ("../secret", "/absolute", "x/../secret", "x//y"):
            with self.assertRaises(a.Failure):
                a.relative(path)
        case = {"name": "safe", "kind": "archive", "order": "direct", "file_id": 1,
                "mount_path": "outer", "directories": [{"path": "inner", "archive_depth": 3}]}
        with self.assertRaises(a.Failure) as failure:
            a.validate_manifest({"version": 1, "cases": [case]}, 2)
        self.assertEqual(failure.exception.code, "depth_budget")

    def test_archive_zero_id_still_rejected(self):
        case = {"name": "safe", "kind": "archive", "order": "direct", "file_id": 0,
                "mount_path": "outer", "directories": [{"path": "", "archive_depth": 0}]}
        with self.assertRaises(a.Failure) as failure:
            a.validate_manifest({"version": 1, "cases": [case]}, 2)
        self.assertEqual(failure.exception.code, "invalid_id")

    def test_cursor_cycle(self):
        args = SimpleNamespace(max_seconds=10, max_pages=4, max_entries=20, max_page_bytes=4096)
        runner = a.Runner(args)
        body = json.dumps({"items": [], "total": 0, "complete": False, "next_cursor": "same"}).encode()
        runner.http = lambda *x: {"status": 200, "content_type": "application/json", "data": a.base64.b64encode(body), "seconds": 0}
        with self.assertRaises(a.Failure) as failure:
            runner.listing({"kind": "archive", "file_id": 1}, {"path": ""})
        self.assertEqual(failure.exception.code, "cursor_cycle")


if __name__ == "__main__":
    unittest.main()
