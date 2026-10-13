#!/usr/bin/env python3
"""Bounded, opt-in web/FUSE acceptance. Standard library only; see live_acceptance.md."""
import argparse
import base64
from datetime import datetime
import errno
import hashlib
from html.parser import HTMLParser
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

VERSION = 1
READ_SIZE = 65536

# Fixed exporter schema: iostats/{stats,cache,recovery}.go. Inventory, active
# counts, limits and latency percentiles remain snapshots, never counter deltas.
BYTE_COUNTER_GROUPS = (
    "downloaded_bytes", "foreground_read_bytes", "cache_hit_bytes",
    "read_ahead_scheduled_bytes", "read_ahead_completed_bytes",
    "read_ahead_consumed_bytes", "read_ahead_wasted_bytes")
DURATION_GROUPS = (
    "foreground_read_latency", "foreground_read_success_time", "transfer_queue_latency",
    "http_body_ttfb", "http_transfer_latency", "cache_publication_latency",
    "read_ahead_foreground_wait") + tuple("stages." + name for name in (
        "directory_lookup", "source_prepare", "url_resolve", "source_probe",
        "build_queue", "archive_index", "decompression", "file_open")) + tuple(
            "api_profile." + name + ".latency" for name in (
                "list", "file_info", "download_url", "access_token", "other"))
COUNTER_FIELDS = {group: ("events", "bytes") for group in BYTE_COUNTER_GROUPS}
COUNTER_FIELDS.update({group: ("samples", "total_nanos") for group in DURATION_GROUPS})
COUNTER_FIELDS.update({
    "cache": ("capacity_evictions", "capacity_eviction_bytes", "enospc", "fill_successes",
              "fill_failures", "fill_cancelled", "fill_errors", "existing_fill_waits",
              "disk_pressure_events", "disk_pressure_rejects", "disk_pressure_evictions", "orphan_files_cleaned"),
    "cache.foreground": ("read_requests", "full_hits", "requested_bytes", "hit_bytes", "miss_bytes"),
    "cache.background": ("read_requests", "full_hits", "requested_bytes", "hit_bytes", "miss_bytes"),
    "cache.refaults": ("count", "bytes"),
    "directory_cache": ("memory_hits", "disk_restores", "disk_restore_attempts", "list_calls", "list_errors",
                        "stale_served", "stale_failures", "retry_backoffs", "permanent_failures",
                        "corrupt_snapshots", "persistence_failures"),
    "remote_recovery": ("attempts", "retries", "recovered", "exhausted", "cancelled", "suffix_bytes"),
    "download_scheduler": ("completed_requests",), "index_queue": ("requests",),
    "image_prefetch": ("planned", "started", "reused", "completed", "cancelled", "failed", "foreground_ready"),
})
COUNTER_FIELDS.update({group: ("requests", "foreground_requests", "background_requests")
                       for group in ("readdir", "lookup", "getattr", "open")})
COUNTER_FIELDS.update({"api_profile." + group: ("requests", "successes", "errors", "retries", "throttle_waits")
                       for group in ("list", "file_info", "download_url", "access_token", "other")})
SNAPSHOT_FIELDS = {
    "": ("uptime_seconds", "stage_schema_version"),
    "cache": ("capacity_bytes", "used_bytes", "reserved_bytes", "entries", "pinned_bytes",
              "index_budget_bytes", "index_used_bytes", "fill_outcome_version", "disk_free_bytes", "disk_minimum_free_bytes"),
    "download_scheduler": ("maximum_requests", "maximum_in_flight_bytes", "active_requests", "active_bytes",
                           "available_background_bytes", "waiting_foreground", "waiting_background", "staging_active_bytes",
                           "staging_peak_bytes", "staging_waiting_foreground", "staging_waiting_background"),
    "index_queue": ("limit", "active", "background_active", "waiting"),
    "image_prefetch": ("foreground_in_flight",),
    "scan": ("metadata_requests_per_second", "enter_requests_per_second", "exit_requests_per_second"),
    "directory_lookup": ("events", "bytes"),  # This group currently has no hook; never infer zero.
}
SNAPSHOT_FIELDS.update({group: ("p50_nanos", "p95_nanos", "max_nanos") for group in DURATION_GROUPS})
SNAPSHOT_FIELDS.update({group: ("active_foreground", "active_background")
                        for group in ("readdir", "lookup", "getattr", "open")})
