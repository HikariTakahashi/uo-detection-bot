import argparse,json,os,sys
from pathlib import Path
parser=argparse.ArgumentParser(description='Synthetic-only real Docker smoke; no Luna or Discord calls')
parser.add_argument('--work',type=Path,required=True);args=parser.parse_args()
root=Path(__file__).resolve().parents[1];work=args.work
work.mkdir(parents=True,exist_ok=True)
if any(work.iterdir()):raise ValueError('new empty work directory required')
sys.path[:0]=[str(root/'services/decoder'),str(root/'experiments/laya/luna_decoder')]
import estimation as e
from adaptive.common import atomic,canonical,read,digest
from pipeline import LUNA_AUTHOR
from sandbox import evaluate
IMAGE='sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b'
CODE="def decode(text):\n    if not text.startswith('rr:'): return None\n    return text[3:][::-1] or None\n"
BAD="def decode(text):\n    return 'うお'\n"
TIMEOUT="def decode(text):\n    while True: pass\n"
RULES={**e.DEFAULT_POLICY,'synthetic_enabled':True}
rows=[]
def run(name,codes=None,unable=False,verify=False):
 state=work/name;q=e.Queue(state);status=q.enqueue('rr:tac','a'*64,'b'*64,source='synthetic',explicit=True)
 calls=[];execution=[]
 def transport(path,method='GET',value=None):
  calls.append((path,method))
  if path=='/v1/connection':return {'send_enabled':True}
  if method=='POST':return {'status':'PENDING'}
  return {'status':'REPLIED','request_id':path.rsplit('/',1)[1],'reply_author_id':LUNA_AUTHOR,'original_id':'synthetic-original','reply_id':'synthetic-reply','sent_at':1,'reply_at':2,'deadline_at':3,'reply':json.dumps({'status':'unable' if unable else 'candidates','candidates':[] if unable else [{'method':'synthetic reverse hypothesis','evidence':'requires independent gold','code':code,'conditions':'rr:','uncertainty':'unverified'} for code in codes]})}
 def execute(code,inputs,image):
  out=evaluate(code,inputs,image,wall_seconds=8)
  assert out['uid']==65534 and not any(k in out['environment_keys'] for k in ['DISCORD_TOKEN','GH_TOKEN','LUNA_RELAY_API_KEY'])
  execution.append({'uid':out['uid'],'values':out['values']})
  return out
 e.advance(state,IMAGE,transport,RULES,now=86401,execute=execute)
 job=read(q.path(status['id']))
 if codes and TIMEOUT in codes:assert not any(result['executed'] for result in job['execution'])
 assert job['state']==('unable' if unable else 'unverified')
 if verify:
  evidence={'schema':1,'request_id':job['id'],'reply_sha256':job['reply_sha256'],'candidate_index':0,'source':{'kind':'independent_encoder','reference':'operator-fixed rr: reversed Unicode encoder; synthetic only','operator_confirmed':True},'selector':{'kind':'prefix','value':'rr:'},'cases':[{'text':'rr:dlrow','expected':'world'},{'text':'rr:案提','expected':'提案'},{'text':'invalid','expected':None}],'public_synthetic_approved':True}
  path=state/'evidence.json';atomic(path,evidence)
  out=e.verify_candidate(state,job['id'],path,IMAGE,trusted_owner=os.getuid())
  assert out['state']==('awaiting_adoption' if codes[0]==CODE else 'unverified')
  assert not (state/'current').exists()
 before=len([c for c in calls if c[1]=='POST'])
 e.advance(state,IMAGE,transport,RULES,now=172801,execute=execute)
 assert before==len([c for c in calls if c[1]=='POST'])==1
 row={'case':name,'passed':True,'state':q.status(job['id'])['state'],'execution':execution,'post_count':before}
 rows.append(row);print(json.dumps({'case':name,'passed':True,'state':row['state']},ensure_ascii=False),flush=True)
run('correct-remains-unverified',[CODE])
run('correct-independent-gold',[CODE],verify=True)
run('wrong-readable-rejected',[BAD],verify=True)
run('three-candidates-still-unverified',[CODE,BAD,"def decode(text):\n    return None\n"])
run('unable',unable=True)
run('bounded-timeout',[TIMEOUT])
(work/'results.json').write_text(json.dumps({'synthetic_only':True,'real_luna_requests':0,'rows':rows,'all_passed':True},ensure_ascii=False,indent=2)+'\n')
