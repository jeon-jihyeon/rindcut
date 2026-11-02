import argparse
import base64
import hashlib
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import sys
import time
from typing import Any

import adapter
import stdio


REVISION = "nodloop-live-v1"
PROJECTION = "nodloop.public.metadata.v1"
TOKEN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
MAX_LINE = 2 << 20
MAX_RESPONSE = 4 << 20
LIMITATIONS = ["metadata_only", "published_envelope_only", "single_record_kind", "guard_not_supported",
               "source_claims_only", "no_unrecorded_actions", "owner_declared_generation",
               "no_store_generation_attestation", "native_cursor_invalid_after_removal", "no_destination_readback"]
CLAIMS = {"verdict": {"approve", "edit", "reject", "withdraw"},
          "result": {"confirmed", "refuted", "inconclusive"},
          "actor": {"person", "code", "model", "unknown"},
          "proof": {"key", "terminal", "elicitation", "reply", "none"}}


def opaque(value: Any) -> str:
    if not isinstance(value, str) or not TOKEN.fullmatch(value):
        raise ValueError("invalid_reference")
    return value


class Failure(ValueError):
    pass


class Arguments(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        raise ValueError("invalid_arguments")


class MCP:
    def __init__(self, program: str, record_dir: str | None, timeout: float,
                 managed_process_group: bool = False) -> None:
        self.arguments = [program, "mcp", "--agent"]
        if record_dir is not None:
            self.arguments.extend(["--record-dir", record_dir])
        self.timeout = timeout
        self.managed_process_group = managed_process_group
        self.process = None
        self.selector = None
        self.buffer = b""
        self.received = 0
        self.sequence = 0
        self.closed = False

    def __enter__(self) -> "MCP":
        self.deadline = time.monotonic() + self.timeout
        try:
            self.process = subprocess.Popen(self.arguments, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                            stderr=subprocess.DEVNULL,
                                            start_new_session=not self.managed_process_group)
            self.selector = selectors.DefaultSelector()
            self.selector.register(self.process.stdout, selectors.EVENT_READ)
            initialized = self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                       "clientInfo": {"name": "rindcut-source", "version": "1"}})
            if initialized.get("protocolVersion") not in ("2025-06-18", "2025-11-25"):
                raise Failure("mcp_protocol")
            self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
            return self
        except BaseException:
            self.close()
            raise

    def __exit__(self, *_: Any) -> None:
        self.close()

    def send(self, message: dict[str, Any]) -> None:
        try:
            stdio.write(self.process.stdin, adapter.encoded(message) + b"\n", self.deadline)
        except TimeoutError:
            raise Failure("mcp_timeout") from None
        except (OSError, ValueError):
            raise Failure("mcp_io") from None

    def receive(self) -> dict[str, Any]:
        while True:
            if b"\n" in self.buffer:
                line, self.buffer = self.buffer.split(b"\n", 1)
                if len(line) > MAX_LINE:
                    raise Failure("mcp_limit")
                try:
                    value = adapter.decode(line)
                except (ValueError, UnicodeError, RecursionError) as error:
                    raise Failure("mcp_invalid_json") from error
                if not isinstance(value, dict):
                    raise Failure("mcp_protocol")
                return value
            if len(self.buffer) > MAX_LINE:
                raise Failure("mcp_limit")
            remaining = self.deadline - time.monotonic()
            if remaining <= 0:
                raise Failure("mcp_timeout")
            if not self.selector.select(min(remaining, 0.1)):
                if self.process.poll() is not None:
                    raise Failure("mcp_eof")
                continue
            chunk = os.read(self.process.stdout.fileno(), 65536)
            if not chunk:
                raise Failure("mcp_invalid_json" if self.buffer else "mcp_eof")
            self.received += len(chunk)
            if self.received > MAX_RESPONSE:
                raise Failure("mcp_limit")
            self.buffer += chunk

    def request(self, method: str, parameters: dict[str, Any]) -> dict[str, Any]:
        self.sequence += 1
        self.send({"jsonrpc": "2.0", "id": self.sequence, "method": method, "params": parameters})
        for _ in range(32):
            value = self.receive()
            if value.get("jsonrpc") != "2.0":
                raise Failure("mcp_protocol")
            if "method" in value:
                if "id" in value:
                    raise Failure("mcp_protocol")
                continue
            if type(value.get("id")) is not int or value["id"] != self.sequence:
                raise Failure("mcp_protocol")
            if "error" in value or not isinstance(value.get("result"), dict):
                raise Failure("mcp_protocol")
            return value["result"]
        raise Failure("mcp_limit")

    def records(self, kind: str, since: str, limit: int) -> dict[str, Any]:
        result = self.request("tools/call", {"name": "records", "arguments": {
            "kind": kind, "since": since, "limit": limit, "content": False}})
        if result.get("isError"):
            text = [item.get("text", "") for item in result.get("content", []) if isinstance(item, dict)]
            if any("schema: the cursor names no record" in value for value in text if isinstance(value, str)):
                raise Failure("cursor_invalid")
            raise Failure("mcp_tool_error")
        page = result.get("structuredContent")
        if not isinstance(page, dict):
            raise Failure("mcp_protocol")
        return page

    def close(self) -> None:
        if self.closed:
            return
        self.closed = True
        if self.selector is not None:
            self.selector.close()
        if self.process is None:
            return
        try:
            self.process.stdin.close()
        except (OSError, ValueError):
            pass
        try:
            if self.managed_process_group:
                self.stop_child()
            else:
                self.stop_group()
        finally:
            self.process.stdout.close()

    def stop_child(self) -> None:
        for sig in (None, signal.SIGTERM, signal.SIGKILL):
            if sig is not None:
                try:
                    self.process.send_signal(sig)
                except ProcessLookupError:
                    pass
            try:
                self.process.wait(timeout=0.2 if sig is None else 0.3)
                return
            except subprocess.TimeoutExpired:
                pass
        raise Failure("mcp_shutdown")

    def stop_group(self) -> None:
        try:
            self.process.wait(timeout=0.2)
        except subprocess.TimeoutExpired:
            pass
        group = self.process.pid
        if group <= 1 or group == os.getpgrp():
            raise Failure("mcp_shutdown")
        # The dedicated group survives its parent while descendants remain
        for sig in (signal.SIGTERM, signal.SIGKILL):
            try:
                os.killpg(group, sig)
            except ProcessLookupError:
                break
            deadline = time.monotonic() + 0.3
            while time.monotonic() < deadline:
                try:
                    self.process.wait(timeout=0.01)
                except subprocess.TimeoutExpired:
                    pass
                try:
                    os.killpg(group, 0)
                except ProcessLookupError:
                    break
                time.sleep(0.01)
        if self.process.poll() is None:
            try:
                self.process.wait(timeout=0.3)
            except subprocess.TimeoutExpired as error:
                raise Failure("mcp_shutdown") from error
        try:
            os.killpg(group, 0)
        except ProcessLookupError:
            pass
        else:
            raise Failure("mcp_shutdown")


