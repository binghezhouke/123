"""Public CLI/local HTTP tests for explicit bounded range order (no FUSE)."""
import hashlib
import json
import socket
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SCRIPT = Path(__file__).resolve().parents[1] / "live_acceptance.py"
SIZE = 3 * 65536 + 17
BODY = b"\xff\xd8\xff" + bytes(i % 251 for i in range(SIZE - 3))


class RangeCLI(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "outer").mkdir()
        self.member = self.root / "outer" / "synthetic.bin"
        self.member.write_bytes(BODY)
        self.ranges = []
        self.bad_range_at = None
        self.mutate_after = None
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                if self.path.split("?", 1)[0].endswith("/entries"):
                    body = json.dumps({"items": [{"name": "synthetic.bin"}], "total": 1,
                                       "complete": True, "next_cursor": ""}).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                else:
                    value = self.headers["Range"]
                    owner.ranges.append(value)
                    start, end = map(int, value.removeprefix("bytes=").split("-"))
                    body = owner.member.read_bytes()[start:end + 1]
                    self.send_response(206)
                    cr = f"bytes {start}-{end}/{owner.sample['size']}"
                    if len(owner.ranges) == owner.bad_range_at:
                        cr = "bytes 0-0/1"
                    self.send_header("Content-Range", cr)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                if len(owner.ranges) == owner.mutate_after:
                    changed = bytearray(owner.member.read_bytes())
                    # Change every requested location after both paths agreed
                    # on the first response, so repeat checking is independent
                    # of FUSE/Web byte equality.
                    for value in owner.ranges:
                        start = int(value.split("=")[1].split("-")[0])
                        changed[start] ^= 1
                    owner.member.write_bytes(changed)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)
        self.manifest, self.report = self.root / "manifest.json", self.root / "report.json"
        self.sample = {"path": "synthetic.bin", "size": SIZE, "archive_depth": 0, "magic_hex": "ffd8ff"}
        self.case = {"name": "synthetic-offsets", "kind": "archive", "order": "outer-first", "file_id": 1,
                     "mount_path": "outer", "directories": [{"path": "", "archive_depth": 0, "count": 1}],
                     "reads": [self.sample]}
        self.cases = [self.case]

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def run_cli(self, *extra, timeout=15, preserve_report=False):
        self.manifest.write_text(json.dumps({"version": 1, "cases": self.cases}))
        if not preserve_report:
            self.report.unlink(missing_ok=True)
        result = subprocess.run([sys.executable, str(SCRIPT), "--live", "--manifest", str(self.manifest),
            "--mount", str(self.root), "--base-url", f"http://127.0.0.1:{self.server.server_port}",
            "--report", str(self.report), "--max-page-bytes", "4096", "--max-seconds", "10", "--timeout", "2", "--repeats", "1",
            *extra], capture_output=True, text=True, timeout=timeout)
        report = json.loads(self.report.read_text()) if self.report.exists() else None
        for value in (result.stdout, result.stderr, json.dumps(report)):
            self.assertNotIn(str(self.root), value)
            self.assertNotIn("synthetic.bin", value)
        return result, report

    def test_default_failure_retains_completed_head_evidence(self):
        self.bad_range_at = 2
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 1)
        case = report["cases"][0]
        self.assertEqual(case["error"]["code"], "content_range_mismatch")
        self.assertEqual(len(case["reads"]), 1)
        ranges = case["reads"][0]["ranges"]
        self.assertEqual([r["edge"] for r in ranges], ["head"])
        self.assertEqual(ranges[0]["sha256"], hashlib.sha256(BODY[:65536]).hexdigest())

    def test_explicit_offsets_follow_order_and_shorten_near_eof(self):
        offsets = [131072, 65536, SIZE - 7, 65536, 0]
        self.sample["offsets"] = offsets
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 0, report)
        expected = [f"bytes={off}-{off + min(65536, SIZE-off)-1}" for off in offsets]
        self.assertEqual(self.ranges, expected)
        ranges = report["cases"][0]["reads"][0]["ranges"]
        self.assertEqual([r["offset"] for r in ranges], offsets)
        self.assertEqual([r["sequence"] for r in ranges], [1, 2, 3, 4, 5])
        self.assertEqual([r["bytes"] for r in ranges], [65536, 65536, 7, 65536, 65536])
        for row in ranges:
            off, length = row["offset"], row["bytes"]
            self.assertEqual(row["sha256"], hashlib.sha256(BODY[off:off+length]).hexdigest())
            self.assertNotIn("signature_hex", row)
            self.assertNotIn("edge", row)
        self.assertEqual(self.report.stat().st_mode & 0o777, 0o600)

    def test_repeated_offsets_and_repeats_detect_changed_matching_bytes(self):
        for offsets, repeats in (([131072, 65536, 131072], "1"), ([65536], "2")):
            with self.subTest(repeats=repeats):
                self.ranges.clear()
                self.member.write_bytes(BODY)
                self.sample["offsets"] = offsets
                self.mutate_after = 1
                result, report = self.run_cli("--repeats", repeats)
                self.assertEqual(result.returncode, 1, report)
                case = report["cases"][0]
                self.assertEqual(case["error"]["code"], "repeat_bytes_changed")
                self.assertEqual(case["phase"], "read")
                self.assertEqual(case["status"], "failed")
                ranges = case["reads"][0]["ranges"]
                self.assertEqual(len(ranges), 2 if repeats == "1" else 1)
                self.assertEqual(ranges[0]["sha256"], hashlib.sha256(BODY[offsets[0]:offsets[0]+65536]).hexdigest())
                self.assertEqual(len(self.ranges), 3 if repeats == "1" else 2)

    def test_range_protocol_failure_keeps_prior_ranges_and_stops(self):
        self.sample["offsets"] = [131072, 65536, 0]
        self.bad_range_at = 2
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 1)
        case = report["cases"][0]
        self.assertEqual(case["error"]["code"], "content_range_mismatch")
        self.assertEqual([row["offset"] for row in case["reads"][0]["ranges"]], [131072])
        self.assertEqual(len(self.ranges), 2)

    def test_offset_zero_still_requires_magic(self):
        self.sample["offsets"] = [65536, 0]
        self.sample["magic_hex"] = "0000"
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 1)
        case = report["cases"][0]
        self.assertEqual(case["error"]["code"], "signature_mismatch")
        self.assertEqual([row["offset"] for row in case["reads"][0]["ranges"]], [65536])

    def test_invalid_offsets_and_edge_conflicts_fail_before_workers(self):
        invalid = ([], None, True, "0", [True], [False], [-1], [SIZE], [1.5], ["1"], [0] * 33)
        for offsets in invalid:
            with self.subTest(offsets=offsets):
                self.sample["offsets"] = offsets
                result, report = self.run_cli()
                self.assertEqual(result.returncode, 2)
                self.assertEqual(json.loads(result.stdout)["code"], "invalid_offsets")
                self.assertIsNone(report)
                self.assertEqual(self.ranges, [])
        self.sample["offsets"] = [0]
        for edge in ("head", "tail"):
            with self.subTest(edge=edge):
                self.sample[edge + "_sha256"] = hashlib.sha256(BODY[:65536]).hexdigest()
                result, report = self.run_cli()
                self.assertEqual(result.returncode, 2)
                self.assertEqual(json.loads(result.stdout)["code"], "offsets_edge_hash_conflict")
                self.assertIsNone(report)
                self.sample.pop(edge + "_sha256")
                result, report = self.run_cli("--range-edge", edge)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(json.loads(result.stdout)["code"], "offsets_range_edge_conflict")
                self.assertIsNone(report)
        self.assertEqual(self.ranges, [])

    def test_thirty_two_offsets_are_bounded_and_one_byte_near_eof(self):
        self.sample["offsets"] = [SIZE - 1] * 32
        result, report = self.run_cli("--max-seconds", "30", timeout=35)
        self.assertEqual(result.returncode, 0, report)
        ranges = report["cases"][0]["reads"][0]["ranges"]
        self.assertEqual(len(ranges), 32)
        self.assertEqual([row["sequence"] for row in ranges], list(range(1, 33)))
        self.assertTrue(all(row["bytes"] == 1 for row in ranges))
        self.assertEqual(self.ranges, [f"bytes={SIZE-1}-{SIZE-1}"] * 32)
        self.assertEqual(report["attempt"]["operations"], 66)

    def test_shared_operation_and_byte_budgets_keep_completed_ranges(self):
        self.sample["offsets"] = [65536, 131072]
        for args, code in ((("--max-operations", "4"), "operation_budget"),
                           (("--max-bytes", "200704"), "suite_byte_budget")):
            with self.subTest(code=code):
                self.ranges.clear()
                result, report = self.run_cli(*args)
                self.assertEqual(result.returncode, 1)
                case = report["cases"][0]
                self.assertEqual(case["error"]["code"], code)
                self.assertEqual([row["offset"] for row in case["reads"][0]["ranges"]], [65536])
                self.assertEqual(len(self.ranges), 1)
                self.assertEqual(report["attempt"]["operations"], 4 if code == "operation_budget" else 5)

    def test_statistics_budget_failure_does_not_replace_protocol_error(self):
        self.sample["offsets"] = [65536]
        self.bad_range_at = 1
        control = self.root / "control.sock"
        listener = socket.socket(socket.AF_UNIX)
        listener.bind(str(control))
        listener.listen(1)
        listener.settimeout(3)
        received = []
        errors = []

        def stats():
            try:
                connection, _ = listener.accept()
                with connection:
                    received.append(json.loads(connection.recv(4096)))
                    connection.sendall(b'{"io_stats":{}}\n')
            except OSError as exc:
                errors.append(type(exc).__name__)

        thread = threading.Thread(target=stats, daemon=True)
        thread.start()
        try:
            result, report = self.run_cli("--control-socket", str(control), "--max-operations", "5")
        finally:
            listener.close()
            thread.join(timeout=4)
        self.assertFalse(thread.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(received, [{"command": "io-stats"}])
        self.assertEqual(result.returncode, 1)
        case = report["cases"][0]
        self.assertEqual(case["error"]["code"], "content_range_mismatch")
        self.assertEqual(case["stats_after"], {"available": False, "reason": "operation_budget"})
        self.assertEqual(case["reads"][0]["ranges"], [])

    def test_default_and_selected_head_ranges_remain_compatible(self):
        head = hashlib.sha256(BODY[:65536]).hexdigest()
        tail = hashlib.sha256(BODY[-65536:]).hexdigest()
        self.sample.update(head_sha256=head, tail_sha256=tail)
        result, report = self.run_cli("--repeats", "2")
        self.assertEqual(result.returncode, 0, report)
        ranges = report["cases"][0]["reads"][0]["ranges"]
        self.assertEqual([row["edge"] for row in ranges], ["head", "tail", "head", "tail"])
        self.assertEqual([row["sha256"] for row in ranges], [head, tail, head, tail])
        self.ranges.clear()
        result, report = self.run_cli("--range-edge", "head")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual([row["edge"] for row in report["cases"][0]["reads"][0]["ranges"]], ["head"])
        self.assertEqual(self.ranges, ["bytes=0-65535"])

    def test_small_default_tail_keeps_old_magic_semantics(self):
        self.sample.update(size=17, magic_hex="0000")
        self.member.write_bytes(BODY[:17])
        result, report = self.run_cli("--range-edge", "tail")
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(self.ranges, ["bytes=0-16"])
        self.assertEqual(report["cases"][0]["reads"][0]["ranges"][0]["edge"], "tail")
        self.ranges.clear()
        self.sample["offsets"] = [0]
        result, report = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["cases"][0]["error"]["code"], "signature_mismatch")

    def legacy_identity(self):
        # Public identity contract before explicit-offset support, using a known
        # CLI parameter snapshot. No historical Git object or script is needed.
        self.manifest.write_text(json.dumps({"version": 1, "cases": self.cases}))
        args = {"live": True, "manifest": str(self.manifest), "mount": str(self.root),
                "base_url": f"http://127.0.0.1:{self.server.server_port}", "cache": None,
                "control_socket": None, "timeout": 2.0, "max_seconds": 10.0,
                "max_bytes": 16777216, "max_page_bytes": 4096, "max_pages": 32,
                "max_entries": 2000, "max_operations": 100, "max_depth": 2,
                "repeats": 1, "range_edge": [], "resume": False,
                "report": str(self.report), "case": []}
        bound = {k: v for k, v in args.items() if k not in ("resume", "report", "case")}
        return hashlib.sha256(self.manifest.read_bytes() + json.dumps(bound, sort_keys=True).encode()).hexdigest()

    def test_resume_rejects_legacy_pass_that_ignored_explicit_offsets(self):
        self.sample["offsets"] = [65536, SIZE - 7]
        legacy = {"version": 1, "identity": self.legacy_identity(), "status": "passed",
                  "cache_configured": False, "cases": [{"name": self.case["name"], "status": "passed",
                  "order": "outer-first", "steps": [], "reads": [{"index": 0, "ranges": [
                  {"edge": "head", "repeat": 1, "bytes": 65536, "sha256": hashlib.sha256(BODY[:65536]).hexdigest()},
                  {"edge": "tail", "repeat": 1, "bytes": 65536, "sha256": hashlib.sha256(BODY[-65536:]).hexdigest()}]}]}]}
        self.report.write_text(json.dumps(legacy))
        self.report.chmod(0o600)
        result, report = self.run_cli("--resume", preserve_report=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stdout)["code"], "resume_identity_mismatch")
        self.assertEqual(self.ranges, [])
        self.assertEqual(report, legacy)

    def test_default_report_keeps_legacy_resume_identity(self):
        legacy_identity = self.legacy_identity()
        first, first_report = self.run_cli()
        self.assertEqual(first.returncode, 0, first_report)
        self.assertEqual(first_report["identity"], legacy_identity)
        self.ranges.clear()
        result, report = self.run_cli("--resume", preserve_report=True)
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(report["identity"], first_report["identity"])
        self.assertEqual(self.ranges, [])

    def test_offsets_identity_is_stable_across_selected_case_changes(self):
        self.sample["offsets"] = [65536]
        plain_sample = {k: v for k, v in self.sample.items() if k != "offsets"}
        plain_case = {**self.case, "name": "default-control", "reads": [plain_sample]}
        self.cases = [plain_case, self.case]
        result, report = self.run_cli("--case", "default-control")
        self.assertEqual(result.returncode, 0, report)
        identity = report["identity"]
        self.ranges.clear()
        result, report = self.run_cli("--resume", "--case", self.case["name"], preserve_report=True)
        self.assertEqual(result.returncode, 0, report)
        self.assertEqual(report["identity"], identity)
        self.assertEqual(self.ranges, ["bytes=65536-131071"])
        self.assertEqual(report["cases"][-1]["reads"][0]["ranges"][0]["offset"], 65536)


if __name__ == "__main__":
    unittest.main()
