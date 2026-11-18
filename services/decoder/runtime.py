#!/usr/bin/env python3
"""Pi-only restoration service. IDs, Discord credentials and counters stay in Go."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT=Path(__file__).resolve().parents[2]
sys.path[:0]=[str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'experiments/laya')]
from adaptive.common import atomic, canonical, digest, read
from adaptive.discovery import candidate, dispatch
from adaptive.engine import explore
from adaptive.registry import Registry


def inference(route, value):
    request=urllib.request.Request('http://127.0.0.1:8766/adaptive/'+route,
        json.dumps(value,ensure_ascii=False).encode(),{'Content-Type':'application/json'})
    with urllib.request.urlopen(request,timeout=45) as response:
        raw=response.read(65537)
        if len(raw)>65536:raise ValueError('inference response bounds')
        return json.loads(raw)


class Runtime:
    def __init__(self,state,image,predict=inference,execute=None):
        self.state=Path(state);self.image=image;self.predict=predict;self.execute=execute
        self.policy=digest((ROOT/'experiments/laya/production_policy.py').read_bytes())

    def snapshot(self):
        snapshot=self.state.resolve(strict=True)
        registry=Registry(snapshot/'installed',self.image)
        approval=read(snapshot/'active-production.json')
        if approval['registry_revision']!=registry.revision():raise ValueError('unapproved registry')
        return digest(canonical([approval['commit'],approval['index_sha256'],registry.revision(),self.policy])),registry

    def release(self):
        return self.snapshot()[0]

    def analyze(self,value):
        if set(value)!={'text','revision','words'}:raise ValueError('invalid fields')
        text,revision,words=value['text'],value['revision'],value['words']
        if not isinstance(text,str) or not 1<=len(text)<=2000 or not re.fullmatch('[a-f0-9]{64}',revision):raise ValueError('input bounds')
        if not isinstance(words,list) or not 1<=len(words)<=64 or any(not isinstance(w,str) or not 1<=len(w)<=80 for w in words):raise ValueError('word bounds')
        release,registry=self.snapshot();records=registry.active()
        # Always try installed decoders FIRST. A model context limit cannot block
        # a deterministic restoration already available in the approved registry.
        kwargs={} if self.execute is None else {'execute':self.execute}
        graph=explore(text,records,self.image,**kwargs)
        result={'revision':revision,'release':release,'unknown':False,'label':'uncertain','units':0,'families':[]}
        if graph['status']!='terminal':
            result['unknown']=True
            return result
        body=graph['text']
        if any(ord(c)<32 and c not in '\n\t' for c in body):
            result['unknown']=True;return result
        detection=self.predict('detect',{'text':body,'words':words})
        c=candidate(body,detection['suspected'])
        matched=[r for r in records if dispatch(r,body) is not None]
        unresolved=bool(c or ((detection['suspected'] or detection.get('technical_hold')) and not detection.get('has_authored_target',False)))
        if unresolved:
            result['unknown']=True
            if c:
                result['families']=[c['family']]
                # Never send a real message or its reversible encoding to Luna.
                # Discovery persists only the selector. Public synthetic samples
                # and independent contracts are approved separately by operators.
                path=self.state.parent/'discovery'/(c['family']+'.json')
                if not path.exists():
                    atomic(path,{'family':c['family'],'selector':c['selector'],
                         'state':'known_rejection' if matched else 'awaiting_public_samples',
                         'release':release})
            return result
        judgment=self.predict('judge',{'text':body,'words':words})
        if judgment.get('label') not in ('mockery','other','uncertain') or type(judgment.get('units')) is not int or not 0<=judgment['units']<=2000:raise ValueError('invalid judgment')
        if judgment['label']!='mockery' and judgment['units']!=0:raise ValueError('invalid units')
        return {**result,**judgment}


class Server(ThreadingHTTPServer):
    daemon_threads=True
    request_queue_size=4
    def __init__(self,address,runtime):
        if address[0]!='127.0.0.1':raise ValueError('loopback only')
        self.runtime=runtime;self.lock=threading.Lock();self.connections=threading.BoundedSemaphore(4)
        super().__init__(address,Handler)
    def process_request(self,request,address):
        if not self.connections.acquire(False):request.close();return
        try:super().process_request(request,address)
        except BaseException:self.connections.release();raise
    def process_request_thread(self,request,address):
        try:super().process_request_thread(request,address)
        finally:self.connections.release()
    def handle_error(self,*args):pass


class Handler(BaseHTTPRequestHandler):
    def setup(self):super().setup();self.request.settimeout(5)
    def log_message(self,*args):pass
    def respond(self,status,value):
        raw=json.dumps(value,ensure_ascii=False).encode()
        self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
    def do_GET(self):
        if self.path!='/health':self.respond(404,{});return
        try:self.respond(200,{'status':'ready','release':self.server.runtime.release()})
        except Exception:self.respond(503,{'status':'not_ready'})
    def do_POST(self):
        if self.path!='/analyze':self.respond(404,{});return
        if not self.server.lock.acquire(False):self.respond(503,{'error':'busy'});return
        try:
            lengths=self.headers.get_all('Content-Length',[])
            if self.headers.get('Transfer-Encoding') or self.headers.get('Content-Type')!='application/json' or len(lengths)!=1:raise ValueError()
            length=int(lengths[0])
            if not 0<length<=32768:raise ValueError()
            raw=self.rfile.read(length)
            if len(raw)!=length:raise ValueError()
            self.respond(200,self.server.runtime.analyze(json.loads(raw)))
        except ValueError:self.respond(400,{'error':'invalid_request_or_result'})
        except Exception:self.respond(503,{'error':'temporarily_unavailable'})
        finally:self.server.lock.release()


def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--image',required=True);a=p.parse_args()
    with Server(('127.0.0.1',8767),Runtime(a.state,a.image)) as server:server.serve_forever()

if __name__=='__main__':main()
