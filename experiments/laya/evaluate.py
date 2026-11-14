"""Evaluate fixed synthetic examples. Reference labels never go to the model."""
import hashlib
import json
import statistics
import sys

LABELS = ("mockery", "other", "uncertain")


def summarize(rows):
    confusion = {gold: {pred: 0 for pred in LABELS} for gold in LABELS}
    for row in rows:
        confusion[row["reference_label"]][row["label"]] += 1
    f1s = []
    for label in LABELS:
        tp = confusion[label][label]
        actual = sum(confusion[label].values())
        predicted = sum(confusion[gold][label] for gold in LABELS)
        f1s.append(2 * tp / (actual + predicted) if actual + predicted else 0)
    hits = confusion["mockery"]["mockery"]
    positives = sum(confusion["mockery"].values())
    predicted_positive = sum(confusion[gold]["mockery"] for gold in LABELS)
    model_rows = [row for row in rows if row["source"] == "laya"]
    return {
        "total": len(rows), "correct": sum(confusion[label][label] for label in LABELS),
        "model_total": len(model_rows),
        "model_correct": sum(r["reference_label"] == r["label"] for r in model_rows),
        "false_positives": predicted_positive - hits,
        "mockery_hits": hits, "mockery_total": positives,
        "mockery_precision": hits / predicted_positive if predicted_positive else None,
        "mockery_recall": hits / positives if positives else None,
        "abstained": sum(confusion[gold]["uncertain"] for gold in LABELS),
        "macro_f1": sum(f1s) / len(LABELS),
        "median_seconds": statistics.median(r["seconds"] for r in model_rows) if model_rows else 0,
        "truncated": sum(bool(r.get("truncated")) for r in rows),
        "confusion": confusion,
    }


def evaluate(classifier, dataset, prompts):
    raw = dataset.read_bytes()
    cases = [json.loads(line) for line in raw.decode("utf-8").splitlines() if line.strip()]
    ids = [row["id"] for row in cases]
    if not cases or len(ids) != len(set(ids)):
        raise ValueError("評価例が空、またはidが重複しています")
    if any(row.get("reference_label") not in LABELS for row in cases):
        raise ValueError("評価例の正解ラベルが不正です")
    dataset_hash = hashlib.sha256(raw).hexdigest()
    for prompt in prompts:
        classifier.set_prompt(prompt)
        rows = []
        print(f"比較中: {prompt} / {len(cases)}件", file=sys.stderr, flush=True)
        for case in cases:
            # Never pass the id, reference_label, or other evaluation metadata as state.
            result = classifier.classify(case["text"], case.get("context", ""))
            row = {**result, "record_type": "case", "case": case["id"],
                   "reference_label": case["reference_label"], "dataset_sha256": dataset_hash}
            rows.append(row)
            print(json.dumps(row, ensure_ascii=False), flush=True)
        summary = {"record_type": "summary", "prompt": prompt,
                   "prompt_sha256": classifier.prompt_sha256,
                   "dataset_sha256": dataset_hash, **summarize(rows)}
        print(json.dumps(summary, ensure_ascii=False), flush=True)
        print(json.dumps(summary, ensure_ascii=False), file=sys.stderr, flush=True)
