import http.client
import json
import threading
import unittest

from serve_shadow import ShadowServer


class FakeClassifier:
    def __init__(self):
        self.calls = []

    def classify(self, text, context):
        self.calls.append((text, context))
        return {"text": text, "label": "other", "model": "fake", "revision": "rev",
                "prompt": "baseline", "prompt_sha256": "a" * 64, "seconds": 0.1,
                "source": "laya", "answer": {"probabilities": {"A": 0.1, "B": 0.8, "C": 0.1}}}


class ServerTests(unittest.TestCase):
    def setUp(self):
        self.classifier = FakeClassifier()
        self.server = ShadowServer(("127.0.0.1", 0), self.classifier)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, body=None, path="/classify", method="POST", headers=None):
        conn = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        conn.request(method, path, body=body,
                     headers=headers or {"Content-Type": "application/json"})
        response = conn.getresponse()
        status, data = response.status, json.loads(response.read())
        conn.close()
        return status, data

    def test_schema_and_no_body_echo(self):
        status, data = self.request(json.dumps({"text": "うお", "context": "引用"}))
        self.assertEqual(status, 200)
        self.assertEqual(set(data), {"label", "model", "revision", "prompt", "prompt_sha256",
                                    "seconds", "probabilities", "truncated"})
        self.assertEqual(self.classifier.calls, [("うお", "引用")])

    def test_invalid_and_oversize(self):
        for value in ({"text": ""}, {"text": "a" * 2001}, {"text": 42},
                      {"text": "abc", "prompt": "override"}, [], None):
            status, _ = self.request(json.dumps(value))
            self.assertEqual(status, 400)
        self.assertEqual(self.request("x" * 16385)[0], 400)
        self.assertEqual(self.classifier.calls, [])

    def test_busy_and_health(self):
        self.server.inference.acquire()
        try:
            self.assertEqual(self.request('{"text":"test"}')[0], 503)
            self.assertEqual(self.request(path="/health", method="GET"), (200, {"status": "ready"}))
        finally:
            self.server.inference.release()
        self.assertEqual(self.request(path="/else", method="GET")[0], 404)

    def test_refuses_public_bind(self):
        with self.assertRaises(ValueError):
            ShadowServer(("0.0.0.0", 0), self.classifier)

    def test_error_does_not_echo_input(self):
        def fail(*args):
            raise RuntimeError("private input")
        self.classifier.classify = fail
        self.assertEqual(self.request('{"text":"private input"}'),
                         (503, {"error": "inference_failed"}))


if __name__ == "__main__":
    unittest.main()
