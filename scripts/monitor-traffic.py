#!/usr/bin/env python3
"""Passively log CLIProxyAPI/Tailscale traffic and host headroom once per second.

Linux, Python 3.12+, Docker, and root are required. No third-party Python packages.
Packet sockets retain only IP headers and the first 13 TCP-header bytes: application
payloads never reach this process. No packet dumps, addresses, or credentials are logged.
"""

import argparse
import collections
import csv
import ctypes
import datetime as dt
import json
import math
import os
from pathlib import Path
import selectors
import signal
import socket
import struct
import subprocess
import sys
import time


SOL_PACKET = 263
PACKET_STATISTICS = 6
SO_ATTACH_FILTER = 26
PACKET_OUTGOING = 4


class SockFilter(ctypes.Structure):
    _fields_ = [("code", ctypes.c_ushort), ("jt", ctypes.c_ubyte),
                ("jf", ctypes.c_ubyte), ("k", ctypes.c_uint32)]


class SockFprog(ctypes.Structure):
    _fields_ = [("len", ctypes.c_ushort), ("filter", ctypes.POINTER(SockFilter))]


def header_filter():
    """Classic BPF on L3 datagrams; never return application payload bytes.

    Noninitial IPv4 fragments and IPv6 extension headers stop at the IP header.
    TCP data-offset is in byte 12; the rest of its header is unnecessary here.
    """
    code, labels, pending = [], {}, []

    def emit(op, k=0, yes=None, no=None):
        pending.append((len(code), yes, no))
        code.append([op, 0, 0, k])

    def label(name):
        labels[name] = len(code)

    emit(0x30, 0)                 # LD B [0]
    emit(0x54, 0xF0)              # AND version mask
    emit(0x15, 0x40, "v4", "v6_check")
    label("v4")
    emit(0xB1, 0)                 # LDX MSH [0]: IPv4 header length
    emit(0x28, 6)                 # LD H fragment field
    emit(0x54, 0x1FFF)
    emit(0x25, 0, "v4_header", "v4_protocol")
    label("v4_protocol")
    emit(0x30, 9)
    emit(0x15, 6, "v4_tcp", "v4_udp_check")
    label("v4_tcp")
    emit(0x87)                    # TXA
    emit(0x04, 13)
    emit(0x16)                    # RET A
    label("v4_udp_check")
    emit(0x15, 17, "v4_udp", "v4_header")
    label("v4_udp")
    emit(0x87)
    emit(0x04, 8)
    emit(0x16)
    label("v4_header")
    emit(0x87)
    emit(0x16)
    label("v6_check")
    emit(0x15, 0x60, "v6", "reject")
    label("v6")
    emit(0x30, 6)
    emit(0x15, 6, "v6_tcp", "v6_udp_check")
    label("v6_tcp")
    emit(0x06, 53)                # RET K
    label("v6_udp_check")
    emit(0x15, 17, "v6_udp", "v6_header")
    label("v6_udp")
    emit(0x06, 48)
    label("v6_header")
    emit(0x06, 40)
    label("reject")
    emit(0x06, 0)
    for index, yes, no in pending:
        for slot, target in ((1, yes), (2, no)):
            if target is not None:
                offset = labels[target] - index - 1
                if not 0 <= offset <= 255:
                    raise ValueError("Invalid BPF jump")
                code[index][slot] = offset
    return [tuple(row) for row in code]


def attach_header_filter(sock):
    rows = header_filter()
    array = (SockFilter * len(rows))(*(SockFilter(*row) for row in rows))
    program = SockFprog(len(rows), array)
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.setsockopt(sock.fileno(), socket.SOL_SOCKET, SO_ATTACH_FILTER,
                       ctypes.byref(program), ctypes.sizeof(program)) != 0:
        err = ctypes.get_errno()
        raise OSError(err, os.strerror(err))


