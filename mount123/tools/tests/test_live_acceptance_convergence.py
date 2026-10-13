"""Offline directory growth; EAGAIN simulations are not kernel FUSE evidence."""
import errno
import json
from pathlib import Path
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import test_live_acceptance as fixtures

a = fixtures.a


class DirectoryConvergence(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.directory = self.root / "archive"
        self.directory.mkdir()
        (self.directory / "one.txt").write_bytes(b"one")
        self.events = []
        self.http_status = 200
        self.on_listing = lambda: (self.directory / "two.txt").write_bytes(b"two")
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.events.append("web")
                owner.on_listing()
                body = json.dumps({"items": [{"name": "one.txt"}, {"name": "two.txt"}],
                                   "total": 2, "complete": True, "next_cursor": ""}).encode()
                content_type = "application/json"
                if self.path.startswith("/browse?"):
                    body = (b'<div id="fileListView">'
                            b'<div class="file-list-row file-item" data-file-id="1" data-favorite-name="one.txt"></div>'
                            b'<div class="file-list-row file-item" data-file-id="2" data-favorite-name="two.txt"></div></div>')
                    content_type = "text/html"
                self.send_response(owner.http_status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.args = SimpleNamespace(base_url=f"http://127.0.0.1:{self.server.server_port}",
            mount=str(self.root), max_seconds=20, max_bytes=16384, max_page_bytes=4096,
            max_pages=2, max_entries=10, max_operations=20, timeout=2, repeats=1,
            control_socket=None)
        self.case = {"name": "synthetic-growth", "kind": "archive", "order": "outer-first",
            "file_id": 1, "mount_path": "archive", "directories": [{"path": "", "count": 2}],
            "reads": []}

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def runner(self):
        runner = a.Runner(self.args)
        real_scan = runner.scan

        def scan(path):
            self.events.append("fuse")
            return real_scan(path)

        runner.scan = scan
        return runner

    def test_real_directory_growth_reopens_after_complete_http_response(self):
        # Both scans use subprocess os.scandir/stat on an ordinary directory.
        # The HTTP worker causes the server to add the second file before its
        # complete response; retaining the first snapshot cannot pass this test.
        runner = self.runner()
        result = runner.case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual(self.events, ["fuse", "web", "fuse"])
        step = result["steps"][0]
        self.assertEqual(step["first_fuse"]["status"], "partial")
        self.assertEqual(step["first_fuse"]["count"], 1)
        self.assertEqual(step["fuse_attempt_count"], 2)
        self.assertEqual([x["status"] for x in step["fuse_attempts"]], ["partial", "complete"])
        self.assertNotIn("one.txt", json.dumps(result))
        self.assertNotIn(str(self.root), json.dumps(result))

    def test_initial_eagain_preserves_first_trigger_then_reopens(self):
        # The subprocess-result seam simulates FUSE errno; this is not a kernel test.
        runner = self.runner()
        real_worker = a.run_worker
        scans = 0

        def worker(job, timeout):
            nonlocal scans
            if job["action"] == "scan":
                scans += 1
                if scans == 1:
                    raise a.Failure("os_error", "mount", errno=errno.EAGAIN)
            return real_worker(job, timeout)

        with patch.object(a, "run_worker", side_effect=worker):
            result = runner.case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual(self.events, ["fuse", "web", "fuse"])
        step = result["steps"][0]
        self.assertEqual(step["first_fuse"]["status"], "pending")
        self.assertEqual(step["first_fuse"]["error"]["errno"], errno.EAGAIN)
        self.assertEqual(step["fuse_attempt_count"], 2)
        self.assertEqual(runner.operations, 3)
        self.assertNotIn(str(self.root), json.dumps(result))

    def test_web_failure_keeps_observed_first_scan_without_claiming_complete(self):
        self.http_status = 503
        result = self.runner().case(self.case)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["error"]["code"], "http_status")
        self.assertEqual(self.events, ["fuse", "web"])
        first = result["steps"][0]["first_fuse"]
        self.assertEqual(first["status"], "observed")
        self.assertEqual(first["count"], 1)
        self.assertNotIn("one.txt", json.dumps(result))
        self.assertNotIn(str(self.root), json.dumps(result))

    def scan_results(self, outcomes):
        """Simulate only the process/filesystem boundary, keeping Runner budgets."""
        real_worker = a.run_worker
        outcomes = iter(outcomes)

        def worker(job, timeout):
            if job["action"] == "scan":
                outcome = next(outcomes, None)
                if isinstance(outcome, a.Failure):
                    raise outcome
                if outcome is not None:
                    return {"entries": [{"name": n, "dir": False} for n in outcome], "seconds": 0.01}
            return real_worker(job, timeout)

        return patch.object(a, "run_worker", side_effect=worker)

    def test_multiple_pending_and_partial_snapshots_converge_without_web_reload(self):
        runner = self.runner()
        pending = a.Failure("os_error", "mount", errno=errno.EAGAIN)
        with self.scan_results([pending, ["one.txt"], pending]):
            result = runner.case(self.case)
        self.assertEqual(result["status"], "passed", result)
        step = result["steps"][0]
        self.assertEqual([x["status"] for x in step["fuse_attempts"]],
                         ["pending", "partial", "pending", "complete"])
        self.assertEqual(step["fuse_attempt_count"], 4)
        self.assertEqual(self.events, ["fuse", "web", "fuse", "fuse", "fuse"])
        self.assertEqual(runner.operations, 5)
        self.assertGreater(runner.bytes, 0)
        self.assertAlmostEqual(step["fuse_wait_seconds"], 0.3)
        self.assertTrue(all(x["seconds"] >= 0 for x in step["fuse_attempts"]))

    def test_complete_first_snapshot_keeps_single_scan(self):
        (self.directory / "two.txt").write_bytes(b"two")
        result = self.runner().case(self.case)
        self.assertEqual(result["status"], "passed", result)
        self.assertEqual(self.events, ["fuse", "web"])
        self.assertEqual(result["steps"][0]["first_fuse"]["status"], "complete")
        self.assertEqual(result["steps"][0]["fuse_attempt_count"], 1)

    def test_extra_and_duplicate_names_cannot_be_healed_by_fresh_scan(self):
        for names, error, events in ((["one.txt", "unexpected.txt"], "directory_mismatch", ["fuse", "web"]),
                                     (["one.txt", "one.txt"], "duplicate_fuse_entries", ["fuse"])):
            with self.subTest(error=error):
                self.events.clear()
                with self.scan_results([names, ["one.txt", "two.txt"]]):
                    result = self.runner().case(self.case)
                self.assertEqual(result["error"]["code"], error)
                self.assertEqual(self.events, events)
                self.assertEqual(result["steps"][0]["fuse_attempt_count"], 1)
                self.assertNotIn("unexpected.txt", json.dumps(result))

    def test_nonretry_errors_and_worker_deadlines_keep_initial_evidence(self):
        errors = [a.Failure("os_error", "mount", errno=n) for n in (errno.EACCES, errno.ENOENT, errno.EIO)]
        errors += [a.Failure("operation_deadline", "timeout"), a.Failure("worker_unreaped", "environment", pid=123)]
        for error in errors:
            with self.subTest(code=error.code, facts=error.facts):
                self.events.clear()
                with self.scan_results([error]):
                    result = self.runner().case(self.case)
                self.assertEqual(result["status"], "failed")
                self.assertEqual(result["error"]["code"], error.code)
                self.assertEqual(self.events, ["fuse"])
                step = result["steps"][0]
                self.assertEqual(step["first_fuse"]["status"], "failed")
                self.assertEqual(step["fuse_attempt_count"], 1)
                self.assertEqual(step["first_fuse"]["error"]["code"], error.code)

    def test_retry_errors_do_not_erase_first_partial_evidence(self):
        with self.scan_results([["one.txt"], a.Failure("os_error", "mount", errno=errno.EIO)]):
            result = self.runner().case(self.case)
        self.assertEqual(result["error"]["errno"], errno.EIO)
        step = result["steps"][0]
        self.assertEqual(step["first_fuse"]["status"], "partial")
        self.assertEqual([x["status"] for x in step["fuse_attempts"]], ["partial", "failed"])
        self.assertEqual(self.events, ["fuse", "web", "fuse"])

    def test_repeated_partial_or_eagain_stops_after_eight_additional_scans(self):
        for outcome, code in ((["one.txt"], "directory_mismatch"),
                              (a.Failure("os_error", "mount", errno=errno.EAGAIN), "fuse_pending_budget")):
            with self.subTest(code=code):
                self.events.clear()
                runner = self.runner()
                with self.scan_results([outcome] * 10), patch.object(a.time, "sleep") as sleep:
                    result = runner.case(self.case)
                self.assertEqual(result["error"]["code"], code)
                self.assertEqual(result["steps"][0]["fuse_attempt_count"], 9)
                self.assertEqual(self.events.count("web"), 1)
                self.assertEqual(self.events.count("fuse"), 9)
                self.assertEqual(sleep.call_count, 8)
                self.assertEqual(runner.operations, 10)

    def test_global_operation_and_time_budgets_stop_before_next_wait(self):
        for budget in ("operations", "time"):
            with self.subTest(budget=budget):
                self.events.clear()
                runner = self.runner()
                if budget == "operations":
                    runner.args.max_operations = 3
                else:
                    self.on_listing = lambda: setattr(runner, "deadline", time.monotonic() - 1)
                with self.scan_results([["one.txt"]] * 10), patch.object(a.time, "sleep") as sleep:
                    result = runner.case(self.case)
                self.assertEqual(result["error"]["code"], "operation_budget" if budget == "operations" else "suite_deadline")
                self.assertEqual(result["steps"][0]["first_fuse"]["status"], "partial")
                self.assertEqual(self.events.count("web"), 1)
                self.assertEqual(self.events.count("fuse"), 2 if budget == "operations" else 1)
                self.assertEqual(sleep.call_count, 1 if budget == "operations" else 0)

    def test_browse_does_not_reconcile_partial_or_eagain(self):
        self.case.update(kind="browse", parent_id=1)
        for outcome, code in ((["one.txt"], "directory_mismatch"),
                              (a.Failure("os_error", "mount", errno=errno.EAGAIN), "os_error")):
            with self.subTest(code=code):
                self.events.clear()
                with self.scan_results([outcome]):
                    result = self.runner().case(self.case)
                self.assertEqual(result["error"]["code"], code)
                self.assertEqual(self.events.count("fuse"), 1)
                self.assertEqual(result["steps"][0]["fuse_attempt_count"], 1)


if __name__ == "__main__":
    unittest.main()
