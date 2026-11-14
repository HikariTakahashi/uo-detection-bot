import unittest
from contextlib import redirect_stdout, redirect_stderr
from io import StringIO
import json
from pathlib import Path
import tempfile

from evaluate import evaluate, summarize


class EvaluationTests(unittest.TestCase):
    def test_abstention_is_not_counted_as_success_for_mockery(self):
        pairs = [("mockery", "uncertain"), ("other", "mockery"),
                 ("mockery", "mockery"), ("other", "other"),
                 ("uncertain", "uncertain")]
        rows = [{"reference_label": gold, "label": pred, "source": "laya", "seconds": 1}
                for gold, pred in pairs]
        result = summarize(rows)
        self.assertEqual(result["correct"], 3)
        self.assertEqual(result["false_positives"], 1)
        self.assertEqual(result["abstained"], 2)
        self.assertEqual(result["mockery_precision"], 0.5)
        self.assertEqual(result["mockery_recall"], 0.5)

    def test_no_positive_prediction_does_not_claim_perfect_precision(self):
        result = summarize([{"reference_label": "mockery", "label": "uncertain",
                             "source": "laya", "seconds": 1}])
        self.assertIsNone(result["mockery_precision"])
        self.assertEqual(result["mockery_recall"], 0)

    def test_reference_labels_are_not_sent_to_classifier(self):
        class FakeClassifier:
            prompt_sha256 = "test"

            def set_prompt(self, name):
                pass

            def classify(self, text, context):
                self.received = (text, context)
                return {"label": "other", "source": "laya", "seconds": 0.1}

        classifier = FakeClassifier()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cases.jsonl"
            path.write_text(json.dumps({"id": "secret-evaluation-id", "text": "投稿本文",
                                        "context": "返信元", "reference_label": "mockery"}))
            output = StringIO()
            with redirect_stdout(output), redirect_stderr(StringIO()):
                evaluate(classifier, path, ["baseline"])
            self.assertEqual(classifier.received, ("投稿本文", "返信元"))
            summary = json.loads(output.getvalue().splitlines()[-1])
            self.assertEqual(summary["correct"], 0)


if __name__ == "__main__":
    unittest.main()
