import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'experiments/laya')]
import synthetic
import generate
import runtime
import autopublish
from adaptive.common import atomic,canonical,digest,read
from adaptive.engine import explore

POLICY={'schema':1,'synthetic_recipes':['x9'],'publish_validated':True,'repository':autopublish.REPO,'daily_jobs':3}


def x9_reference(text):
    # Independent inverse lookup, not Luna's decoder or the encoder's arithmetic inverse.
    if not isinstance(text,str) or not text.startswith('x9:'):return None
    data=text[3:]
    if not data or len(data)%2 or any(c not in '0123456789ABCDEF' for c in data):return None
    table={f'{(73*b+41)%256:02X}':b for b in range(256)}
    try:return bytes(table[data[i:i+2]] for i in range(0,len(data),2)).decode('utf-8')
    except (ValueError,UnicodeError):return None


class SyntheticTests(unittest.TestCase):
    def test_x9_oracle_and_no_shared_gold(self):
        t=synthetic.build('x9')
        for c in t['cases']:self.assertEqual(x9_reference(c['text']),c['expected'],c)
        self.assertEqual(synthetic.encode('x9','うおw'),'x9:E4F25FE4F28318')
        self.assertFalse(set(t['samples'])&{c['text'] for c in t['cases']})
        self.assertNotIn('うお',synthetic.prompt(t));self.assertLess(len(synthetic.prompt(t)),1701)
        self.assertLess(len(json.dumps([c['text'] for c in t['cases']]).encode()),16384)
    def test_automatic_templates_ignore_observed_body(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);t=synthetic.build('x9')
            atomic(state/'discovery'/(t['family']+'.json'),{'family':t['family'],'selector':t['selector'],'state':'awaiting_public_samples','text':'PRIVATE_BODY_DO_NOT_SHARE'})
            folder=synthetic.prepare(state,['x9'])
            self.assertEqual(read(folder/(t['family']+'.json')),t)
            self.assertNotIn('PRIVATE_BODY_DO_NOT_SHARE',(folder/(t['family']+'.json')).read_text())
            self.assertEqual(synthetic.prepare(state,['x9']),folder)
            changed={**t,'samples':['evil']};atomic(folder/(t['family']+'.json'),changed)
            with self.assertRaises(ValueError):synthetic.prepare(state,['x9'])
    def test_unsupported_selector_does_not_invent_gold(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);sel={'kind':'prefix','value':'unknown:'};family='discovered-'+digest(canonical(sel))[:16]
            atomic(state/'discovery'/(family+'.json'),{'family':family,'selector':sel,'state':'awaiting_public_samples'})
            folder=synthetic.prepare(state,list(synthetic.SPECS))
            self.assertEqual(list(folder.glob('*.json')),[])
            self.assertEqual(read(state/'preparation'/(family+'.json'))['state'],'needs_format_spec')
    def test_x9_does_not_claim_bare_hex_or_other_wrappers(self):
        for s in ('E4F25FE4F28318','other:E4F25FE4F28318','X9:E4F25FE4F28318'):
            self.assertIsNone(synthetic.explicit_candidate(s))
        self.assertEqual(synthetic.explicit_candidate('x9:E4F25FE4F28318')['selector'],{'kind':'prefix','value':'x9:'})
    def test_every_recipe_is_bounded_and_reproducible(self):
        for name in synthetic.SPECS:
            t=synthetic.build(name);self.assertEqual(t,synthetic.build(name))
            self.assertLessEqual(len(t['cases']),64);self.assertLessEqual(len(synthetic.prompt(t)),1700)
            self.assertNotIn('@',synthetic.prompt(t))
    def test_explicit_wrapper_precedes_generic_shape(self):
        decoders=[{'id':'x','family':'x9','selector':{'kind':'prefix','value':'x9:'},'code':'x9'},
                  {'id':'h','family':'hex','selector':{'kind':'shape','value':'hex-upper-token'},'code':'hex'}]
        seen=[]
        def execute(code,inputs,*args,**kwargs):
            seen.append(code);return {'values':[{'value':x9_reference(inputs[0]),'error':None}]}
        g=explore('x9:E4F25FE4F28318',decoders,'image',execute=execute)
        self.assertEqual((g['status'],g['text']),('terminal','うおw'));self.assertEqual(seen,['x9'])
    def test_inner_x9_discovery(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=runtime.Runtime(Path(tmp)/'current','image',predict=lambda *args:{'suspected':True})
            class Empty:
                def active(self):return []
            r.snapshot=lambda:('a'*64,Empty())
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'x9:E4F25FE4F28318'}):
                out=r.analyze({'text':'outer encoded text','revision':'b'*64,'words':['うお']})
            t=synthetic.build('x9');self.assertEqual(out['families'],[t['family']])
            self.assertEqual(read(Path(tmp)/'discovery'/(t['family']+'.json'))['selector'],t['selector'])


