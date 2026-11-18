#!/usr/bin/env python3
"""Bounded Pi Luna worker. Only operator-approved synthetic examples leave Pi."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import sys
import urllib.request
import uuid
ROOT=Path(__file__).resolve().parents[2]
sys.path[:0]=[str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'experiments/laya')]
from adaptive.common import read,atomic,canonical,digest
from adaptive.discovery import blind_prompt,validate_selector
from adaptive.registry import Registry
from pipeline import verify_reply

ORIGIN='https://luna-direct-api.hikarit0407.workers.dev'
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args):raise ValueError('relay redirect forbidden')

def call(path,method='GET',value=None):
    key=(Path(os.environ['CREDENTIALS_DIRECTORY'])/'luna-key').read_text().strip()
    request=urllib.request.Request(ORIGIN+path, None if value is None else json.dumps(value).encode(),
        {'Authorization':'Bearer '+key,'Content-Type':'application/json','Accept':'application/json'},method=method)
    with urllib.request.build_opener(NoRedirect).open(request,timeout=25) as response:
        raw=response.read(131073)
        if len(raw)>131072:raise ValueError('relay response bound')
        return json.loads(raw)


def advance(state,templates,image,transport=call):
    state=Path(state);templates=Path(templates);state.mkdir(parents=True,exist_ok=True)
    with (state/'generation.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        jobs=state/'jobs';jobs.mkdir(exist_ok=True)
        attempted=sum(read(p).get('state')!='queued' for p in jobs.glob('*.json'))
        # Default lifetime budget three. A new explicitly reviewed batch is needed
        # for more; restarts and retries do not reset this limit.
        for discovery in sorted((state/'discovery').glob('*.json')):
            item=read(discovery)
            if item['state']=='known_rejection':continue
            family=item['family'];template=templates/(family+'.json')
            if not template.exists():continue
            t=read(template)
            if set(t)!={'family','selector','samples','cases','public_synthetic_approved'} or t['family']!=family or t['selector']!=item['selector'] or t['public_synthetic_approved'] is not True:raise ValueError('approved public synthetic template required')
            validate_selector(t['selector'])
            if family!='discovered-'+digest(canonical(t['selector']))[:16]:raise ValueError('family selector mismatch')
            if not isinstance(t['samples'],list) or not 1<=len(t['samples'])<=3 or any(not isinstance(v,str) or len(v)>400 for v in t['samples']):raise ValueError('sample bounds')
            cases=t['cases']
            if not isinstance(cases,list) or not 2<=len(cases)<=64 or any(set(c)!={'text','expected'} or not isinstance(c['text'],str) or len(c['text'])>2000 or (c['expected'] is not None and (not isinstance(c['expected'],str) or not 1<=len(c['expected'])<=2000)) for c in cases):raise ValueError('independent cases required')
            if not any(c['expected'] is None for c in cases) or not any(c['expected'] is not None for c in cases):raise ValueError('positive/rejection cases required')
            if any(c['text'] in t['samples'] for c in cases):raise ValueError('validation must be independent from shared samples')
            path=jobs/(family+'.json');sha=digest(canonical(t))
            job=read(path) if path.exists() else {'request_id':'luna-'+str(uuid.uuid4()),'state':'queued','template_sha256':sha}
            if job['template_sha256']!=sha:raise ValueError('submitted template changed')
            if job['state'] not in ('queued','submitted'):continue
            if job['state']=='queued':
                if attempted>=3:continue
                if not transport('/v1/connection').get('send_enabled'):return
                prompt=blind_prompt(t['samples'])
                job.update(state='submitted',prompt_sha256=digest(prompt));atomic(path,job);attempted+=1
                try:transport('/v1/jobs','POST',{'request_id':job['request_id'],'prompt':prompt,'share_approved':True})
                except Exception:continue # Only GET on all subsequent invocations.
            try:reply=transport('/v1/jobs/'+job['request_id'])
            except Exception:continue
            if reply.get('status') in ('ERROR','TIMEOUT'):
                job['state']=reply['status'].lower();atomic(path,job);continue
            if reply.get('status')!='REPLIED':continue
            verify_reply(reply,job['request_id'])
            atomic(state/'replies'/(job['request_id']+'.json'),reply)
            registry=Registry(state/'proposals'/family/'registry',image)
            try:bundle=registry.stage(family,reply,cases,selector=t['selector'])
            except Exception:
                job['state']='validation_failed';atomic(path,job);continue
            contract={'schema':1,'scope':'synthetic-evaluation-only','family':family,'selector':t['selector'],'cases':cases}
            contract_path=state/'proposals'/family/'contract.json';atomic(contract_path,contract)
            job.update(state='awaiting_publication_approval',bundle=bundle,contract_sha256=digest(contract_path.read_bytes()))
            atomic(path,job)


def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--templates',type=Path,required=True);p.add_argument('--image',required=True);a=p.parse_args()
    try:advance(a.state,a.templates,a.image)
    except Exception:print('Generation deferred; inspect local job states (no automatic resubmission).',file=sys.stderr);raise SystemExit(1)

if __name__=='__main__':main()
