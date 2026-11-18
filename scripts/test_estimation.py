import copy
import importlib.util
import json
import os
import shutil
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya/luna_decoder')]
import estimation as e
import generate
import autopublish
from adaptive.common import atomic,canonical,digest,read
from adaptive.estimation_reply import parse_reply
from adaptive.registry import Registry
from pipeline import LUNA_AUTHOR
from pipeline import extract_code

CHANNEL='123456789012345678'
REV='a'*64
RELEASE='b'*64
RULES={**e.DEFAULT_POLICY,'synthetic_enabled':True,'external_enabled':True,'channel_ids':[CHANNEL],'input_scopes':['explicit_selection']}
CODE="def decode(text):\n    if not text.startswith('rr:'): return None\n    return text[3:][::-1] or None\n"
BAD="def decode(text):\n    return 'うお'\n"

def candidate(code=CODE):
 return {'method':'reverse after wrapper','evidence':'positional hypothesis only','code':code,'conditions':'rr: wrapper','uncertainty':'independent gold still required'}

def reply(relay_id,candidates=None,unable=False):
 return {'status':'REPLIED','request_id':relay_id,'reply_author_id':LUNA_AUTHOR,'original_id':'synthetic-original','reply_id':'synthetic-reply','sent_at':1,'reply_at':2,'deadline_at':3,
         'reply':json.dumps({'status':'unable' if unable else 'candidates','candidates':[] if unable else (candidates if candidates is not None else [candidate()])})}

def sandbox(code,inputs,image,**kwargs):
 if code==CODE:
  values=[(''.join(reversed(v[3:])) or None) if v.startswith('rr:') else None for v in inputs]
 elif code==BAD:values=['うお']*len(inputs)
 else:raise ValueError('test sandbox only accepts explicitly fixed synthetic programs')
 return {'values':[{'value':v,'error':None} for v in values]}

class Transport:
 def __init__(self,answer=None,post_error=None):self.calls=[];self.answer=answer;self.post_error=post_error
 def __call__(self,path,method='GET',value=None):
  self.calls.append((path,method,value))
  if path=='/v1/connection':return {'send_enabled':True}
  if method=='POST':
   if self.post_error:raise self.post_error
   return {'status':'PENDING'}
  if self.answer=='unable':return reply(path.rsplit('/',1)[1],unable=True)
  if self.answer=='valid':return reply(path.rsplit('/',1)[1])
  if self.answer=='bad':return reply(path.rsplit('/',1)[1],[candidate(BAD)])
  if self.answer in ('ERROR','TIMEOUT'):return {'status':self.answer}
  return {'status':'PENDING'}
 @property
 def posts(self):return [v for _,m,v in self.calls if m=='POST']

