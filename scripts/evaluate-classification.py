#!/usr/bin/env python3
"""Measure fixed labeled cases against an isolated runtime and real local Laya.

Only the supplied state copy receives discoveries. No generation, publishing,
Discord clients, live review records or counters are used.
"""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import sys
import time
import urllib.error

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'services/decoder'))
import runtime


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def outcome(result):
    if result['units'] > 0:
        return 'mockery'
    if result['unknown'] or result['label'] == 'uncertain':
        return 'review'
    return result['label']


def summarize(rows):
    summary = {}
    for category in sorted({row['category'] for row in rows}):
        group = [row for row in rows if row['category'] == category]
        correct = sum(row['passed'] for row in group)
        summary[category] = {
            'n': len(group), 'correct': correct,
            'accuracy': correct / len(group),
            'outcomes': dict(Counter(row['outcome'] for row in group)),
            'false_reviews': sum(row['expected'] == 'other' and row['outcome'] == 'review' for row in group),
            'false_charges': sum(row['expected'] != 'mockery' and row['outcome'] == 'mockery' for row in group),
        }
    return summary


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--cases', type=Path, required=True)
    parser.add_argument('--words', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--image', required=True)
    args = parser.parse_args()
    state = args.state.resolve(strict=True)
    if state == Path('/var/lib/laya-decoder') or Path('/var/lib/laya-decoder') in state.parents:
        raise ValueError('isolated state copy required')
    cases = json.loads(args.cases.read_text())['cases']
    words = json.loads(args.words.read_text())
    if len({case['id'] for case in cases}) != len(cases):
        raise ValueError('duplicate case IDs')
    observed = []

    def predict(route, value):
        deadline = time.monotonic() + 40
        retries = 0
        while True:
            try:
                answer = runtime.inference(route, value)
                observed.append({'route': route, 'text': value['text'], 'answer': answer, 'busy_retries': retries})
                return answer
            except urllib.error.HTTPError as exc:
                body = exc.read()
                if exc.code != 503 or b'busy' not in body or time.monotonic() >= deadline:
                    raise
                retries += 1
                time.sleep(0.3)

    analyzer = runtime.Runtime(state, args.image, predict=predict)
    release, registry = analyzer.snapshot()
    metadata = {
        'case_sha256': sha(args.cases), 'word_sha256': sha(args.words),
        'words': len(words), 'runtime_sha256': sha(ROOT / 'services/decoder/runtime.py'),
        'helper_sha256': sha(analyzer.helper), 'registry_revision': registry.revision(),
        'release': release, 'image': args.image,
        'decoders': [{'family': record['family'], 'selector': record.get('selector'), 'id': record['id']} for record in registry.active()],
        'model': 'convaiinnovations/laya-multilingual',
        'model_revision': '1720e3e3357cfe1e281542e223f8273b0890ca34',
    }
    rows = []
    for index, case in enumerate(cases, 1):
        observed.clear()
        started = time.monotonic()
        row = {**case, 'passed': False, 'outcome': 'error'}
        try:
            result = analyzer.analyze({'text': case['text'], 'revision': 'b' * 64, 'words': words})
            row.update(result=result, outcome=outcome(result))
            row['passed'] = row['outcome'] == case['expected'] and result['units'] == case['expected_units']
        except Exception as exc:
            row['error'] = type(exc).__name__ + ': ' + str(exc)
        row.update(inference=list(observed), seconds=round(time.monotonic() - started, 3))
        rows.append(row)
        report = {'metadata': metadata, 'rows': rows, 'summary': summarize(rows),
                  'complete': len(rows) == len(cases), 'correct': sum(row['passed'] for row in rows), 'n': len(rows)}
        temporary = args.output.with_suffix('.tmp')
        temporary.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        temporary.replace(args.output)
        if index % 10 == 0 or not row['passed']:
            print(json.dumps({'completed': index, 'total': len(cases), 'case': case['id'],
                              'expected': case['expected'], 'actual': row['outcome'], 'passed': row['passed']}, ensure_ascii=False), flush=True)
    print(json.dumps({'correct': report['correct'], 'n': len(rows), 'summary': report['summary']}, ensure_ascii=False), flush=True)


if __name__ == '__main__':
    main()
