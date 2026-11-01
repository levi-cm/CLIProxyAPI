#!/usr/bin/env python3
"""Passive Linux packet observer; stdout contains counters and timestamps only.

Run in the serving network namespace. No requests are sent. Packet bytes exist
only transiently in memory: only a bounded HTTP request line is accumulated;
header values and request bodies are never accumulated, logged, or written.
"""

import argparse
from collections import OrderedDict
import json
import select
import socket
import struct
import sys
import time


class RequestParser:
    MAX_LINE = 2048
    MAX_HEADERS = 65536
    HEADER_NAMES = (b"content-length", b"transfer-encoding", b"upgrade")

    def __init__(self, activity):
        self.activity = activity
        self.opaque = False
        self.saw_monitoring = False
        self.monitoring_only = True
        self._new_request()

    def _new_request(self):
        self.line = bytearray()
        self.state = "line"
        self.business = False
        self.header_bytes = 0
        self.length = None
        self.remaining = 0
        self.header_pos = 0
        self.candidates = set(range(len(self.HEADER_NAMES)))
        self.header_kind = None
        self.header_value = 0
        self.value_digits = 0
        self.value_trailing = False
        self.header_cr = False

    @property
    def pending(self):
        return not self.opaque and (self.state != "line" or bool(self.line))

    @property
    def monitoring_close(self):
        return self.saw_monitoring and self.monitoring_only and not self.pending and not self.opaque

    def uncertain(self):
        self.monitoring_only = False
        self.activity()
        self.opaque = True
        self.line.clear()

    def _request_line(self):
        parts = bytes(self.line).split(b" ")
        self.line.clear()
        if len(parts) != 3 or parts[2] not in (b"HTTP/1.0", b"HTTP/1.1"):
            self.uncertain()
            return
        method, target, _ = parts
        # Query strings do not change the endpoint, but absolute/encoded/odd
        # request targets are deliberately not interpreted as safe monitoring.
        path = target.split(b"?", 1)[0]
        safe_path = path in (b"/healthz", b"/v1/models", b"/management.html")
        safe_path = safe_path or path.startswith(b"/v8/management/")
        self.business = not (method in (b"GET", b"HEAD") and safe_path)
        if self.business:
            self.monitoring_only = False
            self.activity()
        else:
            self.saw_monitoring = True
        self.state = "headers"

    def _finish_header(self):
        if self.header_kind == 0:
            if not self.value_digits or self.length is not None:
                self.uncertain()
                return
            self.length = self.header_value
        elif self.header_kind in (1, 2):
            # Chunked framing and protocol upgrades stay opaque/fail-closed.
            self.uncertain()
            return
        self.header_pos = 0
        self.candidates = set(range(len(self.HEADER_NAMES)))
        self.header_kind = None
        self.header_value = 0
        self.value_digits = 0
        self.value_trailing = False
        self.state = "headers"

    def _header_byte(self, byte):
        self.header_bytes += 1
        if self.header_bytes > self.MAX_HEADERS:
            self.uncertain()
            return
        if self.header_cr:
            self.header_cr = False
            if byte != 10:
                self.uncertain()
            elif self.state == "headers" and self.header_pos == 0:
                self.remaining = self.length or 0
                if self.remaining:
                    # Even a monitoring GET carrying a body is business.
                    self.business = True
                    self.monitoring_only = False
                    self.activity()
                    self.state = "body"
                else:
                    self._new_request()
            elif self.state == "value":
                self._finish_header()
            else:
                self.uncertain()
            return
        if byte == 13:
            self.header_cr = True
            return
        if byte == 10 or byte < 32 and byte != 9 or byte == 127:
            self.uncertain()
            return
        if self.state == "headers":
            if byte == 58:
                if self.header_pos == 0:
                    self.uncertain()
                    return
                self.header_kind = next((i for i in self.candidates
                                         if len(self.HEADER_NAMES[i]) == self.header_pos), None)
                self.state = "value"
                return
            # Reject whitespace/invalid field-name bytes, without storing them.
            if not (byte in b"!#$%&'*+-.^_`|~" or 48 <= byte <= 57
                    or 65 <= byte <= 90 or 97 <= byte <= 122):
                self.uncertain()
                return
            lower = byte + 32 if 65 <= byte <= 90 else byte
            self.candidates = {i for i in self.candidates
                               if self.header_pos < len(self.HEADER_NAMES[i])
                               and self.HEADER_NAMES[i][self.header_pos] == lower}
            self.header_pos += 1
        elif self.header_kind == 0:
            if byte in (32, 9):
                if self.value_digits:
                    self.value_trailing = True
            elif 48 <= byte <= 57 and not self.value_trailing:
                self.value_digits += 1
                if self.value_digits > 18:
                    self.uncertain()
                    return
                self.header_value = self.header_value * 10 + byte - 48
            else:
                self.uncertain()

    def feed(self, payload):
        if self.opaque:
            if payload:
                self.activity()
            return
        offset = 0
        while offset < len(payload):
            if self.opaque:
                return
            if self.state == "body":
                count = min(self.remaining, len(payload) - offset)
                self.activity()
                self.remaining -= count
                offset += count
                if not self.remaining:
                    self._new_request()
                continue
            byte = payload[offset]
            offset += 1
            if self.state == "line":
                self.line.append(byte)
                if len(self.line) > self.MAX_LINE:
                    self.uncertain()
                elif byte == 10:
                    if len(self.line) < 2 or self.line[-2] != 13:
                        self.uncertain()
                    else:
                        del self.line[-2:]
                        self._request_line()
                elif byte < 32 and byte != 13 or byte > 126:
                    self.uncertain()
            else:
                if self.business:
                    self.activity()
                self._header_byte(byte)