class Reader:
    def __init__(self, program: str, record_dir: str | None, kind: str, instance: str, generation: str,
                 stream: str, timeout: float = 10, managed_process_group: bool = False) -> None:
        self.program = shutil.which(program)
        if self.program is None:
            raise ValueError("program_unavailable")
        self.program = str(Path(self.program).resolve())
        self.record_dir = str(Path(record_dir).resolve()) if record_dir is not None else None
        if self.record_dir is not None and not Path(self.record_dir).is_dir():
            raise ValueError("record_store_unavailable")
        self.kind, self.instance, self.generation, self.stream = map(opaque, (kind, instance, generation, stream))
        if kind == "guard" or not 0.1 <= timeout <= 30:
            raise ValueError("unsupported_source")
        self.timeout = timeout
        self.managed_process_group = managed_process_group

    def binding(self, source_id: str) -> str:
        values = [REVISION, PROJECTION, self.program, self.record_dir, self.kind, self.instance,
                  self.generation, self.stream, source_id]
        return hashlib.sha256(adapter.encoded(values)).hexdigest()

    def native_cursor(self, cursor: Any, binding: str) -> str:
        if cursor is None:
            return ""
        if not isinstance(cursor, str) or len(cursor) > 8192:
            raise Failure("cursor_invalid")
        try:
            raw = base64.b64decode(cursor + "=" * (-len(cursor) % 4), altchars=b"-_", validate=True)
            value = adapter.decode(raw)
            if set(value) != {"binding", "native"} or value["binding"] != binding:
                raise ValueError("cursor_scope")
            return self.check_cursor(value["native"])
        except (ValueError, TypeError, KeyError, UnicodeError, RecursionError) as error:
            raise Failure("cursor_invalid") from error

    def check_cursor(self, value: Any) -> str:
        if not isinstance(value, str) or len(value) > 4096 or not re.fullmatch(r"[A-Za-z0-9_-]*", value):
            raise ValueError("invalid_native_cursor")
        return value

    def project(self, record: dict[str, Any]) -> dict[str, Any]:
        version = record["schema_version"]
        versions = (1, 2) if self.kind in ("decide", "knob") else (1,)
        if record["record_kind"] != self.kind or type(version) is not int or version not in versions:
            raise ValueError("unsupported_native_record")
        identity = opaque(record["record_id"])
        attributes = {"record_kind": self.kind, "native_schema_version": version}
        for name, allowed in CLAIMS.items():
            if name in record:
                value = record[name]
                if not isinstance(value, str) or value not in allowed:
                    raise ValueError("invalid_claim")
                attributes["declared_" + name] = value
        event = {"source_instance": self.instance, "generation": self.generation, "stream": self.stream,
                 "id": identity, "type": "nodloop." + self.kind, "schema_version": "nodloop.raw/v" + str(version),
                 "adapter_revision": REVISION, "projection_profile": PROJECTION, "links": [],
                 "attributes": attributes, "evidence": [{"kind": "source_record", "ref": {
                     "namespace": "nodloop.records", "id": identity}}]}
        if self.kind == "run":
            event["action_id"] = identity
        elif "trace_id" in record:
            event["action_id"] = opaque(record["trace_id"])
        adapter.occurrence(event, record["time"])
        return event

    def read(self, request: dict[str, Any]) -> dict[str, Any]:
        required = {"protocol", "operation", "source_id", "source_revision", "source_instance", "stream",
                    "generation", "cursor", "limit"}
        if (set(request) != required or request["protocol"] != adapter.SOURCE_PROTOCOL
                or request["operation"] != "read"):
            raise ValueError("invalid_request")
        for name in ("source_id", "source_instance", "stream"):
            opaque(request[name])
        if type(request["source_revision"]) is not int or request["source_revision"] < 1:
            raise ValueError("invalid_revision")
        if type(request["limit"]) is not int or not 1 <= request["limit"] <= 100:
            raise ValueError("invalid_limit")
        capability = {"kind": "event_stream", "types": ["nodloop." + self.kind], "limitations": LIMITATIONS}
        envelope = ("protocol", "source_id", "source_revision", "source_instance", "stream")
        result = {name: request[name] for name in envelope}
        result.update(generation=self.generation, capability=capability, status="ok", records=[], next_cursor=None,
                      has_more=False, coverage={"kind": "event_stream", "limitations": list(LIMITATIONS)})
        binding = self.binding(request["source_id"])
        try:
            if (request["generation"] not in (None, self.generation) or request["source_instance"] != self.instance
                    or request["stream"] != self.stream):
                raise Failure("cursor_invalid")
            since = self.native_cursor(request["cursor"], binding)
            with MCP(self.program, self.record_dir, self.timeout, self.managed_process_group) as client:
                page = client.records(self.kind, since, request["limit"])
        except (Failure, OSError) as error:
            code = str(error) if isinstance(error, Failure) else "mcp_io"
            result["status"] = "cursor_invalid" if code == "cursor_invalid" else "unavailable"
            if code != "cursor_invalid":
                result["coverage"]["limitations"].extend(["unavailable", code])
            return result
        if not isinstance(page.get("records"), list) or len(page["records"]) > request["limit"]:
            raise ValueError("invalid_page")
        native = self.check_cursor(page["next"])
        if page["records"] and native == since:
            raise ValueError("native_cursor_stalled")
        result["records"] = [self.project(record) for record in page["records"]]
        cursor = adapter.encoded({"binding": binding, "native": native})
        result["next_cursor"] = base64.urlsafe_b64encode(cursor).decode().rstrip("=")
        result["has_more"] = len(page["records"]) == request["limit"]
        return result