NUMERIC_STAT_FIELDS = frozenset((group + "." if group else "") + field
                              for schema in (COUNTER_FIELDS, SNAPSHOT_FIELDS)
                              for group, fields in schema.items() for field in fields)


def stats_path(value, path):
    for key in path.split("."):
        if not isinstance(value, dict):
            return None
        value = value.get(key)
    return value


def stats_metadata(value):
    groups = {}
    for group in COUNTER_FIELDS:
        status = stats_path(value, ("cache" if group == "cache.refaults" else group) + ".status")
        if (status == "unknown" and group in BYTE_COUNTER_GROUPS + DURATION_GROUPS and
                any(stats_path(value, group + "." + field) is not None for field in COUNTER_FIELDS[group])):
            continue  # Malformed values are not the exporter's null-before-first-event shape.
        if status in ("measured", "idle", "unknown"):
            groups[group] = status
    epoch = None
    started = value.get("started_at") if isinstance(value, dict) else None
    stamp = (re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-](\d\d):(\d\d))", started)
             if isinstance(started, str) else None)
    if stamp and (stamp[1] is None or (int(stamp[1]) < 24 and int(stamp[2]) < 60)):
        try:
            if datetime.fromisoformat(started.replace("Z", "+00:00")).year > 1:
                # Keep nanosecond precision. One exporter epoch uses the same
                # RFC3339 spelling in every snapshot, including its UTC offset.
                epoch = digest(started.encode())
        except ValueError:
            pass
    return {"epoch": epoch, "groups": groups}


def counter_value(sample, group, field):
    values, statuses = sample["values"], sample.get("groups", {})
    if group.startswith("stages.") and values.get("stage_schema_version") != 1:
        return None
    if field in ("fill_cancelled", "fill_errors") and group == "cache" and values.get("cache.fill_outcome_version") != 1:
        return None
    path = group + "." + field
    value = values.get(path)
    if value is not None:
        if type(value) is not int or value < 0:
            raise ValueError("invalid counter")
        return value if statuses.get(group) in ("measured", "idle") else None
    # These concrete tracker factories return null fields before their first
    # observation. An absent group, measured-but-missing field, or unknown hook
    # is not a zero. Version 1 explicitly omits zero fill outcomes too.
    if statuses.get(group) == "unknown" and group in BYTE_COUNTER_GROUPS + DURATION_GROUPS:
        return 0
    if group == "cache" and field in ("fill_cancelled", "fill_errors") and statuses.get(group) == "measured":
        return 0
    return None


def stats_delta(before, after):
    if not before.get("available") or not after.get("available"):
        return {}, {"status": "unavailable", "reason": "sample_unavailable"}
    epochs = [sample.get("epoch") for sample in (before, after)]
    if not all(isinstance(epoch, str) and re.fullmatch(r"[0-9a-f]{64}", epoch) for epoch in epochs):
        return {}, {"status": "not_comparable", "reason": "epoch_unknown"}
    if epochs[0] != epochs[1]:
        return {}, {"status": "not_comparable", "reason": "process_changed"}
    uptimes = [sample["values"].get("uptime_seconds") for sample in (before, after)]
    if not all(type(value) in (int, float) and math.isfinite(value) and value >= 0 for value in uptimes):
        return {}, {"status": "not_comparable", "reason": "sample_time_unknown"}
    if uptimes[1] < uptimes[0]:
        return {}, {"status": "not_comparable", "reason": "sample_time_reversed"}
    result = {}
    try:
        for group, fields in COUNTER_FIELDS.items():
            for field in fields:
                previous, current = (counter_value(sample, group, field) for sample in (before, after))
                if previous is None or current is None:
                    continue
                if current < previous:
                    return {}, {"status": "not_comparable", "reason": "counter_reset"}
                result[group + "." + field] = current - previous
    except ValueError:
        return {}, {"status": "not_comparable", "reason": "invalid_counter"}
    return result, ({"status": "measured"} if result else
                    {"status": "unavailable", "reason": "no_comparable_counters"})


class Failure(Exception):
    def __init__(self, code, category="mount", **facts):
        super().__init__(code)
        self.code, self.category, self.facts = code, category, facts