class Observer:
    def __init__(self, port=8317, clock=time.monotonic, max_connections=4096):
        self.port = port
        self.clock = clock
        self.max_connections = max_connections
        self.last_business = clock()
        self.connections = OrderedDict()
        self.capture_drops = 0
        self.healthy = False

    def activity(self):
        self.last_business = self.clock()

    def snapshot(self):
        return {"timestamp": self.clock(), "last_business": self.last_business,
                "healthy": self.healthy, "pending": sum(p.pending for _, p in self.connections.values()),
                "capture_drops": self.capture_drops}

    def dropped(self, count):
        if count:
            self.capture_drops += count
            self.activity()
            self.connections.clear()

    def failed(self):
        self.healthy = False
        self.activity()

    def tcp(self, key, sequence, flags, payload):
        entry = self.connections.pop(key, None)
        if flags & 2:  # SYN consumes one sequence number.
            if entry is not None:
                self.activity()
            entry = None
            sequence = (sequence + 1) & 0xffffffff
        if flags & 4:
            # A proved, fully consumed monitoring-only connection may close
            # with RST. Unknown, partial, upgraded, or ambiguous traffic stays
            # fail-closed, including unexpected sequence numbers and payload.
            if entry is None or payload or sequence != entry[0] or not entry[1].monitoring_close:
                self.activity()
            return
        if entry is None:
            if not payload:
                return
            parser = RequestParser(self.activity)
            expected = sequence
        else:
            expected, parser = entry
        if payload:
            if sequence != expected:
                # Retransmits, overlap, gaps, wraparound ambiguity: never guess.
                parser.uncertain()
            parser.feed(payload)
            expected = (sequence + len(payload)) & 0xffffffff
        if flags & 1:
            if parser.pending:
                self.activity()
            return
        if len(self.connections) >= self.max_connections:
            self.connections.popitem(last=False)
            self.activity()
        self.connections[key] = (expected, parser)

    def frame(self, frame, packet_type=0):
        if packet_type == 4:  # PACKET_OUTGOING: avoid loopback duplicate capture.
            return
        try:
            self._frame(frame)
        except (ValueError, IndexError, struct.error):
            self.activity()

    def _frame(self, frame):
        if len(frame) < 14:
            raise ValueError()
        proto = struct.unpack_from("!H", frame, 12)[0]
        start = 14
        for _ in range(2):
            if proto not in (0x8100, 0x88a8):
                break
            proto = struct.unpack_from("!H", frame, start + 2)[0]
            start += 4
        if proto == 0x0800:
            if frame[start] >> 4 != 4:
                raise ValueError()
            ihl = (frame[start] & 15) * 4
            total = struct.unpack_from("!H", frame, start + 2)[0]
            if ihl < 20 or total < ihl or start + total > len(frame):
                raise ValueError()
            if frame[start + 9] != 6:
                return
            if struct.unpack_from("!H", frame, start + 6)[0] & 0x3fff:
                raise ValueError()  # Fragmented TCP cannot be classified safely.
            addresses = bytes(frame[start + 12:start + 20])
            end = start + total
            start += ihl
        elif proto == 0x86dd:
            if frame[start] >> 4 != 6:
                raise ValueError()
            end = start + 40 + struct.unpack_from("!H", frame, start + 4)[0]
            if end > len(frame):
                raise ValueError()
            addresses = bytes(frame[start + 8:start + 40])
            next_header = frame[start + 6]
            start += 40
            for _ in range(8):
                if next_header == 6:
                    break
                if next_header == 44:
                    raise ValueError()
                if next_header not in (0, 43, 60, 51):
                    return
                following, length = frame[start], frame[start + 1]
                start += (length + 2) * 4 if next_header == 51 else (length + 1) * 8
                next_header = following
                if start > end:
                    raise ValueError()
            if next_header != 6:
                raise ValueError()
        else:
            return
        if start + 20 > end:
            raise ValueError()
        source, dest, sequence = struct.unpack_from("!HHI", frame, start)
        if dest != self.port:
            return
        offset = (frame[start + 12] >> 4) * 4
        if offset < 20 or start + offset > end:
            raise ValueError()
        key = (addresses, source, dest)
        self.tcp(key, sequence, frame[start + 13], memoryview(frame)[start + offset:end])


def run(port):
    observer = Observer(port)
    capture = None
    try:
        capture = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3))
        capture.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 * 1024 * 1024)
        capture.setblocking(False)
        observer.healthy = True
        heartbeat = time.monotonic()
        while True:
            now = time.monotonic()
            if now >= heartbeat:
                # Linux resets these interval counters when they are read.
                stats = capture.getsockopt(263, 6, 8)  # SOL_PACKET, PACKET_STATISTICS
                _, dropped = struct.unpack("II", stats)
                observer.dropped(dropped)
                print(json.dumps(observer.snapshot(), separators=(",", ":")), flush=True)
                heartbeat = now + 1.0
            ready, _, _ = select.select([capture], [], [], max(0, heartbeat - time.monotonic()))
            if ready:
                # Return to heartbeat processing even under sustained traffic.
                for _ in range(256):
                    try:
                        packet, address = capture.recvfrom(65535)
                    except BlockingIOError:
                        break
                    observer.frame(packet, address[2])
                    del packet
    except (OSError, ValueError, struct.error):
        observer.failed()
        print(json.dumps(observer.snapshot(), separators=(",", ":")), flush=True)
        return 1
    finally:
        if capture is not None:
            capture.close()


if __name__ == "__main__":
    arguments = argparse.ArgumentParser(description=__doc__)
    arguments.add_argument("--port", type=int, default=8317)
    args = arguments.parse_args()
    if not 1 <= args.port <= 65535:
        arguments.error("port must be between 1 and 65535")
    sys.exit(run(args.port))