def packet_info(data):
    """Return (IP bytes, TCP source port, destination port, payload bytes).

    Unattributable packets retain their length but have None for ports/payload.
    Counts include retransmissions, IP/TCP headers and ACKs, but not Ethernet or
    encrypted WireGuard/DERP overhead. Payload sizes are calculated, never read.
    """
    if not data:
        return None
    version = data[0] >> 4
    if version == 4 and len(data) >= 20:
        size = int.from_bytes(data[2:4], "big")
        offset = (data[0] & 15) * 4
        if offset < 20 or size < offset:
            return None
        if data[9] != 6 or int.from_bytes(data[6:8], "big") & 0x1FFF:
            return size, None, None, None
    elif version == 6 and len(data) >= 40:
        size = 40 + int.from_bytes(data[4:6], "big")
        offset = 40
        if data[6] != 6:
            return size, None, None, None
    else:
        return None
    if len(data) < offset + 13:
        return size, None, None, None
    source, destination = struct.unpack_from("!HH", data, offset)
    tcp_size = (data[offset + 12] >> 4) * 4
    if tcp_size < 20 or size < offset + tcp_size:
        return size, None, None, None
    return size, source, destination, size - offset - tcp_size


def account_packet(counts, lane, direction, packet, ports):
    if packet is None:
        counts[lane + "_malformed_packets"] += 1
        return
    size, source, destination, payload = packet
    local_port = source if direction == "tx" else destination
    if lane == "tailscale":
        if local_port in ports:
            counts[f"proxy_ts_{direction}_bytes"] += size
            counts[f"proxy_ts_{direction}_payload_bytes"] += payload
        elif source is None:
            counts["tailscale_unattributed_ip_bytes"] += size
        return
    if local_port in ports:
        kind = "client"
    elif source is not None:
        kind = "upstream"
    else:
        kind = "other"
    counts[f"proxy_{kind}_{direction}_bytes"] += size


def inspect_container(name):
    result = subprocess.run(["docker", "inspect", name], check=True, text=True,
                            capture_output=True, timeout=10)
    info = json.loads(result.stdout)[0]
    if not info["State"]["Running"]:
        raise RuntimeError("The target container is not running")
    return info


def open_captures(pid, tail_interface):
    host_ns = os.open("/proc/self/ns/net", os.O_RDONLY)
    router_ns = os.open(f"/proc/{pid}/ns/net", os.O_RDONLY)
    captures = []

    def add(interface, lane):
        # Protocol 0 prevents delivery until the payload-stripping filter is on.
        sock = socket.socket(socket.AF_PACKET, socket.SOCK_DGRAM, 0)
        try:
            attach_header_filter(sock)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 * 1024 * 1024)
            sock.bind((interface, 0x0003))  # ETH_P_ALL, L3 datagrams
            sock.setblocking(False)
            captures.append((sock, lane, interface))
        except BaseException:
            sock.close()
            raise

    try:
        add(tail_interface, "tailscale")
        os.setns(router_ns, os.CLONE_NEWNET)
        interfaces = [line.split(":", 1)[0].strip()
                      for line in Path("/proc/net/dev").read_text().splitlines()[2:]
                      if line.split(":", 1)[0].strip() != "lo"]
        if not interfaces:
            raise RuntimeError("No non-loopback container interface found")
        for interface in interfaces:
            add(interface, "router")
    except BaseException:
        for sock, _, _ in captures:
            sock.close()
        raise
    finally:
        os.setns(host_ns, os.CLONE_NEWNET)
        os.close(host_ns)
        os.close(router_ns)
    return captures


def interface_counters():
    result = {}
    for line in Path("/proc/net/dev").read_text().splitlines()[2:]:
        name, values = line.split(":", 1)
        fields = list(map(int, values.split()))
        result[name.strip()] = {"rx": fields[0], "tx": fields[8],
                                "rx_drops": fields[3], "tx_drops": fields[11]}
    return result


def host_cpu():
    return list(map(int, Path("/proc/stat").read_text().splitlines()[0].split()[1:9]))


def cgroup_values(root):
    cpu = dict(line.split() for line in (root / "cpu.stat").read_text().splitlines())
    return int(cpu["usage_usec"]), int((root / "memory.current").read_text())


def memory_available():
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemAvailable:"):
            return int(line.split()[1]) / 1024
    return None


