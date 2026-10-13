"""Pending archive listings through the real CLI and isolated HTTP workers."""
import json
import importlib.util
from email.utils import formatdate
from pathlib import Path
import subprocess
import signal
import sys
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

SCRIPT = Path(__file__).resolve().parents[1] / "live_acceptance.py"
spec = importlib.util.spec_from_file_location("pending_acceptance", SCRIPT)
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)


class PendingHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        cursor = parse_qs(urlsplit(self.path).query).get("cursor", [""])[0]
        self.server.cursors.append(cursor)
        response = self.server.responses.pop(0)
        status, body, headers = response
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", headers.get("Content-Type", "application/json"))
        self.send_header("Content-Length", str(len(data)))
        for key, value in headers.items():
            if key != "Content-Type":
                self.send_header(key, value)
        self.end_headers()
        self.wfile.write(data)
        self.server.responded.set()


def page(names, cursor="", complete=True):
    return {"items": [{"name": name} for name in names], "next_cursor": cursor,
            "total": 2, "complete": complete}


def pending(cursor=""):
    return {"code": "archive_index_pending", "retryable": True, "items": [],
            "next_cursor": cursor, "total": 0, "complete": False,
            "diagnostic": "SECRET https://secret.invalid/private?signature=SECRET"}


class PendingCLI(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "outer").mkdir()
        for name in ("first", "second"):
            (self.root / "outer" / name).write_bytes(b"synthetic")
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), PendingHandler)
        self.server.responses, self.server.cursors = [], []
        self.server.responded = threading.Event()
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)
        self.manifest = self.root / "manifest.json"
        self.report = self.root / "report.json"
        self.manifest.write_text(json.dumps({"version": 1, "cases": [{
            "name": "synthetic_pending", "kind": "archive", "order": "outer-first",
            "file_id": 1, "mount_path": "outer", "directories": [
                {"path": "", "archive_depth": 0, "count": 2}]}]}))

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def command(self, *extra):
        return [sys.executable, str(SCRIPT), "--live", "--manifest", str(self.manifest),
                   "--mount", str(self.root), "--base-url",
                   f"http://127.0.0.1:{self.server.server_port}", "--report", str(self.report),
                   "--max-page-bytes", "2048", "--max-seconds", "10", "--timeout", "2",
                   *extra]

    def run_cli(self, *extra):
        result = subprocess.run(self.command(*extra), capture_output=True, text=True, timeout=15)
        report = json.loads(self.report.read_text())
        for value in (result.stdout, result.stderr, json.dumps(report)):
            self.assertNotIn("SECRET", value)
            self.assertNotIn("secret.invalid", value)
            self.assertNotIn(str(self.root), value)
        return result, report

    def test_pending_between_pages_preserves_rows_and_cursor(self):
        self.server.responses = [
            (200, page(["first"], "synthetic-cursor"), {}),
            (202, pending("synthetic-cursor"), {"Retry-After": "0"}),
            (202, pending("synthetic-cursor"), {"Retry-After": "0"}),
            (200, page(["second"]), {}),
        ]
        result, report = self.run_cli("--max-pages", "2")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(report["status"], "passed")
        self.assertEqual(self.server.cursors, ["", "synthetic-cursor", "synthetic-cursor", "synthetic-cursor"])
        self.assertEqual([row["count"] for row in report["cases"][0]["steps"][0]["pages"]], [1, 1])
        self.assertEqual(report["cases"][0]["steps"][0]["pages"][1]["pending_responses"], 2)
        self.assertEqual(report["attempt"]["operations"], 5)  # FUSE then four HTTP workers.

    def test_empty_progressive_page_waits_without_consuming_page_or_rows(self):
        self.server.responses = [
            (200, page(["first"], "synthetic-cursor"), {}),
            (200, page([], "synthetic-cursor", False), {"Retry-After": "0"}),
            (200, page([], "synthetic-scan-cursor", False), {"Retry-After": "0"}),
            (202, pending("synthetic-scan-cursor"), {"Retry-After": "0"}),
            (200, page(["second"]), {}),
        ]
        result, report = self.run_cli("--max-pages", "2")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(self.server.cursors, ["", "synthetic-cursor", "synthetic-cursor",
                                             "synthetic-scan-cursor", "synthetic-scan-cursor"])
        self.assertEqual([row["count"] for row in report["cases"][0]["steps"][0]["pages"]], [1, 1])
        self.assertEqual(report["cases"][0]["steps"][0]["pages"][1]["pending_responses"], 3)

    def test_progressive_and_pending_responses_share_the_directory_retry_cap(self):
        self.server.responses = [(202, pending(), {"Retry-After": "0"})] * 4 + [
            (200, page([], "synthetic-scan-cursor", False), {"Retry-After": "0"})] * 4 + [
            (202, pending("synthetic-scan-cursor"), {"Retry-After": "0"})]
        result, report = self.run_cli("--max-pages", "1")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["cases"][0]["error"]["code"], "pending_retry_budget")
        self.assertEqual(len(self.server.cursors), 9)
        self.assertEqual(report["attempt"]["operations"], 10)

    def test_nonempty_progressive_pages_keep_cursor_cycle_protection(self):
        for middle in ([], [(200, page([], "synthetic-scan-cursor", False), {"Retry-After": "0"})]):
            with self.subTest(empty_scan=bool(middle)):
                self.server.responses = [(200, page(["first"], "synthetic-cursor"), {})] + middle + [
                    (200, page(["second"], "synthetic-scan-cursor" if middle else "synthetic-cursor"), {})]
                self.server.cursors = []
                result, report = self.run_cli()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(report["cases"][0]["error"]["code"], "cursor_cycle")
                self.assertEqual(len(self.server.cursors), 3 if middle else 2)

    def test_progressive_metadata_must_be_valid_before_wait(self):
        for field, value, code in (("total", -1, "invalid_pagination"), ("total", True, "invalid_pagination"),
                ("total", 1.5, "invalid_pagination"), ("total", None, "invalid_pagination"),
                ("next_cursor", None, "invalid_cursor"), ("next_cursor", 1, "invalid_cursor")):
            with self.subTest(field=field, value=value):
                self.server.responses = [(200, {**page([], "", False), field: value}, {"Retry-After": "0"})]
                self.server.cursors = []
                result, report = self.run_cli()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(report["cases"][0]["error"]["code"], code)
                self.assertEqual(self.server.cursors, [""])

    def test_empty_progressive_initial_cursor_and_deadline(self):
        self.server.responses = [(200, page([], "", False), {"Retry-After": "0"}),
                                 (200, page(["first", "second"]), {})]
        result, report = self.run_cli("--max-pages", "1")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(self.server.cursors, ["", ""])
        self.server.responses = [(200, page([], "", False), {"Retry-After": "10"})]
        self.server.cursors = []
        result, report = self.run_cli("--max-seconds", "0.5")
        self.assertEqual(report["cases"][0]["error"]["code"], "suite_deadline")
        self.assertEqual(self.server.cursors, [""])

    def test_pending_retry_cap_is_per_directory_not_per_page(self):
        self.server.responses = [(202, pending(), {"Retry-After": "0"})] * 4 + [
            (200, page(["first"], "synthetic-cursor"), {})] + [
            (202, pending("synthetic-cursor"), {"Retry-After": "0"})] * 5
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["cases"][0]["error"], {"code": "pending_retry_budget", "category": "budget"})
        self.assertEqual(len(self.server.cursors), 10)
        self.assertEqual(report["attempt"]["operations"], 11)

    def test_eighth_pending_retry_can_complete(self):
        self.server.responses = [(202, pending(), {"Retry-After": "0"})] * 8 + [(200, page(["first", "second"]), {})]
        result, report = self.run_cli("--max-pages", "1")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(len(self.server.cursors), 9)
        self.assertEqual(report["cases"][0]["steps"][0]["pages"][0]["pending_responses"], 8)

    def test_real_retry_after_wait_is_counted_without_exposing_header(self):
        self.server.responses = [(202, pending(), {"Retry-After": "1"}), (200, page(["first", "second"]), {})]
        started = time.monotonic()
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 0, report)
        self.assertGreaterEqual(time.monotonic() - started, 1)
        self.assertEqual(report["cases"][0]["steps"][0]["pages"][0]["pending_wait_seconds"], 1)
        self.assertNotIn("Retry-After", json.dumps(report))

    def test_legal_delay_uses_global_deadline_without_an_arbitrary_cap(self):
        self.server.responses = [(202, pending(), {"Retry-After": "10"}), (200, page(["first", "second"]), {})]
        runner = a.Runner(SimpleNamespace(base_url=f"http://127.0.0.1:{self.server.server_port}",
            mount=str(self.root), max_seconds=30, max_operations=10, timeout=2,
            max_bytes=16384, max_page_bytes=2048, max_pages=1, max_entries=10, control_socket=None))
        # Keep actual FUSE/HTTP workers; replace only the ten-second sleep.
        with patch.object(a.time, "sleep") as sleep:
            result = runner.case({"name": "synthetic_pending", "kind": "archive", "order": "outer-first",
                "file_id": 1, "mount_path": "outer", "directories": [{"path": "", "count": 2}]})
        self.assertEqual(result["status"], "passed", result)
        sleep.assert_called_once_with(10)
        self.assertEqual(result["steps"][0]["pages"][0]["pending_wait_seconds"], 10)

    def test_retry_after_over_deadline_and_nonfinite_never_wait_or_retry(self):
        for header, code, category in (("10", "suite_deadline", "budget"),
                ("9" * 400, "pending_wait_budget", "budget"),
                ("Infinity", "invalid_retry_after", "protocol"),
                ("-1", "invalid_retry_after", "protocol"),
                ("SECRET", "invalid_retry_after", "protocol"),
                (formatdate(time.time() + 60, usegmt=True), "suite_deadline", "budget")):
            with self.subTest(header_kind=code):
                self.server.responses = [(202, pending(), {"Retry-After": header})]
                self.server.cursors = []
                result, report = self.run_cli("--max-seconds", "0.5")
                self.assertEqual(result.returncode, 1, report)
                self.assertEqual(report["cases"][0]["error"], {"code": code, "category": category})
                self.assertEqual(self.server.cursors, [""])

    def test_missing_retry_after_respects_deadline_and_past_httpdate_can_retry(self):
        self.server.responses = [(202, pending(), {})]
        result, report = self.run_cli("--max-seconds", "0.5")
        self.assertEqual(report["cases"][0]["error"]["code"], "suite_deadline")
        self.server.responses = [(202, pending(), {"Retry-After": "Thu, 01 Jan 1970 00:00:00 GMT"}),
                                 (200, page(["first", "second"]), {})]
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(report["cases"][0]["steps"][0]["pages"][0]["pending_wait_seconds"], 0)

    def test_operation_and_response_budgets_are_checked_before_wait(self):
        for args, code in ((["--max-operations", "2"], "operation_budget"),
                           (["--max-bytes", "2049"], "suite_byte_budget")):
            with self.subTest(code=code):
                self.server.responses = [(202, pending(), {"Retry-After": "1"})]
                self.server.cursors = []
                result, report = self.run_cli(*args)
                self.assertEqual(report["cases"][0]["error"]["code"], code)
                self.assertEqual(self.server.cursors, [""])
                self.assertEqual(report["attempt"]["operations"], 2)
        self.server.responses = [(202, pending(), {})]
        result, report = self.run_cli("--max-page-bytes", "64")
        self.assertEqual(report["cases"][0]["error"]["code"], "response_byte_budget")

    def test_malformed_pending_and_real_http_errors_fail_immediately(self):
        for mutation in ({"code": "unexpected"}, {"retryable": False}, {"retryable": 1},
                         {"items": [{"name": "SECRET"}]}, {"next_cursor": "different"},
                         {"complete": True}, {"total": 1}):
            with self.subTest(mutation=list(mutation)):
                self.server.responses = [(202, {**pending(), **mutation}, {"Retry-After": "0"})]
                self.server.cursors = []
                result, report = self.run_cli()
                self.assertEqual(report["cases"][0]["error"]["code"], "invalid_pending_response")
                self.assertEqual(self.server.cursors, [""])
        for body, headers in ((b"SECRET invalid-json", {}), (pending(), {"Content-Type": "text/html"})):
            self.server.responses = [(202, body, headers)]
            result, report = self.run_cli()
            self.assertEqual(report["cases"][0]["error"]["code"], "invalid_pending_response")
        for status, category in ((400, "web"), (403, "authentication")):
            self.server.responses = [(status, {"secret": "SECRET"}, {})]
            self.server.cursors = []
            result, report = self.run_cli()
            self.assertEqual(report["cases"][0]["error"], {"code": "http_status", "category": category, "status": status})
            self.assertEqual(self.server.cursors, [""])

    def test_keyboard_interrupt_during_pending_wait_stops_requests(self):
        self.server.responses = [(202, pending(), {"Retry-After": "5"})]
        proc = subprocess.Popen(self.command(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self.assertTrue(self.server.responded.wait(3))
            time.sleep(0.2)  # The real HTTP worker returns before the parent wait.
            proc.send_signal(signal.SIGINT)
            out, err = proc.communicate(timeout=3)
            self.assertNotEqual(proc.returncode, 0)
            self.assertEqual(self.server.cursors, [""])
            self.assertNotIn("SECRET", out + err)
            self.assertNotIn("secret.invalid", out + err)
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.communicate()

    def test_nonfinite_global_budgets_are_rejected_before_workers(self):
        for key in ("--max-seconds", "--timeout"):
            proc = subprocess.run(self.command(key, "inf"), capture_output=True, text=True, timeout=3)
            self.assertEqual(proc.returncode, 2)
            self.assertEqual(json.loads(proc.stdout), {"status": "failed", "code": "invalid_budget", "category": "configuration"})
        self.assertEqual(self.server.cursors, [])


if __name__ == "__main__":
    unittest.main()
