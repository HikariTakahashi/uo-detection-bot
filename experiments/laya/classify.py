#!/usr/bin/env python3
"""Standalone, offline Laya trial. No Discord client or production state access."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import sys
import time

ROOT = Path(__file__).resolve().parent
MODEL_ID = "convaiinnovations/laya-multilingual"
REVISION = "1720e3e3357cfe1e281542e223f8273b0890ca34"
MODEL_DIR = ROOT / "models" / REVISION
# Conservative URL masking for this experiment, not a full Discord Markdown parser.
URL = re.compile(r"(?:https?://|www\.)[^\s<>\"『』「」【】、。！？]+", re.IGNORECASE)
LABELS = {"A": "mockery", "B": "other", "C": "uncertain"}
DEFAULT_PROMPT = "baseline"
PROMPTS = json.loads((ROOT / "prompts.json").read_text(encoding="utf-8"))


def prepare(text, context=""):
    if not isinstance(text, str) or not isinstance(context, str):
        raise ValueError("text/context は文字列で指定してください")
    if not text.strip():
        raise ValueError("空の投稿は判定できません")
    if len(text) + len(context) > 2000:
        raise ValueError("この試験では本文と返信元の合計を2000文字以内にしてください")
    # Use a space, never concatenate text from either side of a URL.
    cleaned = URL.sub(" ", text).strip()
    return {"投稿": cleaned, "返信元": URL.sub(" ", context).strip()}


class Classifier:
    def __init__(self, prompt_name=DEFAULT_PROMPT):
        self.agent = None
        self.load_seconds = None
        self.set_prompt(prompt_name)

    def set_prompt(self, prompt_name):
        self.prompt_name = prompt_name
        self.questions = json.loads(json.dumps(PROMPTS[prompt_name]))
        self.prompt_sha256 = hashlib.sha256(
            json.dumps(self.questions, ensure_ascii=False, sort_keys=True).encode()
        ).hexdigest()

    def load(self):
        if self.agent is not None:
            return
        if not (MODEL_DIR / "model.safetensors").is_file():
            raise RuntimeError("モデル未取得です。download_model.py を実行してください")
        print("Laya MultilingualをCPUに読み込み中…", file=sys.stderr, flush=True)
        started = time.perf_counter()
        import torch
        import laya

        torch.set_num_threads(2)
        torch.set_num_interop_threads(1)
        self.agent = laya.load(str(MODEL_DIR), device="cpu", backend="eager")
        self.load_seconds = round(time.perf_counter() - started, 3)
        print(f"読み込み完了: {self.load_seconds}秒。通知・カウントへの接続はありません。",
              file=sys.stderr, flush=True)

    def classify(self, text, context=""):
        state = prepare(text, context)
        base = {"text": text, "context": context, "cleaned_text": state["投稿"],
                "model": MODEL_ID, "revision": REVISION, "experimental": True,
                "prompt": self.prompt_name, "prompt_sha256": self.prompt_sha256}
        if not state["投稿"]:
            return {**base, "label": "other", "source": "url_filter",
                    "seconds": 0, "probabilities": None}
        self.load()
        started = time.perf_counter()
        result = self.agent.predict(state, self.questions, max_len=1024, head_max_len=256)
        elapsed = round(time.perf_counter() - started, 3)
        answer = result["answers"]["classification"]
        choice = answer.get("choice")
        if choice not in LABELS:
            raise RuntimeError("Layaから未知の選択肢が返りました")
        usage = result.get("usage", {})
        truncated = bool(usage.get("truncated") or usage.get("truncated_questions")
                         or usage.get("state_tokens_dropped"))
        return {**base, "label": "uncertain" if truncated else LABELS[choice],
                "model_label": LABELS[choice], "source": "laya", "seconds": elapsed,
                "load_seconds": self.load_seconds, "truncated": truncated,
                "answer": answer, "usage": usage,
                "note": "確率・confidenceは未校正で、正答率を保証しません。"}


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def main():
    parser = argparse.ArgumentParser(description="Botと独立したLaya冷笑分類の試験")
    source = parser.add_mutually_exclusive_group()
    source.add_argument("--text", help="判定する投稿")
    source.add_argument("--batch", type=Path, help="text/contextを持つJSONLファイル")
    source.add_argument("--examples", action="store_true", help="付属の合成例文を判定")
    source.add_argument("--evaluate", type=Path, help="正解付きJSONLで指示文を比較")
    parser.add_argument("--prompt", choices=list(PROMPTS), default=DEFAULT_PROMPT,
                        help="判定の指示文（旧版はbaseline）")
    parser.add_argument("--compare", nargs="+", choices=list(PROMPTS),
                        help="--evaluateで比較する指示文の名前")
    parser.add_argument("--context", default="", help="任意の返信元。--textと併用")
    args = parser.parse_args()
    if args.context and args.text is None:
        parser.error("--context は --text と併用してください")
    if args.compare and not args.evaluate:
        parser.error("--compare は --evaluate と併用してください")
    classifier = Classifier(args.prompt)
    if args.evaluate:
        from evaluate import evaluate
        evaluate(classifier, args.evaluate, args.compare or [args.prompt])
    elif args.text is not None:
        emit(classifier.classify(args.text, args.context))
    elif args.batch or args.examples:
        path = args.batch or ROOT / "examples.jsonl"
        with path.open(encoding="utf-8") as f:
            for number, line in enumerate(f, 1):
                if not line.strip():
                    continue
                row = json.loads(line)
                result = classifier.classify(row["text"], row.get("context", ""))
                emit({**result, "case": row.get("id", number),
                      "reference_label": row.get("reference_label")})
        metrics = {"peak_rss_mib": round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 1),
                   "address_space_limit_bytes": resource.getrlimit(resource.RLIMIT_AS)[0]}
        status_path = Path("/proc/self/status")
        if status_path.exists():
            metrics["memory_status"] = [line for line in status_path.read_text().splitlines()
                                        if line.startswith(("VmPeak:", "VmRSS:", "VmSwap:"))]
        print(json.dumps(metrics, ensure_ascii=False), file=sys.stderr)
    else:
        print("試験専用: 文章を1行ずつ入力。終了は /quit または Ctrl-D。入力は保存しません。",
              file=sys.stderr, flush=True)
        while True:
            try:
                text = input("投稿> ")
            except EOFError:
                break
            if text.strip() == "/quit":
                break
            try:
                result = classifier.classify(text)
                title = {"mockery": "冷笑の候補", "other": "対象外", "uncertain": "判断困難"}
                print(f"判定: {title[result['label']]} / {result['seconds']}秒（試験結果）")
                emit(result)
            except ValueError as error:
                print(str(error), file=sys.stderr)


if __name__ == "__main__":
    # The wrapper sets these too; protect direct CLI calls from unintended downloads.
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    os.environ["HF_HUB_DISABLE_IMPLICIT_TOKEN"] = "1"
    os.environ["USE_TF"] = "0"
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
    except Exception as error:
        print(f"判定を中止しました: {type(error).__name__}: {error}", file=sys.stderr)
        sys.exit(1)