def connection_counts(pid, ports):
    client = upstream = 0
    for family in ("tcp", "tcp6"):
        for line in Path(f"/proc/{pid}/net/{family}").read_text().splitlines()[1:]:
            fields = line.split()
            if fields[3] == "01":  # ESTABLISHED sockets, not requests or agents
                if int(fields[1].rsplit(":", 1)[1], 16) in ports:
                    client += 1
                else:
                    upstream += 1
    return client, upstream


def output_file(path, mode="x"):
    stream = path.open(mode, encoding="utf-8", newline="")
    os.fchmod(stream.fileno(), 0o600)
    owner = path.parent.stat()
    if os.geteuid() == 0:
        os.fchown(stream.fileno(), owner.st_uid, owner.st_gid)
    return stream


def write_json(path, data, replace=False):
    # Only monitor-owned files are written; never touch production state.
    target = path.with_name(path.name + ".new") if replace else path
    with output_file(target, "w" if replace else "x") as stream:
        json.dump(data, stream, indent=2)
        stream.write("\n")
    if replace:
        target.replace(path)


def read_phase(path):
    value = json.loads(path.read_text())
    label, agents = value["phase"], value.get("expected_agents")
    if not isinstance(label, str) or len(label) > 100:
        raise ValueError("Invalid phase label")
    if agents is not None and (type(agents) is not int or agents < 0):
        raise ValueError("Invalid expected agent count")
    return label, agents


def snapshot(cgroup):
    cpu, memory = cgroup_values(cgroup)
    return {"time": time.monotonic(), "interfaces": interface_counters(),
            "cpu": cpu, "host_cpu": host_cpu(), "memory": memory}