def need(condition, code, category="mount", **facts):
    if not condition:
        raise Failure(code, category, **facts)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def relative(value, empty=False):
    need(isinstance(value, str), "invalid_path", "configuration")
    parts = value.split("/")
    need((empty and value == "") or (value and not value.startswith("/") and
         all(p not in ("", ".", "..") for p in parts) and "\x00" not in value),
         "invalid_path", "configuration")
    return value


def local_host(host):
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise Failure("redirect_refused", "authentication" if "/login" in newurl else "transport", status=code)


class BrowseParser(HTMLParser):
    def __init__(self):
        super().__init__()
        self.entries, self.next, self.marker = {}, None, False
        self.next_marker = False

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        need(not (a.get("role") == "alert" and "error" in a.get("class", "").split()),
             "browse_error_page", "web")
        if a.get("id") == "fileListView" or "data-file-page" in a:
            self.marker = True
        # Grid and list render the same rows: use only the list representation.
        if "file-list-row" in a.get("class", "").split() and "data-file-id" in a:
            key = a["data-file-id"]
            need(key not in self.entries, "duplicate_browse_row")
            self.entries[key] = a["data-favorite-name"]
        if "data-load-more" in a:
            need(not self.next_marker, "duplicate_next_link")
            self.next_marker = True
            href = a.get("href")
            need(isinstance(href, str) and bool(href.strip()), "invalid_cursor", "protocol")
            self.next = href


def numeric_stats(value):
    # Never copy arbitrary values, paths, status reasons, URLs or configuration.
    out = {}
    for path in sorted(NUMERIC_STAT_FIELDS):
        number = stats_path(value, path)
        if type(number) is int or (type(number) is float and math.isfinite(number)):
            out[path] = number
    return out


def worker(job):
    action = job["action"]
    if action == "scan":
        entries = []
        with os.scandir(job["path"]) as it:
            for entry in it:
                need(len(entries) < job["max_entries"], "entry_budget", "budget")
                # Request attributes too: exercises the kernel's READDIRPLUS/lookup path.
                info = entry.stat(follow_symlinks=False)
                entries.append({"name": entry.name, "dir": stat.S_ISDIR(info.st_mode)})
        return {"entries": entries}
    if action == "read":
        fd = os.open(job["path"], os.O_RDONLY | os.O_NONBLOCK)
        try:
            info = os.fstat(fd)
            need(stat.S_ISREG(info.st_mode), "not_regular_file")
            need(info.st_size == job["size"], "file_size_mismatch")
            data = os.pread(fd, job["length"], job["offset"])
            return {"data": base64.b64encode(data).decode(), "bytes": len(data)}
        finally:
            os.close(fd)
    if action == "stats":
        with socket.socket(socket.AF_UNIX) as sock:
            sock.settimeout(job["timeout"])
            sock.connect(job["path"])
            sock.sendall(b'{"command":"io-stats"}\n')
            data = bytearray()
            while b"\n" not in data:
                chunk = sock.recv(min(4096, 262145 - len(data)))
                need(chunk and len(data) + len(chunk) <= 262144, "stats_response_invalid")
                data.extend(chunk)
        value = json.loads(data)
        need("io_stats" in value and "error" not in value, "stats_response_invalid")
        need(isinstance(value["io_stats"], dict), "stats_response_invalid")
        return {"values": numeric_stats(value["io_stats"]), **stats_metadata(value["io_stats"])}
    need(action == "http", "invalid_worker", "configuration")
    url = urllib.parse.urlsplit(job["url"])
    handlers = [NoRedirect()]
    if local_host(url.hostname):
        handlers.append(urllib.request.ProxyHandler({}))
    opener = urllib.request.build_opener(*handlers)
    headers = {"Accept-Encoding": "identity"}
    if job.get("range"):
        headers["Range"] = job["range"]
    # Credentials stay in the worker environment and request headers, never JSON/argv.
    for env, header in (("MOUNT123_ACCEPTANCE_COOKIE", "Cookie"),
                        ("MOUNT123_ACCEPTANCE_AUTHORIZATION", "Authorization")):
        if os.environ.get(env):
            headers[header] = os.environ[env]
    request = urllib.request.Request(job["url"], headers=headers)
    try:
        response = opener.open(request, timeout=job["timeout"])
    except urllib.error.HTTPError as exc:
        status = exc.code
        exc.close()
        category = "authentication" if status in (401, 403) else "platform" if status == 429 else "web"
        raise Failure("http_status", category, status=status)
    with response:
        data = response.read(job["limit"] + 1)
        need(len(data) <= job["limit"], "response_byte_budget", "budget")
        return {"status": response.status, "bytes": len(data),
                "content_range": response.headers.get("Content-Range", ""),
                "content_type": response.headers.get("Content-Type", ""),
                "encoding": response.headers.get("Content-Encoding", "identity"),
                "data": base64.b64encode(data).decode()}


