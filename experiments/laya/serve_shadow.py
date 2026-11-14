#!/usr/bin/env python3
"""Loopback-only inference adapter. No Discord access and no message persistence."""
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Set before importing any model libraries; requests cannot select models/prompts.
for key, value in {"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
                   "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1", "USE_TF": "0",
                   "OMP_NUM_THREADS": "2", "OPENBLAS_NUM_THREADS": "1"}.items():
    os.environ[key] = value

from classify import Classifier, prepare


class ShadowServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 4

    def __init__(self, address, classifier):
        if address[0] != "127.0.0.1":
            raise ValueError("loopback binding required")
        self.classifier = classifier
        self.inference = threading.Lock()
        self.connections = threading.BoundedSemaphore(4)
        super().__init__(address, Handler)

    def process_request(self, request, client_address):
        if not self.connections.acquire(blocking=False):
            request.close()
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.connections.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.connections.release()

    def handle_error(self, request, client_address):
        # Never log a request body or exception that might include the message.
        pass


class Handler(BaseHTTPRequestHandler):
    server_version = "LayaShadow"

    def setup(self):
        self.request.settimeout(5)
        super().setup()

    def log_message(self, *args):
        pass

    def reply(self, status, value):
        data = json.dumps(value, ensure_ascii=False, allow_nan=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200 if self.path == "/health" else 404,
                   {"status": "ready"} if self.path == "/health" else {"error": "not_found"})

    def do_POST(self):
        if self.path != "/classify":
            self.reply(404, {"error": "not_found"})
            return
        if self.headers.get("Transfer-Encoding") or self.headers.get("Content-Type") != "application/json":
            self.reply(400, {"error": "invalid_request"})
            return
        try:
            lengths = self.headers.get_all("Content-Length", [])
            if len(lengths) != 1:
                raise ValueError()
            length = int(lengths[0])
            if not 0 < length <= 16384:
                raise ValueError()
            data = self.rfile.read(length)
            if len(data) != length:
                raise ValueError()
            value = json.loads(data)
            if not isinstance(value, dict) or set(value) - {"text", "context"}:
                raise ValueError()
            text, context = value.get("text"), value.get("context", "")
            prepare(text, context)
        except (ValueError, TypeError, UnicodeError, TimeoutError):
            self.reply(400, {"error": "invalid_request"})
            return
        if not self.server.inference.acquire(blocking=False):
            self.reply(503, {"error": "busy"})
            return
        try:
            result = self.server.classifier.classify(text, context)
            output = {key: result[key] for key in
                      ("label", "model", "revision", "prompt", "prompt_sha256", "seconds")}
            output["truncated"] = result.get("truncated", False)
            output["probabilities"] = (result.get("answer", {}).get("probabilities")
                                       if result.get("source") != "url_filter"
                                       else {"A": 0.0, "B": 1.0, "C": 0.0})
            self.reply(200, output)
        except Exception:
            self.reply(503, {"error": "inference_failed"})
        finally:
            self.server.inference.release()


def main():
    classifier = Classifier("baseline")
    classifier.load()
    with ShadowServer(("127.0.0.1", 8766), classifier) as server:
        print("Laya shadow ready on 127.0.0.1:8766", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    main()
