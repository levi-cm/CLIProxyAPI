"""Offline tests: real kernel filter strips payloads, and attribution stays separate."""

import collections
import importlib.util
from pathlib import Path
import socket
import struct
import unittest


spec = importlib.util.spec_from_file_location("monitor_traffic", Path(__file__).with_name("monitor-traffic.py"))
monitor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(monitor)


def tcp(source=45000, destination=8317, header_size=20):
    header = bytearray(header_size)
    struct.pack_into("!HH", header, 0, source, destination)
    header[12] = (header_size // 4) << 4
    return bytes(header)


def ipv4(body, protocol=6, header_size=20, fragment_offset=0):
    header = bytearray(header_size)
    header[0] = 0x40 | (header_size // 4)
    header[9] = protocol
    struct.pack_into("!H", header, 2, header_size + len(body))
    struct.pack_into("!H", header, 6, fragment_offset)
    return bytes(header) + body


def ipv6(body, protocol=6):
    header = bytearray(40)
    header[0] = 0x60
    header[6] = protocol
    struct.pack_into("!H", header, 4, len(body))
    return bytes(header) + body


class TrafficTests(unittest.TestCase):
    def filtered(self, data):
        # AF_UNIX applies the same socket BPF interpreter without sending anything
        # over a network. No root, live router, or application data is involved.
        sender, receiver = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
        with sender, receiver:
            monitor.attach_header_filter(receiver)
            receiver.settimeout(1)
            sender.send(data)
            return receiver.recv(65535)

    def test_ipv4_payload_is_stripped_in_kernel_but_length_is_preserved(self):
        original = ipv4(tcp() + b"PRIVATE_APPLICATION_PAYLOAD" * 40)
        captured = self.filtered(original)
        self.assertEqual(len(captured), 33)
        self.assertEqual(monitor.packet_info(captured), (len(original), 45000, 8317, len(original) - 40))

    def test_ip_and_tcp_options_do_not_leak_payload_or_inflate_payload_count(self):
        original = ipv4(tcp(header_size=60) + b"PRIVATE", header_size=60)
        captured = self.filtered(original)
        self.assertEqual(len(captured), 73)
        self.assertEqual(monitor.packet_info(captured)[3], 7)

    def test_ipv6_tcp_payload_is_stripped(self):
        original = ipv6(tcp(8317, 45000) + b"PRIVATE")
        captured = self.filtered(original)
        self.assertEqual(len(captured), 53)
        self.assertEqual(monitor.packet_info(captured), (67, 8317, 45000, 7))

    def test_noninitial_ipv4_fragment_never_leaks_application_bytes(self):
        original = ipv4(b"PRIVATE_APPLICATION_PAYLOAD", fragment_offset=3)
        captured = self.filtered(original)
        self.assertEqual(len(captured), 20)
        self.assertEqual(monitor.packet_info(captured), (len(original), None, None, None))

    def test_ipv6_extension_is_explicitly_unattributable(self):
        original = ipv6(b"\x06" + bytes(7) + tcp() + b"PRIVATE", protocol=0)
        captured = self.filtered(original)
        self.assertEqual(len(captured), 40)
        self.assertEqual(monitor.packet_info(captured), (len(original), None, None, None))

    def test_udp_payload_is_stripped(self):
        for packet, wanted in ((ipv4(bytes(8) + b"PRIVATE", protocol=17), 28),
                               (ipv6(bytes(8) + b"PRIVATE", protocol=17), 48)):
            with self.subTest(wanted=wanted):
                self.assertEqual(len(self.filtered(packet)), wanted)

    def test_tcp_ack_has_zero_payload_but_consumes_network_bytes(self):
        packet = self.filtered(ipv4(tcp()))
        self.assertEqual(monitor.packet_info(packet), (40, 45000, 8317, 0))

    def test_client_and_provider_directions_do_not_double_count(self):
        counts = collections.Counter()
        for lane, direction, packet in (
            ("tailscale", "rx", (1000, 45000, 8317, 960)),
            ("tailscale", "tx", (200, 8317, 45000, 160)),
            ("router", "rx", (1000, 45000, 8317, 960)),
            ("router", "tx", (1000, 45001, 443, 960)),
            ("router", "rx", (200, 443, 45001, 160)),
            ("router", "tx", (200, 8317, 45000, 160)),
        ):
            monitor.account_packet(counts, lane, direction, packet, {8317})
        self.assertEqual(counts["proxy_ts_rx_bytes"], 1000)
        self.assertEqual(counts["proxy_ts_tx_bytes"], 200)
        self.assertEqual(counts["proxy_client_rx_bytes"], 1000)
        self.assertEqual(counts["proxy_upstream_tx_bytes"], 1000)
        self.assertEqual(counts["proxy_upstream_rx_bytes"], 200)
        self.assertEqual(counts["proxy_client_tx_bytes"], 200)
        self.assertEqual(counts["proxy_ts_rx_payload_bytes"], 960)

    def test_unrelated_tailscale_services_are_excluded(self):
        counts = collections.Counter()
        monitor.account_packet(counts, "tailscale", "rx", (100, 45000, 22, 60), {8317})
        self.assertEqual(dict(counts), {})

    def test_unattributable_packets_are_not_falsely_assigned_to_router(self):
        counts = collections.Counter()
        monitor.account_packet(counts, "tailscale", "rx", (100, None, None, None), {8317})
        self.assertEqual(dict(counts), {"tailscale_unattributed_ip_bytes": 100})

    def test_truncated_or_invalid_packets_do_not_crash(self):
        for data in (b"", b"\x45", bytes(80), b"\x40" + bytes(40)):
            with self.subTest(data=data):
                self.assertIsNone(monitor.packet_info(data))

    def test_bad_tcp_data_offset_is_unattributable(self):
        header = bytearray(tcp())
        header[12] = 0x10
        self.assertEqual(monitor.packet_info(ipv4(header)), (40, None, None, None))


if __name__ == "__main__":
    unittest.main()