def worker_main():
    try:
        job = json.load(sys.stdin)
        started = time.monotonic()
        result = worker(job)
        result["seconds"] = round(time.monotonic() - started, 6)
        print(json.dumps({"ok": result}))
    except Failure as exc:
        print(json.dumps({"error": {"code": exc.code, "category": exc.category, **exc.facts}}))
    except OSError as exc:
        category = "format_or_policy" if exc.errno in (errno.ELOOP, errno.ENOTSUP) else "transport" if isinstance(exc, (TimeoutError, ConnectionError)) else "mount"
        print(json.dumps({"error": {"code": "os_error", "category": category, "errno": exc.errno}}))
    except Exception:
        # Exception strings can contain response bodies, signed URLs and filenames.
        print(json.dumps({"error": {"code": "invalid_response", "category": "protocol"}}))


def run_worker(job, timeout):
    started = time.monotonic()
    proc = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), "--worker"],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        raw, _ = proc.communicate(json.dumps(job).encode(), timeout=timeout)
    except (subprocess.TimeoutExpired, KeyboardInterrupt):
        os.killpg(proc.pid, signal.SIGKILL)
        try:
            proc.communicate(timeout=1)
        except subprocess.TimeoutExpired:
            # Linux uninterruptible FUSE I/O can survive SIGKILL until kernel return.
            # Stop the suite; never pile up more blocked workers.
            raise Failure("worker_unreaped", "environment", pid=proc.pid)
        raise Failure("operation_deadline", "timeout")
    need(proc.returncode == 0, "worker_failed", "environment")
    result = json.loads(raw)
    if "error" in result:
        error = result["error"]
        raise Failure(error.pop("code"), error.pop("category"), **error)
    result["ok"]["wall_seconds"] = round(time.monotonic() - started, 6)
    return result["ok"]