class AutomaticGenerationTests(unittest.TestCase):
    def test_preparation_submit_once_and_daily_budget_persists(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);templates=state/'manual';templates.mkdir();t=synthetic.build('x9')
            atomic(state/'discovery'/(t['family']+'.json'),{'family':t['family'],'selector':t['selector'],'state':'awaiting_public_samples'})
            calls=[]
            def transport(path,method='GET',value=None):
                calls.append((path,method,value))
                if path=='/v1/connection':return {'send_enabled':True}
                if method=='POST':raise TimeoutError('ambiguous')
                return {'status':'PENDING'}
            generate.advance(state,templates,'image',transport,automatic=POLICY,now=86401)
            generate.advance(state,templates,'image',transport,automatic=POLICY,now=172801)
            self.assertEqual(sum(m=='POST' for _,m,_ in calls),1)
            job=read(state/'jobs'/(t['family']+'.json'));self.assertEqual(job['submitted_at'],86401)
            self.assertIn('73*b+41',next(v['prompt'] for _,m,v in calls if m=='POST'))
    def test_failed_reply_no_automatic_resubmit(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);t=synthetic.build('x9')
            atomic(state/'discovery'/(t['family']+'.json'),{'family':t['family'],'selector':t['selector'],'state':'awaiting_public_samples'})
            def transport(path,method='GET',value=None):return {'send_enabled':True} if path=='/v1/connection' else {'status':'ERROR'}
            generate.advance(state,state/'manual','image',transport,automatic=POLICY)
            generate.advance(state,state/'manual','image',lambda *a:self.fail('terminal retry'),automatic=POLICY)
            self.assertEqual(read(state/'jobs'/(t['family']+'.json'))['state'],'error')


class PublicationRecoveryTests(unittest.TestCase):
    def test_commit_push_interruption_recovers_same_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo=root/'repo';repo.mkdir();state=root/'state';state.mkdir()
            def git(*a):return subprocess.run(['git','-C',str(repo),*a],capture_output=True,text=True,check=True).stdout.strip()
            git('init','-b','main');git('config','user.name','test');git('config','user.email','test@example.invalid')
            (repo/'index.json').write_text('{}');git('add','.');git('commit','-m','base');base=git('rev-parse','HEAD')
            (repo/'index.json').write_text('{"change":true}')
            plan={'base_commit':base,'title':'Add validated decoder test','files':{'index.json':digest((repo/'index.json').read_bytes())}}
            atomic(state/'jobs/test.json',{'state':'awaiting_publication_approval'})
            tx={'family':'test','state':'prepared','plan':plan};pushes=[]
            original=autopublish.git
            def wrapped(checkout,*args):
                if args[0]=='push':
                    pushes.append(args[2]);
                    if len(pushes)==1:raise TimeoutError('after remote acceptance')
                    return ''
                return original(checkout,*args)
            def receipt(checkout,commit):return {'repository':autopublish.REPO,'commit':commit,'index_sha256':'b'*64}
            with patch.object(autopublish,'git',wrapped),patch.object(autopublish,'receipt',receipt):
                with self.assertRaises(TimeoutError):autopublish.finish(state,repo,tx)
                first=git('rev-parse','HEAD');autopublish.finish(state,repo,tx)
                self.assertEqual(git('rev-parse','HEAD'),first)
            self.assertEqual(pushes,[first+':refs/heads/main']*2)
            self.assertEqual(read(state/'jobs/test.json')['state'],'published')
    def test_pending_dm_prevents_candidate_replacement(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);old={'repository':autopublish.REPO,'commit':'a'*40,'index_sha256':'b'*64};new={**old,'commit':'c'*40}
            atomic(state/'current/active-production.json',old);atomic(state/'publication-pushed.json',new)
            shared=state/'shared';self.assertFalse(autopublish.ready_for_next(state,shared))
            rid='a'*32;atomic(shared/'requests/current.json',{'id':rid});atomic(shared/'requests'/(rid+'.json'),{'approval':new})
            atomic(shared/'results'/(rid+'.json'),{'state':'pending'});self.assertFalse(autopublish.ready_for_next(state,shared))
            atomic(shared/'results'/(rid+'.json'),{'state':'rejected'});self.assertTrue(autopublish.ready_for_next(state,shared))
    def test_disabled_policy_never_touches_git(self):
        with patch.object(autopublish,'ensure_checkout',side_effect=AssertionError('no git')):
            autopublish.advance('/missing','/missing','/missing','image',{**POLICY,'publish_validated':False})

if __name__=='__main__':unittest.main()
