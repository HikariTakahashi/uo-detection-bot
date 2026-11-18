"""Bounded, data-only Luna estimation responses. No generated code is imported."""
import json
from sandbox import preflight


def parse_reply(raw):
    if not isinstance(raw, str) or len(raw.encode()) > 32000:
        raise ValueError('estimation response bounds')
    if raw.startswith('```json\n') and raw.rstrip().endswith('```'):
        raw = raw[8:raw.rfind('```')]
    value = json.loads(raw)
    if set(value) != {'status', 'candidates'} or value['status'] not in ('candidates', 'unable'):
        raise ValueError('estimation response contract')
    candidates = value['candidates']
    if not isinstance(candidates, list) or len(candidates) > 3:
        raise ValueError('at most three candidates')
    if (value['status'] == 'unable') != (len(candidates) == 0):
        raise ValueError('inconsistent estimation response')
    for candidate in candidates:
        if not isinstance(candidate, dict) or set(candidate) != {'method', 'evidence', 'code', 'conditions', 'uncertainty'}:
            raise ValueError('candidate fields')
        for key in ('method', 'evidence', 'conditions', 'uncertainty'):
            if not isinstance(candidate[key], str) or not 1 <= len(candidate[key]) <= 1000:
                raise ValueError('candidate explanation bounds')
        if not isinstance(candidate['code'], str):
            raise ValueError('candidate code type')
        candidate['code'] = candidate['code'].strip() + '\n'
        preflight(candidate['code'])
    return candidates