def run(args):
    if os.geteuid() != 0:
        raise RuntimeError("Run with sudo -n; passive packet sockets require root")
    out = args.output.resolve()
    if not out.is_dir():
        raise RuntimeError("Create the output directory as your normal user first")
    if any((out / name).exists() for name in ("samples.csv", "metadata.json", "summary.json")):
        raise RuntimeError("Output already contains a recording; use a fresh directory")
    info = inspect_container(args.container)
    pid = info["State"]["Pid"]
    identity = Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[19]
    cgroup = Path("/sys/fs/cgroup") / Path(f"/proc/{pid}/cgroup").read_text().strip().split("::", 1)[1].lstrip("/")
    if not args.wan_interface:
        routes = json.loads(subprocess.check_output(["ip", "-json", "route", "show", "default"], text=True))
        args.wan_interface = next(row["dev"] for row in routes if "dev" in row)
    phase_path = out / "phase.json"
    if not phase_path.exists():
        write_json(phase_path, {"phase": args.phase, "expected_agents": args.agents})
    ports = set(args.port or [8317])
    captures = open_captures(pid, args.tailscale_interface)
    selector = selectors.DefaultSelector()
    for sock, lane, _ in captures:
        selector.register(sock, selectors.EVENT_READ, lane)
    start = time.monotonic()
    deadline = start + args.duration if args.duration else math.inf
    counts, totals = collections.Counter(), collections.Counter()
    maximum, phases = {}, {}
    reason, stopped = "duration_complete", False

    def stop(signum, frame):
        nonlocal stopped, reason
        stopped, reason = True, signal.Signals(signum).name

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, stop)
    rate_keys = [f"proxy_{kind}_{direction}_bytes" for kind in
                 ("ts", "client", "upstream", "other") for direction in ("rx", "tx")]
    rate_keys += [f"proxy_ts_{direction}_payload_bytes" for direction in ("rx", "tx")]
    fields = ["timestamp_utc", "elapsed_s", "interval_s", "phase", "expected_agents"]
    rate_fields = {key: key.removesuffix("_bytes") + "_Bps" for key in rate_keys}
    fields += list(rate_fields.values())
    fields += [f"host_{name}_{direction}_Bps" for name in ("wan", "tailscale") for direction in ("rx", "tx")]
    fields += ["proxy_cpu_pct_total", "host_cpu_pct_total", "proxy_memory_MiB", "host_available_MiB",
               "proxy_client_established_tcp", "proxy_upstream_established_tcp",
               "tailscale_capture_drops", "router_capture_drops", "tailscale_unattributed_ip_bytes",
               "tailscale_malformed_packets", "router_malformed_packets", "host_wan_rx_drops", "host_wan_tx_drops"]
    metadata = {"started_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
                "container_id": info["Id"], "container_pid": pid, "image_id": info["Image"],
                "interval_seconds": args.interval, "duration_seconds": args.duration or None,
                "service_ports": sorted(ports), "logical_cpus": os.cpu_count(),
                "wan_interface": args.wan_interface, "tailscale_interface": args.tailscale_interface,
                "capture_interfaces": [{"lane": lane, "interface": interface} for _, lane, interface in captures],
                "definitions": {
                    "proxy_ts": "CLIProxyAPI TCP traffic on tailscale0, selected by local service port; rx=clients to router, tx=router to clients",
                    "proxy_client": "All router TCP client traffic, including LAN and loopback forwarding; overlaps proxy_ts and must not be added to it",
                    "proxy_upstream": "Router non-service TCP traffic, normally provider traffic; also includes background TCP connections",
                    "proxy_other": "Router non-TCP, fragments, and IPv6 extension-header traffic; cannot safely attribute TCP ports",
                    "host": "Whole-host interface counters; WAN includes encrypted Tailscale plus other apps, not router-only internet usage",
                    "bytes": "Packet categories count pre-encapsulation L3 IP bytes including TCP/IP headers and retransmissions, excluding Ethernet and tunnel overhead; segmentation offload can change actual wire overhead",
                    "payload": "Calculated TCP payload length; includes retransmitted data and is not unique HTTP request body size",
                    "cpu": "Percent of all logical CPUs, maximum 100; Docker typically reports percent of one CPU instead",
                    "connections": "Established TCP sockets, not active inference requests or number of agents",
                    "drops": "Packet-socket capture drops; nonzero means attribution counters are incomplete",
                    "privacy": "Kernel BPF strips application payloads. No addresses, prompts, credentials, or packet dumps are saved"}}
    samples = 0
    try:
        write_json(out / "metadata.json", metadata)
        previous = snapshot(cgroup)
        next_tick = previous["time"] + args.interval
        limit = f"for at most {args.duration:g}s" if args.duration else "until explicitly stopped"
        print(f"Recording to {out}/samples.csv every {args.interval:g}s {limit}", flush=True)
        with output_file(out / "samples.csv") as stream:
            writer = csv.DictWriter(stream, fieldnames=fields)
            writer.writeheader()
            stream.flush()
            while not stopped:
                until = min(next_tick, deadline) - time.monotonic()
                for key, _ in selector.select(max(0, min(until, 0.5))):
                    for _ in range(4096):
                        try:
                            packet, address = key.fileobj.recvfrom(256)
                        except BlockingIOError:
                            break
                        direction = "tx" if address[2] == PACKET_OUTGOING else "rx"
                        account_packet(counts, key.data, direction, packet_info(packet), ports)
                now = time.monotonic()
                if now < next_tick and now < deadline and not stopped:
                    continue
                current = snapshot(cgroup)
                elapsed = current["time"] - previous["time"]
                if elapsed <= 0:
                    continue
                current_identity = Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[19]
                if current_identity != identity:
                    raise RuntimeError("Container PID identity changed; refusing to mix recordings")
                phase, agents = read_phase(phase_path)
                row = {"timestamp_utc": dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds"),
                       "elapsed_s": round(current["time"] - start, 3), "interval_s": round(elapsed, 6),
                       "phase": phase, "expected_agents": agents}
                for sock, lane, _ in captures:
                    stats = sock.getsockopt(SOL_PACKET, PACKET_STATISTICS, 12)
                    _, dropped = struct.unpack_from("II", stats)
                    counts[lane + "_capture_drops"] += dropped
                for key, field in rate_fields.items():
                    row[field] = round(counts[key] / elapsed, 3)
                for label, interface in (("wan", args.wan_interface), ("tailscale", args.tailscale_interface)):
                    old, new = previous["interfaces"][interface], current["interfaces"][interface]
                    if any(new[direction] < old[direction] for direction in ("rx", "tx")):
                        raise RuntimeError("Interface counters reset; refusing misleading rates")
                    for direction in ("rx", "tx"):
                        row[f"host_{label}_{direction}_Bps"] = round((new[direction] - old[direction]) / elapsed, 3)
                    if label == "wan":
                        row["host_wan_rx_drops"] = max(0, new["rx_drops"] - old["rx_drops"])
                        row["host_wan_tx_drops"] = max(0, new["tx_drops"] - old["tx_drops"])
                cpu_delta = [b - a for a, b in zip(previous["host_cpu"], current["host_cpu"])]
                total_cpu = sum(cpu_delta)
                row["host_cpu_pct_total"] = round(100 * (total_cpu - cpu_delta[3] - cpu_delta[4]) / max(1, total_cpu), 3)
                row["proxy_cpu_pct_total"] = round((current["cpu"] - previous["cpu"]) / 1e6 / elapsed / os.cpu_count() * 100, 3)
                row["proxy_memory_MiB"] = round(current["memory"] / 1048576, 2)
                row["host_available_MiB"] = memory_available()
                row["proxy_client_established_tcp"], row["proxy_upstream_established_tcp"] = connection_counts(pid, ports)
                for field in fields:
                    if field not in row:
                        row[field] = counts[field]
                writer.writerow(row)
                stream.flush()
                totals.update(counts)
                group = phases.setdefault(phase, {"seconds": 0, "samples": 0, "bytes": collections.Counter(), "expected_agents": agents})
                group["seconds"] += elapsed
                group["samples"] += 1
                group["bytes"].update({key: counts[key] for key in rate_keys})
                for field in list(rate_fields.values()) + ["proxy_cpu_pct_total", "host_cpu_pct_total"]:
                    maximum[field] = max(maximum.get(field, 0), row[field])
                samples += 1
                counts.clear()
                previous = current
                next_tick = current["time"] + args.interval
                if current["time"] >= deadline:
                    break
    except BaseException as exc:
        reason = f"error: {type(exc).__name__}: {exc}"
        raise
    finally:
        for sock, _, _ in captures:
            sock.close()
        selector.close()
        write_json(out / "summary.json", {"stopped_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
                   "stop_reason": reason, "samples": samples, "wall_seconds": round(time.monotonic() - start, 3),
                   "totals": dict(totals), "maximum": maximum, "phases": phases})
        print(f"Stopped: {reason}; {samples} samples; summary: {out}/summary.json", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", default="cli-proxy-api")
    parser.add_argument("--tailscale-interface", default="tailscale0")
    parser.add_argument("--wan-interface")
    parser.add_argument("--port", type=int, action="append", help="Local API port; repeat as needed (default 8317)")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--interval", type=float, default=1)
    parser.add_argument("--duration", type=float, default=4 * 3600,
                        help="Recording seconds; 0 means until explicitly stopped (default 14400)")
    parser.add_argument("--phase", default="baseline")
    parser.add_argument("--agents", type=int, help="Manually supplied label, not a measured agent count")
    parser.add_argument("--mark", help="Change phase of an existing recording; does not require root")
    args = parser.parse_args()
    if not math.isfinite(args.interval) or args.interval < 0.1:
        parser.error("--interval must be finite and at least 0.1 seconds")
    if not math.isfinite(args.duration) or args.duration < 0:
        parser.error("--duration must be finite and nonnegative; use 0 for no time limit")
    if args.agents is not None and args.agents < 0:
        parser.error("--agents must be nonnegative")
    if args.port and any(not 1 <= p <= 65535 for p in args.port):
        parser.error("--port must be between 1 and 65535")
    if args.mark is not None:
        if len(args.mark) > 100:
            parser.error("--mark must be at most 100 characters")
        phase = args.output / "phase.json"
        if not phase.is_file():
            parser.error("No existing phase.json in output directory")
        write_json(phase, {"phase": args.mark, "expected_agents": args.agents}, replace=True)
        print(f"Marked phase {args.mark!r}, expected agents={args.agents}")
        return
    try:
        run(args)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(f"monitor-traffic: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
