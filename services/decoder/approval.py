#!/usr/bin/env python3
"""Pi release approval inbox. Discord secrets remain exclusively with the Go Bot."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import stat
import tempfile
import time
import uuid
import release
from adaptive.common import atomic, canonical, digest, read
from adaptive.delivery import gh, sync
from adaptive.registry import Registry

APPROVER = '845282954636951583'
REPO = release.REPOSITORY
TERMINAL = {'active', 'rejected', 'expired', 'superseded', 'failed'}


def secure_read(path, uid=None):
    path = Path(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > 65536 or info.st_mode & 0o022 or (uid is not None and info.st_uid != uid):
            raise ValueError('untrusted approval file')
        with os.fdopen(fd, 'r') as stream:
            fd = None
            return json.load(stream)
    finally:
        if fd is not None:
            os.close(fd)


def shared_write(path, value):
    # chmod before rename: never expose a partially written or owner-only message.
    path = Path(path)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.pending-')
    try:
        os.fchmod(fd, 0o640)
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, ensure_ascii=False, indent=2)
            stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try: os.fsync(directory)
        finally: os.close(directory)
    finally:
        if os.path.exists(name): os.unlink(name)


def pin(value):
    if set(value) != {'repository', 'commit', 'index_sha256'} or value['repository'] != REPO or not re.fullmatch('[a-f0-9]{40}', value['commit']) or not re.fullmatch('[a-f0-9]{64}', value['index_sha256']):
        raise ValueError('invalid release pin')
    return value


def current(state):
    active = read(Path(state)/'current/active-production.json')
    return {k: active[k] for k in ('repository', 'commit', 'index_sha256')}


def ci_success(candidate):
    # Only the dedicated evaluation workflow at this exact commit is accepted.
    runs = json.loads(gh('api', f'repos/{REPO}/actions/workflows/evaluation.yml/runs?head_sha={candidate["commit"]}&event=push&per_page=100'))
    matches = [r for r in runs['workflow_runs'] if r['head_sha'] == candidate['commit'] and r['head_branch'] == 'main']
    if not matches or max(matches, key=lambda r: r['id'])['conclusion'] != 'success':
        raise ValueError('evaluation CI has not succeeded')


def verify_candidate(state, candidate, image):
    ci_success(candidate)
    with tempfile.TemporaryDirectory(dir=state, prefix='.approval-') as tmp:
        registry = Registry(Path(tmp)/'installed', image)
        receipt = sync(REPO, candidate['commit'], candidate['index_sha256'], registry)
        before = read(Path(state)/'current/installed/active.json')
        families = sorted(f for f, sha in receipt['bundles'].items() if before.get(f) != sha)
        # Installation receipts are produced by fresh local contract execution.
        checks = sum(len(read(p)['execution']['values']) for p in (registry.path/'receipts').glob('*.json'))
        return {'families': families, 'bundles': len(receipt['bundles']), 'checks': checks}


def advance(state, shared, image, now=None, bot_uid=None, verify=verify_candidate, activate=release.activate, retry_request=None):
    state, shared = Path(state), Path(shared)
    now = int(time.time()) if now is None else now
    bot_uid = pwd.getpwnam('discord-bot').pw_uid if bot_uid is None else bot_uid
    with (state/'dm-approval.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        pointer = shared/'requests/current.json'
        request = None
        if pointer.exists():
            rid = secure_read(pointer)['id']
            if not re.fullmatch('[a-f0-9]{32}', rid): raise ValueError('invalid request id')
            request = secure_read(shared/'requests'/(rid+'.json'))
        result_path = shared/'results'/(request['id']+'.json') if request else None
        result = secure_read(result_path) if result_path and result_path.exists() else {'state': 'pending'}
        latest_path = state/'publication-pushed.json'
        candidate = pin(secure_read(latest_path)) if latest_path.exists() else None
        # Once DM mode is enabled, the current snapshot is authoritative. Never
        # replay the bootstrap root pin on each timer tick.
        live = current(state)
        if request and result['state'] not in TERMINAL:
            target = pin(request['approval'])
            decision_path = shared/'decisions'/(request['id']+'.json')
            decision = secure_read(decision_path, bot_uid) if decision_path.exists() else None
            request_hash = hashlib.sha256((shared/'requests'/(request['id']+'.json')).read_bytes()).hexdigest()
            valid = decision and set(decision) == {'request_id','request_sha256','actor','action','decided_at','channel_id','message_id'} and decision['request_id'] == request['id'] and decision['request_sha256'] == request_hash and decision['actor'] == APPROVER and decision['action'] in ('approve','reject') and type(decision['decided_at']) is int and request['created_at'] <= decision['decided_at'] < request['expires_at'] and decision['decided_at'] <= now and all(isinstance(decision.get(k),str) and re.fullmatch('[0-9]{15,22}',decision[k]) for k in ('channel_id','message_id'))
            if decision and not valid: raise ValueError('invalid Discord approval receipt')
            status = None
            if valid and decision['action'] == 'reject': status = 'rejected'
            elif valid and live == target and result['state'] == 'applying': status = 'active'  # crash after symlink commit
            elif candidate != target or live != request['base']: status = 'superseded'
            elif now >= request['expires_at'] and not valid: status = 'expired'
            elif valid:
                shared_write(result_path, {'state':'applying','updated_at':now})
                try:
                    # Recheck CI and artifacts, then compare the base under the
                    # same release lock used by manual deployments.
                    ci_success(target)
                    activate(state, target, image, expected_base=request['base'])
                    status = 'active'
                except (ValueError, FileNotFoundError):
                    status = 'failed'
                except Exception:
                    # Ambiguous interruption may already have switched current.
                    # Persist applying and reconcile before another activation.
                    raise
            if status:
                shared_write(result_path, {'state':status,'updated_at':now})
                result = {'state':status}
                live = current(state)
        if not candidate or candidate == live: return
        same = request and request['approval'] == candidate
        if same and not retry_request: return
        if retry_request and (not request or request['id'] != retry_request or result['state'] not in TERMINAL):
            raise ValueError('retry requires the exact terminal request')
        summary = verify(state, candidate, image)
        if current(state) != live: return  # deployment raced verification; retry next tick
        if request and result['state'] not in TERMINAL:
            shared_write(result_path, {'state':'superseded','updated_at':now})
        rid = uuid.uuid4().hex
        new = {'schema':1,'id':rid,'approver':APPROVER,'approval':candidate,'base':live,'created_at':now,'expires_at':now+7*86400,**summary}
        shared_write(shared/'requests'/(rid+'.json'),new)
        shared_write(shared/'results'/(rid+'.json'),{'state':'pending','updated_at':now})
        shared_write(pointer, {'id':rid})


def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--shared',type=Path,required=True);p.add_argument('--image',required=True);p.add_argument('--retry-request');a=p.parse_args()
    if os.environ.get('CREDENTIALS_DIRECTORY'): release.credential('github-token','GH_TOKEN')
    policy=secure_read('/etc/laya-decoder/dm-approval.json',0)
    if policy != {'enabled':True,'approver':APPROVER,'repository':REPO}: raise ValueError('DM approval policy required')
    advance(a.state,a.shared,a.image,retry_request=a.retry_request)

if __name__=='__main__': main()