class Runner:
    def __init__(self, args):
        self.args = args
        self.deadline = time.monotonic() + args.max_seconds
        self.bytes = 0
        self.operations = 0

    def call(self, job):
        remaining = self.deadline - time.monotonic()
        need(remaining > 0, "suite_deadline", "budget")
        need(self.operations < self.args.max_operations, "operation_budget", "budget")
        self.operations += 1
        timeout = min(self.args.timeout, remaining)
        job["timeout"] = timeout
        result = run_worker(job, timeout)
        self.bytes += result.get("bytes", 0)
        return result

    def reserve(self, amount):
        need(self.bytes + amount <= self.args.max_bytes, "suite_byte_budget", "budget")

    def http(self, path, limit, byte_range=None):
        self.reserve(limit + 1)
        return self.call({"action": "http", "url": self.args.base_url.rstrip("/") + path,
                          "limit": limit, "range": byte_range})

    def scan(self, path):
        return self.call({"action": "scan", "path": str(Path(self.args.mount) / path),
                          "max_entries": self.args.max_entries})

    def stats(self):
        if not self.args.control_socket:
            return {"available": False, "reason": "not_configured"}
        try:
            result = self.call({"action": "stats", "path": self.args.control_socket})
            return {"available": True, "values": result["values"], "epoch": result["epoch"], "groups": result["groups"]}
        except Failure as exc:
            if exc.code == "worker_unreaped":
                raise
            return {"available": False, "reason": exc.code}

    def listing(self, case, step):
        names, cursor, seen, pages = [], "", set(), []
        total = None
        for _ in range(self.args.max_pages):
            if case["kind"] == "archive":
                query = urllib.parse.urlencode({"path": step["path"], "cursor": cursor})
                path = f'/archive/{case["file_id"]}/entries?{query}'
            else:
                query = urllib.parse.urlencode({"parent_id": case["parent_id"], "offset": cursor or "0"})
                path = "/browse?" + query
            response = self.http(path, self.args.max_page_bytes)
            need(response["status"] == 200, "listing_status", "web")
            body = base64.b64decode(response["data"])
            if case["kind"] == "archive":
                need("application/json" in response["content_type"], "listing_not_json", "protocol")
                value = json.loads(body)
                batch = [entry["name"] for entry in value["items"]]
                total = value["total"]
                need(type(total) is int and total >= 0 and type(value["complete"]) is bool,
                     "invalid_pagination", "protocol")
                next_cursor = value["next_cursor"]
                need(isinstance(next_cursor, str), "invalid_cursor", "protocol")
                complete = value["complete"]
            else:
                parser = BrowseParser()
                parser.feed(body.decode("utf8"))
                need(parser.marker, "listing_not_browse", "protocol")
                batch = list(parser.entries.values())
                complete, next_cursor = True, ""
                if parser.next:
                    link = urllib.parse.urlsplit(parser.next)
                    q = urllib.parse.parse_qs(link.query)
                    need(not link.scheme and not link.netloc and link.path == "/browse" and
                         q.get("parent_id") == [str(case["parent_id"])], "unsafe_pagination", "protocol")
                    next_cursor = q.get("offset", [""])[0]
                    need(next_cursor.isdigit() and int(next_cursor) > int(cursor or 0), "invalid_cursor", "protocol")
            need(all(isinstance(n, str) and n not in ("", ".", "..") and "/" not in n for n in batch), "invalid_entry", "protocol")
            need(len(set(batch)) == len(batch) and not set(batch).intersection(names), "duplicate_entries", "pagination")
            names.extend(batch)
            need(len(names) <= self.args.max_entries, "entry_budget", "budget")
            pages.append({"count": len(batch), "seconds": response["seconds"], "complete": complete})
            if not next_cursor:
                need(complete, "incomplete_listing", "pagination")
                if total is not None:
                    need(total == len(names), "total_mismatch", "pagination", advertised=total, actual=len(names))
                return names, pages
            need(next_cursor not in seen, "cursor_cycle", "pagination")
            seen.add(next_cursor)
            cursor = next_cursor
        raise Failure("page_budget", "budget")

    def compare_directory(self, case, step):
        # FUSE first is essential: web indexing must not mask the outer-first trigger.
        path = case["mount_path"] + (("/" + step["path"]) if step["path"] else "")
        fuse = self.scan(path)
        web, pages = self.listing(case, step)
        actual = [e["name"] for e in fuse["entries"]]
        need(len(actual) == len(set(actual)), "duplicate_fuse_entries")
        need(set(actual) == set(web), "directory_mismatch", web_count=len(web), fuse_count=len(actual),
             missing_count=len(set(web) - set(actual)), extra_count=len(set(actual) - set(web)))
        if "count" in step:
            need(len(web) == step["count"], "expected_count_mismatch", actual=len(web), expected=step["count"])
        fingerprint = digest(json.dumps(sorted(web), ensure_ascii=False).encode())
        return {"count": len(web), "names_sha256": fingerprint, "pages": pages, "fuse_seconds": fuse["seconds"]}

    def read_member(self, case, sample):
        size = sample["size"]
        length = min(READ_SIZE, size)
        results, baseline = [], {}
        for repeat in range(self.args.repeats):
            for edge, offset in (("head", 0), ("tail", max(0, size - length))):
                if getattr(self.args, "range_edge", []) and edge not in self.args.range_edge:
                    continue
                self.reserve(length)
                fuse = self.call({"action": "read", "path": str(Path(self.args.mount) / case["mount_path"] / sample["path"]),
                                  "size": size, "offset": offset, "length": length})
                query = urllib.parse.urlencode({"path": sample["path"]})
                web = self.http(f'/archive/{case["file_id"]}/member?{query}', length,
                                f"bytes={offset}-{offset + length - 1}")
                need(web["status"] == 206, "range_not_partial", "protocol", status=web["status"])
                need(web["content_range"] == f"bytes {offset}-{offset + length - 1}/{size}", "content_range_mismatch", "protocol")
                need(web["encoding"] in ("", "identity"), "encoded_range", "protocol")
                a, b = base64.b64decode(fuse["data"]), base64.b64decode(web["data"])
                need(len(a) == length and len(b) == length, "short_range", "protocol")
                need(a == b, "range_bytes_mismatch", "integrity")
                sha = digest(a)
                need(edge not in baseline or baseline[edge] == sha, "repeat_bytes_changed", "integrity")
                baseline[edge] = sha
                expected = sample.get(edge + "_sha256")
                need(not expected or sha == expected, "expected_hash_mismatch", "integrity")
                if edge == "head" and sample.get("magic_hex"):
                    need(a.startswith(bytes.fromhex(sample["magic_hex"])), "signature_mismatch", "format_or_policy")
                results.append({"edge": edge, "repeat": repeat + 1, "bytes": length, "sha256": sha,
                                "signature_hex": a[:16].hex(), "fuse_seconds": fuse["seconds"], "web_seconds": web["seconds"]})
        return results

    def case(self, case):
        record = {"name": case["name"], "status": "running", "order": case["order"], "steps": [], "reads": []}
        record["stats_before"] = {"available": False, "reason": "not_collected"}
        record["stats_after"] = {"available": False, "reason": "not_collected"}
        record["stats_delta"] = {}
        record["stats_delta_status"] = {"status": "unavailable", "reason": "sample_unavailable"}
        try:
            record["stats_before"] = self.stats()
            for index, step in enumerate(case["directories"]):
                record.update(phase="directory", active_index=index)
                result = self.compare_directory(case, step)
                record["steps"].append({"index": index, **result})
            for index, sample in enumerate(case.get("reads", [])):
                record.update(phase="read", active_index=index)
                record["reads"].append({"index": index, "ranges": self.read_member(case, sample)})
            record["status"] = "passed"
            record["phase"] = "complete"
            record.pop("active_index", None)
        except Failure as exc:
            record.update(status="failed", error={"code": exc.code, "category": exc.category, **exc.facts})
        except (KeyError, ValueError, TypeError):
            record.update(status="failed", error={"code": "invalid_response", "category": "protocol"})
        if record.get("error", {}).get("code") != "worker_unreaped":
            try:
                record["stats_after"] = self.stats()
            except Failure as exc:
                record.update(status="failed", error={"code": exc.code, "category": exc.category, **exc.facts})
                return record
            before, after = record["stats_before"], record["stats_after"]
            record["stats_delta"], record["stats_delta_status"] = stats_delta(before, after)
        return record


