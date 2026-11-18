#!/usr/bin/env python3
"""Synthetic Pi acceptance; requires an ISOLATED copy of the adopted registry.

No Discord clients, credentials, counts or adoption calls. Writes discoveries
only next to the supplied isolated state, and calls the local Laya classifier.
"""
import argparse
import base64
import json
from pathlib import Path
import sys
import time

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'services/decoder'))
import runtime
import synthetic

def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--output',type=Path,required=True);a=p.parse_args()
    state=a.state.resolve(strict=True)
    # Refuse known live paths, including symlinks into them.
    if state==Path('/var/lib/laya-decoder') or Path('/var/lib/laya-decoder') in state.parents:
        raise ValueError('isolated state copy required')
    observed=[]
    def predict(route,value):
        answer=runtime.inference(route,value);observed.append({'route':route,'text':value['text'],'answer':answer});return answer
    r=runtime.Runtime(state,'sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b',predict=predict)
    percent=lambda s:''.join('%%%02X'%b for b in s.encode())
    bare='E4F25FE4F28318'
    cases=[('initial-bare',bare,False,'mockery',1),
           ('decoder-reuse',bare,False,'mockery',1),
           ('fixed-hex','e38186e3818a',False,'mockery',1),
           ('fixed-rotation','f1c043f1c045',False,'mockery',1),
           ('fixed-repetition','うおうお'.encode().hex(),False,'mockery',2),
           ('fixed-then-approved',base64.a85encode(bare.encode()).decode(),False,'mockery',1),
           ('approved-then-fixed',synthetic.encode('hex-or-x9','%E3%81%86%E3%81%8Aw'),False,'mockery',1),
           ('interleaved',base64.b64encode(percent(bare).encode()).decode(),False,'mockery',1),
           ('ambiguous','41414141',True,'uncertain',0),
           ('unknown-inner',percent('unseen:!??!!??!'),True,'uncertain',0),
           ('restored-quote',synthetic.encode('hex-or-x9','「うおw」の意味'),False,'other',0)]
    rows=[]
    for name,text,unknown,label,units in cases:
        observed.clear();start=time.monotonic()
        result=r.analyze({'text':text,'revision':'b'*64,'words':['うお','uo']})
        passed=(result['unknown'],result['label'],result['units'])==(unknown,label,units)
        rows.append({'case':name,'input':text,'result':result,'laya':list(observed),'passed':passed,'seconds':round(time.monotonic()-start,3)})
        a.output.write_text(json.dumps({'rows':rows,'all_passed':all(row['passed'] for row in rows)},ensure_ascii=False,indent=2)+'\n')
        print(json.dumps({'case':name,'passed':passed,'result':result},ensure_ascii=False),flush=True)
    if not all(row['passed'] for row in rows):raise SystemExit(1)

if __name__=='__main__':main()
