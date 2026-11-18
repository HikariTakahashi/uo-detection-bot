#!/usr/bin/env python3
"""Offline admission/egress benchmark. Not a measurement of Luna's guesses.

Real Go fixed transforms + independent decoder oracle + deliberately fake Laya,
Luna and sandbox transport. No network calls, credentials or production state.
"""
import argparse,json,subprocess,sys,tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'scripts')]
import runtime,estimation
from adaptive.common import digest
from test_candidate_inference import Registry,execute

def main():
 p=argparse.ArgumentParser();p.add_argument('--helper',type=Path,required=True);p.add_argument('--output',type=Path,required=True);a=p.parse_args()
 source=ROOT/'experiments/laya/evaluation/readable-vs-unknown-20261118/cases.json'
 cases=json.loads(source.read_text())['cases']
 cases.extend([{'id':'selectorless-ascii-1','category':'unknown_ascii','text':'q7|p2^R9~s4!T6'},
               {'id':'selectorless-ascii-2','category':'unknown_ascii','text':'m8^K3|u7~L2!x9'},
               {'id':'long-decimal-id','category':'ambiguous_id','text':'012345678901234567890123'},
               {'id':'long-hex-id','category':'ambiguous_id','text':'8f0d3a7c9e1b246805acdef123456789'}])
 rows=[]
 with tempfile.TemporaryDirectory() as tmp:
  state=Path(tmp);registry=Registry(state/'registry',['hex-or-x9','base64','base32','percent','unicode','x9'])
  def fixed(text,words):return json.loads(subprocess.run([str(a.helper),'-restore-view'],input=json.dumps({'text':text,'words':words}).encode(),capture_output=True,check=True).stdout)
  def predict(route,value):return {'suspected':True,'has_authored_target':False} if route=='detect' else {'label':'other','units':0}
  r=runtime.Runtime(state/'current','image',predict=predict,execute=execute,fixed=fixed);r.snapshot=lambda:('a'*64,registry)
  for case in cases:
   out=r.analyze({'text':case['text'],'revision':digest(case['id']),'words':['うお','uo','どわー'],'channel_id':'123456789012345678'})
   row={'id':case['id'],'category':case['category'],'automatic_queued':'estimation' in out,'legacy_or_estimation_hold':out['unknown']}
   if case['category']=='unknown_mockery':
    explicit=r.estimate({'text':case['text'],'revision':digest(case['id']),'channel_id':'123456789012345678','start':0,'end':len(case['text'])})
    row['explicit_queued']=explicit['state']=='queued'
   rows.append(row)
  calls=[]
  def transport(*args):calls.append(args);raise AssertionError('default policy must never access relay')
  estimation.advance(state,'image',transport,estimation.DEFAULT_POLICY)
  summary={}
  for category in sorted({row['category'] for row in rows}):
   group=[row for row in rows if row['category']==category]
   summary[category]={'n':len(group),'automatic_queued':sum(row['automatic_queued'] for row in group),'explicit_queued':sum(row.get('explicit_queued',False) for row in group)}
  value={'scope':'Queue admission and default-disabled egress only; fake sentiment and Luna, no inference-accuracy claim.',
         'source_cases_sha256':digest(source.read_bytes()),'helper_sha256':digest(a.helper.read_bytes()),'runtime_sha256':digest((ROOT/'services/decoder/runtime.py').read_bytes()),'estimation_sha256':digest((ROOT/'services/decoder/estimation.py').read_bytes()),
         'n':len(rows),'relay_calls':len(calls),'summary':summary,'rows':rows}
  a.output.parent.mkdir(parents=True,exist_ok=True);a.output.write_text(json.dumps(value,ensure_ascii=False,indent=2)+'\n')
  assert len(calls)==0
  print(json.dumps({key:value[key] for key in ['n','relay_calls','summary']},ensure_ascii=False,indent=2))
if __name__=='__main__':main()