def main() -> int:
    parser = Arguments(description="Read nodloop public MCP records as metadata-only source pages")
    parser.add_argument("--nodloop-program", required=True)
    parser.add_argument("--record-dir")
    parser.add_argument("--kind", required=True)
    parser.add_argument("--source-instance", required=True)
    parser.add_argument("--generation", required=True)
    parser.add_argument("--stream", required=True)
    parser.add_argument("--managed-process-group", action="store_true")
    try:
        args = parser.parse_args()
        if args.managed_process_group and os.getpgrp() != os.getpid():
            raise ValueError("invalid_managed_process_group")
        raw = sys.stdin.buffer.read(adapter.LIMIT + 1)
        if len(raw) > adapter.LIMIT:
            raise ValueError("input_limit")
        request = adapter.decode(raw)
        reader = Reader(args.nodloop_program, args.record_dir, args.kind,
                        args.source_instance, args.generation, args.stream,
                        managed_process_group=args.managed_process_group)
        output = adapter.encoded(reader.read(request))
        if len(output) > adapter.LIMIT:
            raise ValueError("output_limit")
        sys.stdout.buffer.write(output + b"\n")
    except (OSError, ValueError, KeyError, TypeError, UnicodeError, AttributeError,
            IndexError, RecursionError, OverflowError):
        sys.stderr.write("nodloop_live_failed\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
