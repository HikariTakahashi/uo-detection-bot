import unittest

from classify import Classifier, prepare


class IsolationTests(unittest.TestCase):
    def test_url_only_never_loads_model(self):
        classifier = Classifier()
        result = classifier.classify("https://example.com/uo")
        self.assertEqual(result["label"], "other")
        self.assertIsNone(classifier.agent)

    def test_url_preserves_following_japanese_sentence(self):
        self.assertEqual(prepare("https://example.com/uo。うお！")["投稿"], "。うお！")

    def test_url_mask_does_not_join_words(self):
        self.assertEqual(prepare("う https://example.com/uo お")["投稿"], "う   お")

    def test_long_input_is_rejected(self):
        with self.assertRaises(ValueError):
            prepare("あ" * 2001)

    def test_truncated_result_is_not_a_positive_decision(self):
        class FakeAgent:
            def predict(self, *args, **kwargs):
                return {"answers": {"classification": {"choice": "A"}},
                        "usage": {"truncated": True}}
        classifier = Classifier()
        classifier.agent = FakeAgent()
        result = classifier.classify("うお")
        self.assertEqual(result["label"], "uncertain")
        self.assertEqual(result["model_label"], "mockery")

    def test_laughter_sends_post_and_configured_candidate_to_the_model(self):
        case = self

        class FakeAgent:
            def predict(self, state, questions, **kwargs):
                case.assertEqual(state, {"投稿": "ＵＯｗｗ！", "返信元": "参考", "対象ワード": "uo"})
                case.assertIn("うおｗｗ → A", questions["classification"]["criteria"]["A"])
                return {"answers": {"classification": {"choice": "A"}}, "usage": {}}

        classifier = Classifier("laughter")
        classifier.agent = FakeAgent()
        self.assertEqual(classifier.classify("https://example.com/ignored ＵＯｗｗ！", "参考", "uo")["label"], "mockery")

    def test_laughter_excludes_quotes_and_explanations_before_inference(self):
        classifier = Classifier("laughter")
        for text in ("彼は「うおw」と書いた。", "『うおw』は検知されますか？",
                     "> うおｗｗ\nこの書き方は何ですか？", ">>> 例\nうおw", "`うおw` のことです",
                     "```txt\nうおw\n```", "うおｗｗの意味を説明します。", "うおｗｗって何？",
                     "どわーｗを説明します。", "うおｗはカウントされる？",
                     "文中にあるちゅううおwうと表記されていた", "本文の候補なし"):
            with self.subTest(text=text):
                result = classifier.classify(text, "返信元にはうおw", "うお、どわー")
                self.assertEqual((result["label"], result["source"]), ("other", "laughter_filter"))
                self.assertIsNone(classifier.agent)

    def test_laughter_keeps_independent_uses_after_mentions(self):
        states = []

        class FakeAgent:
            def predict(self, state, *args, **kwargs):
                states.append(state)
                return {"answers": {"classification": {"choice": "A"}}, "usage": {}}

        classifier = Classifier("laughter")
        classifier.agent = FakeAgent()
        text = "『うおw』は例。うおｗｗの意味を説明します。どわーｗｗ また同じミスか"
        self.assertEqual(classifier.classify(text, "", "うお、どわー")["label"], "mockery")
        self.assertEqual(states[0]["対象ワード"], "どわー")
        self.assertIn("どわーｗｗ また同じミスか", states[0]["投稿"])
        self.assertNotIn("うお", states[0]["投稿"])

    def test_prompt_switch_preserves_input_and_reuses_model(self):
        class FakeAgent:
            def __init__(self):
                self.calls = []

            def predict(self, state, questions, **kwargs):
                self.calls.append((state, questions, kwargs))
                return {"answers": {"classification": {"choice": "B"}}, "usage": {}}

        classifier = Classifier("baseline")
        agent = FakeAgent()
        classifier.agent = agent
        first = classifier.classify("うお！", "返信元")
        classifier.set_prompt("en_rules")
        second = classifier.classify("うお！", "返信元")
        self.assertIs(classifier.agent, agent)
        self.assertEqual(agent.calls[0][0], agent.calls[1][0])
        self.assertEqual(agent.calls[0][2], agent.calls[1][2])
        self.assertNotEqual(first["prompt_sha256"], second["prompt_sha256"])


if __name__ == "__main__":
    unittest.main()