def validate_manifest(value, max_depth):
    need(isinstance(value, dict) and value.get("version") == VERSION, "manifest_version", "configuration")
    cases = value["cases"]
    need(isinstance(cases, list) and cases, "empty_manifest", "configuration")
    seen = set()
    for case in cases:
        name = case["name"]
        need(isinstance(name, str) and re.fullmatch(r"[a-zA-Z0-9_-]{1,64}", name) and name not in seen, "invalid_case_name", "configuration")
        seen.add(name)
        need(case["kind"] in ("archive", "browse") and case["order"] in ("outer-first", "direct"), "invalid_case", "configuration")
        relative(case["mount_path"], empty=case["kind"] == "browse")
        key = "file_id" if case["kind"] == "archive" else "parent_id"
        minimum = 1 if case["kind"] == "archive" else 0
        need(type(case[key]) is int and case[key] >= minimum, "invalid_id", "configuration")
        directories = case["directories"]
        need(isinstance(directories, list) and directories, "missing_directories", "configuration")
        previous = None
        for step in directories:
            relative(step["path"], empty=True)
            need(type(step["archive_depth"]) is int and 0 <= step["archive_depth"] <= max_depth, "depth_budget", "configuration")
            if previous is not None:
                need(step["path"].startswith(previous + "/") if previous else bool(step["path"]), "directory_order", "configuration")
            previous = step["path"]
        if case["order"] == "outer-first":
            need(directories[0]["path"] == "", "outer_first_path", "configuration")
        if case["kind"] == "browse":
            need(len(directories) == 1 and directories[0]["path"] == "" and not case.get("reads"), "browse_scope", "configuration")
        for sample in case.get("reads", []):
            relative(sample["path"])
            need(type(sample["size"]) is int and sample["size"] > 0, "invalid_sample_size", "configuration")
            need(type(sample["archive_depth"]) is int and 0 <= sample["archive_depth"] <= max_depth, "depth_budget", "configuration")
            for edge in ("head", "tail"):
                if sample.get(edge + "_sha256"):
                    need(re.fullmatch(r"[0-9a-f]{64}", sample[edge + "_sha256"]), "invalid_hash", "configuration")
            if sample.get("magic_hex"):
                need(re.fullmatch(r"(?:[0-9a-fA-F]{2}){1,32}", sample["magic_hex"]), "invalid_magic", "configuration")
    return cases


