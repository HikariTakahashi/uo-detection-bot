import http.client
import json
import threading
import unittest

from classify import Classifier
from serve_shadow import ShadowServer


class FakeClassifier:
    def __init__(self):
        self.calls = []
        self.prompt = "baseline"

    def set_prompt(self, prompt):
        self.prompt = prompt

    def classify(self, text, context, *target_words):
        self.calls.append((text, context, *target_words))
        return {"text": text, "label": "other", "model": "fake", "revision": "rev",
                "prompt": self.prompt, "prompt_sha256": "a" * 64, "seconds": 0.1,
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

    def test_laughter_has_a_fixed_prompt_and_does_not_change_baseline(self):
        value = {"text": "ＵＯｗｗ！", "context": "返信元", "target_words": "uo"}
        self.assertEqual(self.request(json.dumps(value))[0], 400)
        status, data = self.request(json.dumps(value), path="/classify/laughter")
        self.assertEqual((status, data["prompt"]), (200, "laughter"))
        value.pop("target_words")
        self.assertEqual(self.request(json.dumps(value), path="/classify/laughter")[0], 400)
        status, data = self.request(json.dumps(value))
        self.assertEqual((status, data["prompt"]), (200, "baseline"))
        self.assertEqual(self.classifier.calls, [("ＵＯｗｗ！", "返信元", "uo"), ("ＵＯｗｗ！", "返信元")])

    def test_laughter_filter_produces_valid_other_response_without_ai(self):
        self.server.classifier = Classifier()
        status, data = self.request(json.dumps({"text": "うおｗｗの意味を説明します。", "target_words": "うお"}),
                                    path="/classify/laughter")
        self.assertEqual((status, data["label"], data["prompt"]), (200, "other", "laughter"))
        self.assertEqual(data["probabilities"], {"A": 0.0, "B": 1.0, "C": 0.0})
        self.assertIsNone(self.server.classifier.agent)

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