class EstimationTests(unittest.TestCase):
 def queue(self,tmp,text='rr:tac',source='synthetic',explicit=True,**kwargs):
  q=e.Queue(tmp);status=q.enqueue(text,REV,RELEASE,channel_id=CHANNEL,source=source,explicit=explicit,**kwargs)
  return q,status
 def advance(self,tmp,transport,rules=None,now=86401,execute=sandbox):
  e.advance(tmp,'image',transport,RULES if rules is None else rules,now=now,execute=execute)
 def evidence(self,tmp,q,status,source='public_specification'):
  job=read(q.path(status['id']))
  value={'schema':1,'request_id':status['id'],'reply_sha256':job['reply_sha256'],'candidate_index':0,
         'source':{'kind':source,'reference':'operator-fixed synthetic reversal specification','operator_confirmed':True},
         'selector':{'kind':'prefix','value':'rr:'},'cases':[{'text':'rr:dlrow','expected':'world'},{'text':'rr:案提','expected':'提案'},{'text':'invalid','expected':None}],
         'public_synthetic_approved':True}
  path=Path(tmp)/'evidence.json';atomic(path,value);return path,value
 def test_unknown_selectorless_ascii_and_hidden_characters_enter_queue(self):
  for text in ['q7|p2^R9~s4!T6','う\u200bお\u200bw']:
   with self.subTest(text=text),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp,text,explicit=False)
    self.assertIsNotNone(status);self.assertEqual(status['state'],'queued')
    job=read(q.path(status['id']));self.assertNotIn('family',job);self.assertNotIn('selector',job)
    self.assertNotEqual(status['id'],job['relay_id'])
 def test_normals_do_not_enter_automatic_queue(self):
  cases=json.loads((ROOT/'scripts/fixtures/readable-japanese.json').read_text())
  texts=[c['text'] for c in cases]+['こんにちは、よろしくお願いします。','helloWorld','abcdefgh','deadbeef','key:value','status:ok',CHANNEL,'abc-123_','https://example.org/x','家族👨‍👩‍👧‍👦です','今日はう\u200bお\u200bwでした','普通の本文']
  for text in texts:
   with self.subTest(text=text),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp,text,explicit=False);self.assertIsNone(status)
    t=Transport();self.advance(tmp,t);self.assertEqual(t.calls,[])
 def test_reverse_text_requires_explicit_request(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp,'wおう',explicit=False);self.assertIsNone(status)
   _,status=self.queue(tmp,'wおう');self.assertEqual(status['state'],'queued')
 def test_explicit_selection_upgrades_pending_consent_without_new_request(self):
  with tempfile.TemporaryDirectory() as tmp:
   text='q7|p2^R9~s4!T6';q,status=self.queue(tmp,text,source='discord',explicit=False)
   upgraded=q.enqueue(text,REV,RELEASE,CHANNEL,'discord',True,0,len(text))
   self.assertEqual(status['id'],upgraded['id'])
   self.assertEqual(read(q.path(status['id']))['scope'],'explicit_selection')
   t=Transport();self.advance(tmp,t);self.assertEqual(len(t.posts),1)
 def test_input_and_version_deduplicate_across_restart(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,a=self.queue(tmp);old=read(q.path(a['id']))['relay_id']
   b=e.Queue(tmp).enqueue('rr:tac',REV,RELEASE,CHANNEL,'synthetic',True)
   self.assertEqual(a['id'],b['id']);self.assertEqual(read(q.path(b['id']))['relay_id'],old)
   c=q.enqueue('rr:tac',REV,'c'*64,CHANNEL,'synthetic',True)
   self.assertNotEqual(a['id'],c['id'])
 def test_default_policy_sends_nothing_for_real_and_synthetic_inputs(self):
  for source in ['discord','synthetic']:
   with self.subTest(source=source),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp,source=source);t=Transport();self.advance(tmp,t,e.DEFAULT_POLICY)
    self.assertEqual(t.calls,[]);self.assertEqual(q.status(status['id'])['reason'],'external_disabled')
 def test_channel_scope_and_mentions_block_export(self):
  for changes,text in [({'channel_ids':[]},'rr:tac'),({'input_scopes':[]},'rr:tac'),({},'<@123456789012345678> rr:tac'),({},'rr:user@example.invalid')]:
   with self.subTest(changes=changes,text=text),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp,text,source='discord',start=0,end=len(text));t=Transport()
    self.advance(tmp,t,{**RULES,**changes});self.assertEqual(t.calls,[])
    self.assertEqual(q.status(status['id'])['state'],'queued')
 def test_restored_payload_needs_its_own_export_scope(self):
  with tempfile.TemporaryDirectory() as tmp:
   q=e.Queue(tmp);status=q.enqueue('q7|p2^R9~s4!T6',REV,RELEASE,CHANNEL,derived=True)
   t=Transport();self.advance(tmp,t,{**RULES,'input_scopes':['whole_message']});self.assertEqual(t.calls,[])
   self.assertEqual(q.status(status['id'])['reason'],'scope_not_allowed')
   self.advance(tmp,t,{**RULES,'input_scopes':['restored_payload']});self.assertEqual(len(t.posts),1)
 def test_only_explicit_selection_leaves_no_metadata_or_context(self):
  with tempfile.TemporaryDirectory() as tmp:
   text='無関係な説明 <@123456789012345678> 入力: rr:tac 返信元の話'
   start=text.index('rr:tac');q,status=self.queue(tmp,text,source='discord',start=start,end=start+6)
   t=Transport();self.advance(tmp,t)
   self.assertEqual(len(t.posts),1);prompt=t.posts[0]['prompt']
   self.assertIn('rr:tac',prompt)
   for excluded in ['無関係','返信元','123456789012345678','channel_id','author']:
    self.assertNotIn(excluded,prompt)
   job=read(q.path(status['id']));self.assertEqual(job['sample'],'rr:tac');self.assertNotIn(text,canonical(job))
 def test_post_is_persisted_before_network_and_never_reposted_after_timeout(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);t=Transport(post_error=TimeoutError())
   def transport(path,method='GET',value=None):
    if method=='POST':self.assertEqual(read(q.path(status['id']))['state'],'awaiting_reply')
    return t(path,method,value)
   self.advance(tmp,transport);self.advance(tmp,transport,now=172801)
   self.assertEqual(len(t.posts),1);self.assertTrue(any(p.endswith(read(q.path(status['id']))['relay_id']) and m=='GET' for p,m,_ in t.calls))
 def test_crash_during_post_recovers_with_get_only(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);t=Transport(post_error=KeyboardInterrupt())
   with self.assertRaises(KeyboardInterrupt):self.advance(tmp,t)
   t.post_error=None;self.advance(tmp,t)
   self.assertEqual(len(t.posts),1);self.assertEqual(q.status(status['id'])['state'],'awaiting_reply')
 def test_known_and_unknown_requests_share_daily_budget(self):
  with tempfile.TemporaryDirectory() as tmp:
   for i in range(2):atomic(Path(tmp)/'jobs'/f'legacy-{i}.json',{'state':'submitted','submitted_at':86400})
   for i in range(3):self.queue(tmp,f'rr:sample-{i}')
   t=Transport()
   with patch.object(e,'policy',return_value=RULES):
    generate.advance(tmp,Path(tmp)/'templates','image',t,automatic={'synthetic_recipes':['x9'],'daily_jobs':3},now=86401)
    self.assertEqual(len(t.posts),1)
    self.assertEqual(e.attempted(tmp,86401),3)
    generate.advance(tmp,Path(tmp)/'templates','image',t,automatic={'synthetic_recipes':['x9'],'daily_jobs':3},now=172801)
    self.assertEqual(len(t.posts),3)
 def test_connection_failure_does_not_consume_post_budget(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp)
   self.advance(tmp,lambda *a:(_ for _ in ()).throw(OSError()))
   self.assertEqual(q.status(status['id'])['reason'],'connection_failed');self.assertEqual(e.attempted(tmp,86401),0)
 def test_poll_failure_recovers_without_post_and_updates_notice_state(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);t=Transport();self.advance(tmp,t)
   self.advance(tmp,lambda *a:(_ for _ in ()).throw(ConnectionError()))
   self.assertEqual(q.status(status['id'])['reason'],'poll_failed')
   self.advance(tmp,t);self.assertEqual(q.status(status['id'])['reason'],'awaiting_response')
   t.answer='valid';self.advance(tmp,t)
   self.assertEqual(q.status(status['id'])['state'],'unverified');self.assertEqual(len(t.posts),1)
 def test_terminal_transport_failure_and_unable_do_not_retry(self):
  for answer,expected in [('ERROR','failed'),('TIMEOUT','failed'),('unable','unable')]:
   with self.subTest(answer=answer),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp);t=Transport(answer);self.advance(tmp,t)
    self.assertEqual(q.status(status['id'])['state'],expected)
    calls=len(t.calls);self.advance(tmp,t);self.assertEqual(len(t.calls),calls)
 def test_successful_readable_execution_remains_unverified(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp,'rr:おう');t=Transport('valid');self.advance(tmp,t)
   job=read(q.path(status['id']));self.assertTrue(job['execution'][0]['executed'])
   self.assertEqual(job['state'],'unverified');self.assertFalse((Path(tmp)/'jobs').exists())
   self.assertFalse((Path(tmp)/'current').exists())
 def test_wrong_candidate_cannot_pass_independent_gold(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);self.advance(tmp,Transport('bad'));path,value=self.evidence(tmp,q,status)
   with patch.object(e,'evaluate',side_effect=sandbox):out=e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
   self.assertEqual(out['state'],'unverified');self.assertEqual(out['reason'],'independent_validation_failed')
   self.assertFalse((Path(tmp)/'jobs').exists())
 def test_self_examples_round_trip_or_luna_claim_are_not_independent_evidence(self):
  for mutate in ['sample','source','distinct','rejection','reply']:
   with self.subTest(mutate=mutate),tempfile.TemporaryDirectory() as tmp:
    q,status=self.queue(tmp);self.advance(tmp,Transport('valid'));path,value=self.evidence(tmp,q,status)
    if mutate=='sample':value['cases'][0]['text']='rr:tac'
    if mutate=='source':value['source']['kind']='luna_roundtrip'
    if mutate=='distinct':value['cases'][1]['expected']='world'
    if mutate=='rejection':value['cases'][2]['expected']='hello'
    if mutate=='reply':value['reply_sha256']='f'*64
    atomic(path,value)
    with self.assertRaises(ValueError),patch.object(e,'evaluate',side_effect=AssertionError('untrusted evidence executed')):
     e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
 def test_verified_public_candidate_requires_publication_and_adoption(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);self.advance(tmp,Transport('valid'));path,value=self.evidence(tmp,q,status)
   with patch.object(e,'evaluate',side_effect=sandbox),patch('adaptive.registry.evaluate',side_effect=sandbox):
    out=e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
   self.assertEqual(out['state'],'awaiting_adoption')
   job=read(q.path(status['id']));family=job['family'];pub=read(Path(tmp)/'jobs'/(family+'.json'))
   self.assertEqual(pub['state'],'awaiting_publication_approval');self.assertFalse((Path(tmp)/'current').exists())
   manifest,code=Registry(Path(tmp)/'proposals'/family/'registry','image').load(job['bundle'])
   self.assertEqual(code,CODE);self.assertEqual(manifest['candidate_index'],0)
   self.assertEqual(extract_code(manifest['reply']['reply']),CODE)
   self.assertEqual(manifest['reply']['normalization'],'candidate-extraction')
   atomic(Path(tmp)/'current/installed/active.json',{family:'0'*64});self.advance(tmp,Transport());self.assertEqual(q.status(status['id'])['state'],'awaiting_adoption')
   atomic(Path(tmp)/'current/installed/active.json',{family:job['bundle']});self.advance(tmp,Transport());self.assertEqual(q.status(status['id'])['state'],'awaiting_adoption')
   shutil.copytree(Path(tmp)/'proposals'/family/'registry/bundles',Path(tmp)/'current/installed/bundles')
   revision=Registry(Path(tmp)/'current/installed','image').revision()
   atomic(Path(tmp)/'current/active-production.json',{'commit':'c'*40,'index_sha256':'d'*64,'registry_revision':revision})
   self.advance(tmp,Transport());self.assertEqual(q.status(status['id'])['state'],'complete')
 def test_real_input_reply_never_becomes_public_bundle(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp,source='discord',start=0,end=6);self.advance(tmp,Transport('valid'));path,value=self.evidence(tmp,q,status)
   with patch.object(e,'evaluate',side_effect=sandbox):out=e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
   self.assertEqual(out['reason'],'public_synthetic_reproduction_required');self.assertFalse((Path(tmp)/'proposals').exists())
 def test_estimation_proposal_cannot_inherit_automatic_publication_permission(self):
  with tempfile.TemporaryDirectory() as tmp:
   state=Path(tmp);atomic(state/'jobs/family.json',{'state':'awaiting_publication_approval','origin_estimation':'estimate-'+'a'*64})
   atomic(state/'auto-templates/family.json',{'old_template':'exists'})
   with patch.object(autopublish,'ensure_checkout',side_effect=AssertionError('unknown proposal reached automatic git')):
    autopublish.advance(state,state/'checkout',state/'shared','image',{'publish_validated':True})
 def test_private_verification_links_only_to_verified_public_proposal(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,private=self.queue(tmp,'rr:etavirp',source='discord',start=0,end=10)
   self.advance(tmp,Transport('valid'));path,_=self.evidence(tmp,q,private,'confirmed_pairs')
   with patch.object(e,'evaluate',side_effect=sandbox):e.verify_candidate(tmp,private['id'],path,'image',trusted_owner=os.getuid())
   _,public=self.queue(tmp);self.advance(tmp,Transport('valid'));path,_=self.evidence(tmp,q,public)
   with patch.object(e,'evaluate',side_effect=sandbox),patch('adaptive.registry.evaluate',side_effect=sandbox):
    e.verify_candidate(tmp,public['id'],path,'image',trusted_owner=os.getuid())
   family=read(q.path(private['id']))['family']
   with patch.object(e,'evaluate',side_effect=sandbox):out=e.link_public_proposal(tmp,private['id'],family,'image')
   self.assertEqual(out['state'],'awaiting_adoption')
   self.assertNotIn('rr:etavirp',''.join(p.read_text() for p in (Path(tmp)/'proposals').rglob('*.json')))
 def test_partial_promotion_recovers_same_verified_bundle(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp);self.advance(tmp,Transport('valid'));path,_=self.evidence(tmp,q,status)
   original=e.Queue.save
   def fail_checkpoint(queue,job,state=None,reason=None):
    if state=='awaiting_adoption':raise OSError('checkpoint interrupted')
    return original(queue,job,state,reason)
   with patch.object(e,'evaluate',side_effect=sandbox),patch('adaptive.registry.evaluate',side_effect=sandbox):
    with patch.object(e.Queue,'save',fail_checkpoint),self.assertRaises(OSError):e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
    existing=next((Path(tmp)/'jobs').glob('*.json'));bundle=read(existing)['bundle']
    out=e.verify_candidate(tmp,status['id'],path,'image',trusted_owner=os.getuid())
   self.assertEqual(out['state'],'awaiting_adoption');self.assertEqual(read(q.path(status['id']))['bundle'],bundle)
 def test_unsafe_or_excess_candidates_are_rejected(self):
  for candidates in [[candidate()]*4,[candidate('import os\ndef decode(text): return None\n')]]:
   with self.subTest(n=len(candidates)),self.assertRaises(ValueError):parse_reply(json.dumps({'status':'candidates','candidates':candidates}))
  self.assertEqual(len(parse_reply(json.dumps({'status':'candidates','candidates':[candidate()]*3}))),3)
 def test_unverified_reply_identity_never_executes(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp)
   def transport(path,method='GET',value=None):
    if path=='/v1/connection':return {'send_enabled':True}
    if method=='POST':return {'status':'PENDING'}
    return reply('luna-wrong-id')
   self.advance(tmp,transport,execute=lambda *a:self.fail('unverified response executed'))
   self.assertEqual(q.status(status['id'])['state'],'failed')
 def test_policy_permissions_and_selection_validation(self):
  with tempfile.TemporaryDirectory() as tmp:
   path=Path(tmp)/'policy.json';atomic(path,e.DEFAULT_POLICY)
   self.assertEqual(e.trusted_json(path,os.getuid()),e.DEFAULT_POLICY)
   path.chmod(0o666)
   with self.assertRaises(ValueError):e.trusted_json(path,os.getuid())
   for start,end in [(0,None),(-1,4),(0,999),(True,3)]:
    with self.subTest(start=start,end=end),self.assertRaises(ValueError):self.queue(tmp,start=start,end=end)
 def test_queue_capacity_and_oversized_input_never_send(self):
  with tempfile.TemporaryDirectory() as tmp:
   q,status=self.queue(tmp)
   for i in range(999):(q.jobs/(str(i)+'.json')).write_text('{}')
   with self.assertRaises(ValueError):q.enqueue('different',REV,RELEASE,CHANNEL,'synthetic',True)
  with tempfile.TemporaryDirectory() as tmp:
   self.assertIsNone(e.Queue(tmp).enqueue('0123456789ABCDEF'*30,REV,RELEASE,CHANNEL))

if __name__=='__main__':unittest.main()