def write_report(path, value):
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".acceptance-", dir=target.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, indent=2, ensure_ascii=False)
            stream.write("\n")
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--manifest")
    parser.add_argument("--base-url", default="http://127.0.0.1:8082")
    parser.add_argument("--mount")
    parser.add_argument("--cache", help="Recorded only as configured; never scanned, cleared or modified")
    parser.add_argument("--control-socket")
    parser.add_argument("--report", default="live-acceptance-report.json")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--case", action="append", default=[])
    parser.add_argument("--timeout", type=float, default=40)
    parser.add_argument("--max-seconds", type=float, default=180)
    parser.add_argument("--max-bytes", type=int, default=16 * 1024 * 1024)
    parser.add_argument("--max-page-bytes", type=int, default=1024 * 1024)
    parser.add_argument("--max-pages", type=int, default=32)
    parser.add_argument("--max-entries", type=int, default=2000)
    parser.add_argument("--max-operations", type=int, default=100)
    parser.add_argument("--max-depth", type=int, default=2)
    parser.add_argument("--repeats", type=int, default=2)
    parser.add_argument("--range-edge", action="append", choices=("head", "tail"), default=[],
                        help="Read only selected edge(s); default reads both head and tail")
    args = parser.parse_args(argv)
    if not args.live:
        print(json.dumps({"status": "skipped", "reason": "live_not_enabled"}))
        return 0
    try:
        need(args.manifest and args.mount, "manifest_and_mount_required", "configuration")
        url = urllib.parse.urlsplit(args.base_url)
        need(url.scheme in ("http", "https") and url.hostname and not url.username and not url.password and
             not url.query and not url.fragment and url.path in ("", "/"), "invalid_base_url", "configuration")
        for key in ("timeout", "max_seconds", "max_bytes", "max_page_bytes", "max_pages", "max_entries", "max_operations", "repeats"):
            need(getattr(args, key) > 0, "invalid_budget", "configuration")
        raw = Path(args.manifest).read_bytes()
        all_cases = validate_manifest(json.loads(raw), args.max_depth)
        need(set(args.case).issubset({c["name"] for c in all_cases}), "unknown_case", "configuration")
        cases = [c for c in all_cases if not args.case or c["name"] in args.case]
        # Bind resume to inputs and target without exposing either in the report.
        identity = digest(raw + json.dumps({k: v for k, v in vars(args).items() if k not in ("resume", "report", "case")}, sort_keys=True).encode())
        report = {"version": VERSION, "identity": identity, "status": "running", "cache_configured": bool(args.cache), "cases": []}
        if args.resume:
            old = json.loads(Path(args.report).read_bytes())
            need(old.get("version") == VERSION and old.get("identity") == identity, "resume_identity_mismatch", "configuration")
            # Copy only results produced by this version; callers must trust their own report.
            report = old
        runner = Runner(args)
        completed = {c["name"] for c in report["cases"] if c["status"] == "passed"}
        for case in cases:
            if case["name"] in completed:
                continue
            report["cases"] = [c for c in report["cases"] if c["name"] != case["name"]]
            result = runner.case(case)
            report["cases"].append(result)
            report["status"] = "running"
            report["attempt"] = {"operations": runner.operations, "response_bytes": runner.bytes}
            write_report(args.report, report)
            print(json.dumps({"case": case["name"], "status": result["status"], "error": result.get("error")}))
            if result.get("error", {}).get("category") in ("budget", "timeout", "environment"):
                break
        selected = {c["name"] for c in cases}
        passed = {c["name"] for c in report["cases"] if c["status"] == "passed"}
        report["status"] = "passed" if selected.issubset(passed) else "failed"
        report["selected_cases"] = sorted(selected)
        write_report(args.report, report)
        return 0 if report["status"] == "passed" else 1
    except Failure as exc:
        print(json.dumps({"status": "failed", "code": exc.code, "category": exc.category, **exc.facts}))
        return 2
    except (OSError, ValueError, KeyError, TypeError):
        print(json.dumps({"status": "failed", "code": "invalid_configuration", "category": "configuration"}))
        return 2


if __name__ == "__main__":
    if sys.argv[1:] == ["--worker"]:
        worker_main()
    else:
        sys.exit(main())
