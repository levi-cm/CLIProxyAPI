"""Optional real Caddy smoke on the central host, with an offline provider.

CADDY_BIN=/path/to/caddy python3 -m unittest discover -s companion -v
Only the temporary private port 18417 is opened; no service is installed.
"""
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import socket
import subprocess
import threading
import unittest


class OfflineProvider(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.headers.get("Upgrade", "").lower() == "websocket":
            digest = hashlib.sha1((self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", base64.b64encode(digest).decode())
            self.end_headers()
            self.wfile.write(b"\x81\x02OK\x88\x00")
            self.wfile.flush()
            self.close_connection = True
            return
        body = b"data: start\n\ndata: complete\n\n" if self.path == "/v1/sse" else b"fixture OK"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if self.path == "/v1/sse" else "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()


@unittest.skipUnless(os.environ.get("CADDY_BIN"), "Set CADDY_BIN on the central Tailscale host for the real private edge smoke")
class EdgeTests(unittest.TestCase):
    def test_private_operator_gate_and_http_sse_websocket_reconnect(self):
        server = ThreadingHTTPServer(("127.0.0.1", 0), OfflineProvider)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        source = Path(__file__).with_name("Caddyfile").read_text()
        config = source.replace(":8317", ":18417").replace("127.0.0.1:8318", "127.0.0.1:" + str(server.server_port))
        # Test the delivered recipe itself, with only the two ports replaced.
        caddy = subprocess.Popen([os.environ["CADDY_BIN"], "run", "--config", "/dev/stdin", "--adapter", "caddyfile"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            caddy.stdin.write(config)
            caddy.stdin.close()
            for line in caddy.stderr:
                if "serving initial configuration" in line:
                    break
            else:
                self.fail("Caddy did not start; check that the private port is unused and Tailscale is running")

            def request(path, extra=()):
                argv = ["curl", "--noproxy", "*", "-sS", "--resolve", "levi-thinkcentre-m700.dinosaur-dojo.ts.net:18417:100.82.251.30", *extra, "-w", "\n%{http_code}", "http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:18417" + path]
                result = subprocess.run(argv, capture_output=True, text=True, check=True)
                return result.stdout.rsplit("\n", 1)

            self.assertEqual(request("/v1/models"), ["fixture OK", "200"])
            self.assertEqual(request("/v8/management/account-policy/accounts"), ["fixture OK", "200"])
            for path in ("/v8/management/account-policy/accounts", "/v0/management/accounts", "/management.html", "/account-policy.html"):
                self.assertEqual(request(path, ("--interface", "127.0.0.1", "-H", "X-Forwarded-For: 100.82.251.30"))[1], "403", path)
            self.assertEqual(request("/v1/models", ("--interface", "127.0.0.1")), ["fixture OK", "200"])
            self.assertEqual(request("/v1/sse")[0], "data: start\n\ndata: complete\n\n")
            for _ in range(2):
                with socket.create_connection(("100.82.251.30", 18417)) as sock:
                    sock.sendall(b"GET /v1/responses HTTP/1.1\r\nHost: levi-thinkcentre-m700.dinosaur-dojo.ts.net:18417\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
                    with sock.makefile("rb") as stream:
                        self.assertIn(b"101", stream.readline())
                        while stream.readline() != b"\r\n":
                            pass
                        self.assertEqual(stream.read(4), b"\x81\x02OK")
        finally:
            if caddy.poll() is None:
                caddy.terminate()
            caddy.wait()
            caddy.stdout.close()
            caddy.stderr.close()
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
