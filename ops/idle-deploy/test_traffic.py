import importlib.util
import contextlib
import io
import json
from pathlib import Path
import struct
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("idle_traffic", Path(__file__).with_name("traffic.py"))
traffic = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(traffic)


class Clock:
    def __init__(self):
        self.now = 10.0

    def __call__(self):
        return self.now


def frame(payload=b"", sequence=100, flags=0x18, port=8317, ipv6=False, extension=False):
    tcp = struct.pack("!HHIIBBHHH", 42000, port, sequence, 0, 5 << 4, flags, 0, 0, 0)
    if ipv6:
        extra = bytes((6, 0)) + bytes(6) if extension else b""
        ip = struct.pack("!IHBB16s16s", 6 << 28, len(extra + tcp + payload),
                         0 if extension else 6, 64, bytes(16), bytes(15) + b"\x01")
        return bytes(12) + b"\x86\xdd" + ip + extra + tcp + payload
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(tcp + payload),
                     0, 0, 64, 6, 0, b"\x7f\x00\x00\x01", b"\x7f\x00\x00\x01")
    return bytes(12) + b"\x08\x00" + ip + tcp + payload


class ObserverTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.observer = traffic.Observer(clock=self.clock)
        self.clock.now = 20.0

    def send(self, payload, **kwargs):
        self.observer.frame(frame(payload, **kwargs))

    def test_read_only_monitoring_is_ignored(self):
        for method in (b"GET", b"HEAD"):
            for path in (b"/healthz", b"/v1/models?check=1", b"/management.html",
                         b"/v8/management/account-policy"):
                with self.subTest(method=method, path=path):
                    parser = traffic.RequestParser(self.observer.activity)
                    parser.feed(method + b" " + path + b" HTTP/1.1\r\nHost: localhost\r\n"
                                b"Authorization: Bearer SECRET\r\n\r\n")
                    self.assertFalse(parser.pending)
                    self.assertFalse(parser.opaque)
                    self.assertEqual(self.observer.last_business, 10.0)
                    self.assertNotIn("SECRET", repr(vars(parser)))

    def test_complete_monitoring_reset_does_not_restart_idle_window(self):
        from core import IdleWindow
        clock=Clock();clock.now=0.
        observer=traffic.Observer(clock=clock);observer.healthy=True
        idle=IdleWindow(now=clock)
        request=b"GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n"
        for tick in range(30,601,30):
            clock.now=float(tick)
            observer.tcp(tick,100,0x18,request)
            observer.tcp(tick,100+len(request),0x04,b"")
        self.assertEqual(observer.last_business,0.)
        self.assertTrue(idle.ready(observer.snapshot()))

    def test_only_proven_complete_monitoring_resets_are_ignored(self):
        requests=(b"GET /healthz HTTP/1.1\r\n", # incomplete
                  b"POST /v1/responses HTTP/1.1\r\n\r\n",
                  b"GET /healthz HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}",
                  b"GET /healthz HTTP/1.1\r\nUpgrade: websocket\r\n\r\n",
                  b"POST /v1/responses HTTP/1.1\r\n\r\nGET /healthz HTTP/1.1\r\n\r\n")
        for request in requests:
            with self.subTest(request=request):
                observer=traffic.Observer(clock=self.clock)
                observer.tcp('client',100,0x18,request)
                self.clock.now+=1
                observer.tcp('client',100+len(request),0x04,b'')
                self.assertEqual(observer.last_business,self.clock.now)
        observer=traffic.Observer(clock=self.clock);self.clock.now+=1
        observer.tcp('unknown',100,0x04,b'')
        self.assertEqual(observer.last_business,self.clock.now)

    def test_monitoring_reset_with_payload_or_sequence_gap_is_uncertain(self):
        request=b"GET /healthz HTTP/1.1\r\n\r\n"
        for offset,payload in ((0,b'new work'),(1,b'')):
            observer=traffic.Observer(clock=self.clock)
            observer.tcp('client',100,0x18,request);self.clock.now+=1
            observer.tcp('client',100+len(request)+offset,0x04,payload)
            self.assertEqual(observer.last_business,self.clock.now)

    def test_models_business_and_unknown_are_business(self):
        requests = (b"POST /v1/responses HTTP/1.1\r\n\r\n",
                    b"GET /v1/responses HTTP/1.1\r\nUpgrade: websocket\r\n\r\n",
                    b"POST /v8/management/anything HTTP/1.1\r\n\r\n",
                    b"GET /v1/account-policy/mcp HTTP/1.1\r\n\r\n",
                    b"GET /unknown HTTP/1.1\r\n\r\n",
                    b"GET http://localhost/healthz HTTP/1.1\r\n\r\n",
                    b"GET /healthz/ HTTP/1.1\r\n\r\n",
                    b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", b"\x16\x03\x01opaque")
        for request in requests:
            with self.subTest(request=request):
                observer = traffic.Observer(clock=self.clock)
                self.clock.now += 1
                observer.frame(frame(request))
                self.assertEqual(observer.last_business, self.clock.now)

    def test_fragmented_headers_and_pipelined_model(self):
        first = b"GET /healthz HTTP/1.1\r\nAuthoriz"
        self.send(first)
        self.assertEqual(self.observer.snapshot()["pending"], 1)
        self.assertEqual(self.observer.last_business, 10)
        second = b"ation: Bearer SECRET\r\n\r\nHEAD /v1/models HTTP/1.1\r\n\r\n"
        self.send(second, sequence=100 + len(first))
        self.assertEqual(self.observer.snapshot()["pending"], 0)
        self.assertEqual(self.observer.last_business, 10)
        self.clock.now = 30
        self.send(b"POST /v1/responses HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}",
                  sequence=100 + len(first + second))
        self.assertEqual(self.observer.last_business, 30)

    def test_fragment_every_byte(self):
        request = b"GET /healthz HTTP/1.1\r\nContent-Length: 0\r\nAuthorization: SECRET\r\n\r\n"
        for index, value in enumerate(request):
            self.send(bytes([value]), sequence=100 + index)
        self.assertEqual(self.observer.last_business, 10)
        self.assertEqual(self.observer.snapshot()["pending"], 0)

    def test_business_body_is_not_retained_and_next_health_is_ignored(self):
        parser = traffic.RequestParser(self.observer.activity)
        parser.feed(b"POST /v1/responses HTTP/1.1\r\nContent-Length: 12\r\n\r\nSECRET")
        self.assertTrue(parser.pending)
        self.assertNotIn("SECRET", repr(vars(parser)))
        self.clock.now = 30
        parser.feed(b"SECRETGET /healthz HTTP/1.1\r\n\r\n")
        self.assertEqual(self.observer.last_business, 30)
        self.assertFalse(parser.pending)
        self.clock.now = 40
        parser.feed(b"GET /healthz HTTP/1.1\r\n\r\n")
        self.assertEqual(self.observer.last_business, 30)

    def test_monitoring_body_is_business(self):
        self.send(b"GET /healthz HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}")
        self.assertEqual(self.observer.last_business, 20)

    def test_malformed_headers_framing_upgrade_and_limits_fail_closed(self):
        suffixes = (b"Content-Length: nope\r\n\r\n", b"Content-Length: 0\r\nContent-Length: 0\r\n\r\n",
                    b"Content-Length: 2 3\r\n\r\n", b"Transfer-Encoding: chunked\r\n\r\n",
                    b"Upgrade: websocket\r\n\r\n", b" folded: value\r\n\r\n",
                    b"Bad Header: value\r\n\r\n", b"Foo: bar\nx", b"Foo: bar\rX",
                    b"X: " + b"a" * 65536)
        for suffix in suffixes:
            with self.subTest(suffix=suffix[:50]):
                parser = traffic.RequestParser(self.observer.activity)
                parser.feed(b"GET /healthz HTTP/1.1\r\n" + suffix)
                self.assertTrue(parser.opaque)
                self.assertEqual(self.observer.last_business, 20)
        parser = traffic.RequestParser(self.observer.activity)
        parser.feed(b"GET /" + b"a" * 3000)
        self.assertTrue(parser.opaque)
        self.assertLessEqual(len(parser.line), parser.MAX_LINE)

    def test_retransmission_overlap_and_gap_reset_idle(self):
        for offset in (0, 5, 500):
            with self.subTest(offset=offset):
                self.observer = traffic.Observer(clock=self.clock)
                request = b"GET /healthz HTTP/1.1\r\n\r\n"
                self.send(request)
                self.clock.now += 1
                self.send(request, sequence=100 + offset)
                self.assertEqual(self.observer.last_business, self.clock.now)
                self.assertTrue(next(iter(self.observer.connections.values()))[1].opaque)

    def test_ipv4_ipv6_extensions_and_vlan(self):
        for ipv6, extension, vlan in ((False, False, False), (True, False, False),
                                      (True, True, False), (False, False, True)):
            with self.subTest(ipv6=ipv6, extension=extension, vlan=vlan):
                self.observer = traffic.Observer(clock=self.clock)
                self.clock.now += 1
                packet = frame(b"POST /v1/responses HTTP/1.1\r\n\r\n", ipv6=ipv6, extension=extension)
                if vlan:
                    packet = packet[:12] + b"\x81\x00\x00\x01" + packet[12:]
                self.observer.frame(packet)
                self.assertEqual(self.observer.last_business, self.clock.now)

    def test_unrelated_port_and_outgoing_duplicate_ignored(self):
        self.send(b"POST /v1/responses HTTP/1.1\r\n\r\n", port=9999)
        self.observer.frame(frame(b"POST /v1/responses HTTP/1.1\r\n\r\n"), packet_type=4)
        self.assertEqual(self.observer.last_business, 10)
        self.assertEqual(len(self.observer.connections), 0)

    def test_truncated_fragmented_and_bad_tcp_fail_closed(self):
        packet = frame(b"GET /healthz HTTP/1.1\r\n\r\n")
        fragmented = bytearray(packet)
        fragmented[20] = 0x20
        malformed_tcp = bytearray(packet)
        malformed_tcp[46] = 0x10
        for malformed in (b"tiny", packet[:-1], fragmented, malformed_tcp):
            observer = traffic.Observer(clock=self.clock)
            self.clock.now += 1
            observer.frame(malformed)
            self.assertEqual(observer.last_business, self.clock.now)

    def test_connection_limit_reset_fin_and_pending(self):
        self.observer.max_connections = 2
        self.observer.tcp("a", 1, 0x18, b"GET ")
        self.assertEqual(self.observer.snapshot()["pending"], 1)
        self.observer.tcp("a", 5, 1, b"")
        self.assertEqual(self.observer.last_business, 20)
        self.assertEqual(len(self.observer.connections), 0)
        self.clock.now = 30
        for key in ("a", "b", "c"):
            self.observer.tcp(key, 1, 0x18, b"GET /healthz HTTP/1.1\r\n\r\n")
        self.assertEqual(len(self.observer.connections), 2)
        self.assertEqual(self.observer.last_business, 30)
        self.clock.now = 40
        self.observer.tcp("b", 1, 4, b"")
        self.assertEqual(self.observer.last_business, 40)
        self.assertEqual(len(self.observer.connections), 1)

    def test_capture_loss_and_failure_are_sanitized(self):
        self.send(b"GET /healthz HTTP/1.1\r\nAuthorization: SECRET")
        self.observer.healthy = True
        self.observer.dropped(3)
        snapshot = self.observer.snapshot()
        self.assertEqual(set(snapshot), {"timestamp", "last_business", "healthy", "pending", "capture_drops"})
        self.assertEqual(snapshot["capture_drops"], 3)
        self.assertEqual(snapshot["pending"], 0)
        self.assertEqual(snapshot["last_business"], 20)
        self.assertNotIn("SECRET", str(snapshot))
        self.clock.now = 30
        self.observer.failed()
        self.assertFalse(self.observer.snapshot()["healthy"])
        self.assertEqual(self.observer.last_business, 30)

    def test_capture_creation_and_stats_failure_output_no_error_details(self):
        for creation_fails in (True, False):
            with self.subTest(creation_fails=creation_fails):
                capture = mock.Mock()
                capture.getsockopt.side_effect = OSError("private socket details")
                with mock.patch.object(traffic.socket, "socket") as constructor:
                    if creation_fails:
                        constructor.side_effect = OSError("private socket details")
                    else:
                        constructor.return_value = capture
                    output = io.StringIO()
                    with contextlib.redirect_stdout(output):
                        self.assertEqual(traffic.run(8317), 1)
                snapshot = json.loads(output.getvalue())
                self.assertFalse(snapshot["healthy"])
                self.assertNotIn("private", output.getvalue())
                if not creation_fails:
                    capture.close.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
