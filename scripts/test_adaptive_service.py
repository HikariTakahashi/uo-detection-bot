import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya'),str(ROOT/'experiments/laya/luna_decoder')]
import runtime
import generate
import release
import publication
import production_policy as policy
from adaptive.discovery import shape,dispatch,candidate
from adaptive.common import atomic,digest,canonical

class FakeAgent:
    def __init__(self,choices,blocked=False):self.choices=iter(choices);self.blocked=blocked;self.states=[]
    def predict(self,state,questions,**kwargs):
        self.states.append(state);key=next(iter(questions))
        return {'answers':{key:{'choice':next(self.choices)}},'usage':{'truncated':self.blocked}}
class FakeClassifier:
    def __init__(self,choices,blocked=False):self.agent=FakeAgent(choices,blocked)
class FakeRegistry:
    def __init__(self,records):self.records=records
    def active(self):return self.records

class ProductionPolicyTests(unittest.TestCase):
    def test_large_vocabulary_keeps_actual_targets_and_small_prompt(self):
        words=[f'未使用語{i}' for i in range(270)]+['うお','uo','ＵＯ','どわー']
        c=FakeClassifier(['A','B'])
        self.assertEqual(policy.judge(c,'ＵＯww。どわー、ありがとう！',words),{'label':'mockery','units':1})
        self.assertEqual(c.agent.states[0]['対象ワード'],'uo')
        self.assertNotIn('未使用語',json.dumps(c.agent.states,ensure_ascii=False))
        self.assertLess(len(json.dumps(c.agent.states)),500)
    def test_long_detection_is_bounded(self):
        c=FakeClassifier(['A']);self.assertTrue(policy.detect(c,'01'*900)['suspected'])
        self.assertLess(len(json.dumps(c.agent.states[0])),300)
    def test_truncation_never_positive(self):
        self.assertFalse(policy.detect(FakeClassifier(['A'],True),'123456789')['suspected'])
        self.assertEqual(policy.judge(FakeClassifier(['A'],True),'うおw',['うお']),{'label':'uncertain','units':0})
    def test_quotes_and_explanations_not_counted(self):
        for text in ['「うおw」を引用','うおwの意味','https://example.com/うおw']:
            self.assertEqual(policy.judge(FakeClassifier([]),text,['うお']),{'label':'other','units':0})
    def test_configured_words_and_mixed_clauses(self):
        self.assertEqual(policy.judge(FakeClassifier(['A','B']),'うおw。どわー、よくできたね！',['うお','どわー']),{'label':'mockery','units':1})
        self.assertEqual(policy.judge(FakeClassifier(['A']),'独自語w',['独自語']),{'label':'mockery','units':1})
    def test_partial_uncertainty_not_counted(self):
        self.assertEqual(policy.judge(FakeClassifier(['A','C']),'うおw。どわー。',['うお','どわー']),{'label':'uncertain','units':0})

class RuntimeTests(unittest.TestCase):
    def test_large_vocabulary_unknown_and_restored_target(self):
        words=[f'未使用語{i}' for i in range(270)]+['うお']
        with tempfile.TemporaryDirectory() as tmp:
            seen=[]
            def predict(route,value):
                seen.append(value['words'])
                return policy.detect(FakeClassifier(['A']),value['text'],value['words']) if route=='detect' else policy.judge(FakeClassifier(['A']),value['text'],value['words'])
            r=self.setup_runtime(tmp,[],predict)
            out=r.analyze({'text':'x9:E4F25FE4F28318','revision':'b'*64,'words':words})
            self.assertTrue(out['unknown']);self.assertEqual(out['units'],0)
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'うおw','chain':['decoder']}):
                out=r.analyze({'text':'encoded','revision':'b'*64,'words':words})
            self.assertEqual((out['label'],out['units']),('mockery',1))
            self.assertTrue(all(v==words for v in seen))
            for invalid in (["語"]*1025,["語"*81],[None]):
                with self.assertRaises(ValueError):r.analyze({'text':'text','revision':'b'*64,'words':invalid})
    def setup_runtime(self,path,records,predict):
        r=runtime.Runtime(Path(path)/'current','sha256:'+'0'*64,predict=predict,fixed=lambda text,words:{'candidates':[],'literal_units':0})
        r.snapshot=lambda:('a'*64,FakeRegistry(records))
        return r
    def test_short_padded_base64_dispatch(self):
        self.assertEqual(shape('dW93dw=='),'ascii-alnum-token')
        self.assertEqual(dispatch({'selector':{'kind':'shape','value':'ascii-alnum-token'}},'dW93dw=='),'dW93dw==')
    def test_restore_precedes_detector_even_for_long_inputs(self):
        with tempfile.TemporaryDirectory() as tmp:
            seen=[]
            def predict(route,value):
                seen.append((route,value['text']))
                return {'suspected':False,'technical_hold':False} if route=='detect' else {'label':'mockery','units':1}
            r=self.setup_runtime(tmp,[],predict)
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'うおw','chain':['x']}):
                result=r.analyze({'text':'0'*1800,'revision':'b'*64,'words':['うお']})
            self.assertEqual(seen[0],('detect','うおw'));self.assertEqual(result['units'],1)
    def test_readable_target_is_judged_after_detector_false_positive(self):
        with tempfile.TemporaryDirectory() as tmp:
            def predict(route,value):
                return {'suspected':True,'technical_hold':False,'has_authored_target':True} if route=='detect' else {'label':'mockery','units':1}
            r=self.setup_runtime(tmp,[],predict)
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'どわーw また言い訳ですか。','chain':[]}):
                out=r.analyze({'text':'どわーw また言い訳ですか。','revision':'b'*64,'words':['どわー']})
            self.assertFalse(out['unknown']);self.assertEqual(out['units'],1)
    def test_decoded_control_character_is_held(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=self.setup_runtime(tmp,[],lambda *args:self.fail('control character reached model'))
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'\x00うお','chain':[]}):
                self.assertTrue(r.analyze({'text':'AAAA','revision':'b'*64,'words':['うお']})['unknown'])
    def test_known_rejection_not_generation(self):
        with tempfile.TemporaryDirectory() as tmp:
            sel={'kind':'shape','value':'ascii-alnum-token'}
            r=self.setup_runtime(tmp,[{'selector':sel}],lambda *args:{'suspected':True,'technical_hold':False})
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'eHh4eHh4','chain':[]}):
                result=r.analyze({'text':'eHh4eHh4','revision':'b'*64,'words':['うお']})
            self.assertTrue(result['unknown'])
            record=json.loads(next((Path(tmp)/'discovery').glob('*.json')).read_text())
            self.assertEqual(record['state'],'known_rejection');self.assertNotIn('text',record)
    def test_unknown_spool_contains_no_real_content(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=self.setup_runtime(tmp,[],lambda *args:{'suspected':True,'technical_hold':False})
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'private:abcxyz','chain':[]}):
                r.analyze({'text':'private:abcxyz','revision':'b'*64,'words':['うお']})
            raw=next((Path(tmp)/'discovery').glob('*.json')).read_text()
            self.assertNotIn('abcxyz',raw)
    def test_ambiguous_not_judged(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=self.setup_runtime(tmp,[],lambda *args:self.fail('must not judge'))
            with patch.object(runtime,'explore',return_value={'status':'ambiguous'}):
                self.assertTrue(r.analyze({'text':'encoded','revision':'b'*64,'words':['うお']})['unknown'])

class GenerationTests(unittest.TestCase):
    def setup_template(self,tmp):
        root=Path(tmp);state=root/'state';templates=root/'templates';templates.mkdir()
        sel={'kind':'shape','value':'ascii-alnum-token'};family='discovered-'+digest(canonical(sel))[:16]
        atomic(state/'discovery'/(family+'.json'),{'family':family,'selector':sel,'state':'awaiting_public_samples'})
        atomic(templates/(family+'.json'),{'family':family,'selector':sel,'samples':['dGVzdA=='],'cases':[{'text':'eA==','expected':'x'},{'text':'!','expected':None}],'public_synthetic_approved':True})
        return state,templates,family
    def test_ambiguous_post_only_get_on_next_tick(self):
        with tempfile.TemporaryDirectory() as tmp:
            state,templates,family=self.setup_template(tmp);calls=[]
            def transport(path,method='GET',value=None):
                calls.append((path,method,value))
                if path=='/v1/connection':return {'send_enabled':True}
                if method=='POST':raise TimeoutError()
                return {'status':'PENDING'}
            generate.advance(state,templates,'sha256:'+'0'*64,transport)
            generate.advance(state,templates,'sha256:'+'0'*64,transport)
            self.assertEqual(sum(m=='POST' for _,m,_ in calls),1)
            job=json.loads((state/'jobs'/(family+'.json')).read_text());self.assertEqual(job['state'],'submitted')
    def test_missing_template_never_sends(self):
        with tempfile.TemporaryDirectory() as tmp:
            state,templates,family=self.setup_template(tmp);(templates/(family+'.json')).unlink()
            generate.advance(state,templates,'sha256:'+'0'*64,lambda *args:self.fail('unauthorized send'))
    def test_terminal_error_not_retried(self):
        with tempfile.TemporaryDirectory() as tmp:
            state,templates,family=self.setup_template(tmp)
            def transport(path,*args):
                return {'send_enabled':True} if path=='/v1/connection' else {'status':'ERROR'}
            generate.advance(state,templates,'sha256:'+'0'*64,transport)
            generate.advance(state,templates,'sha256:'+'0'*64,lambda *args:self.fail('terminal retry'))

class PublicationTests(unittest.TestCase):
    def test_preserves_leading_space_in_git_porcelain(self):
        import subprocess
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp);(path/'index.json').write_text('{}')
            with patch.object(publication.subprocess,'run',return_value=subprocess.CompletedProcess([],0,' M index.json\n','')):
                self.assertEqual(publication.paths(path),{'index.json':digest(b'{}')})
    def test_drift_blocks_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);plan={'base_commit':'a'*40,'files':{'index.json':'b'*64},'title':'Test','repository':publication.REPO}
            atomic(state/'publication-plan.json',plan)
            calls=[]
            def fake_git(*args):calls.append(args);return 'a'*40
            with patch.object(publication,'git',fake_git),patch.object(publication,'paths',return_value={'index.json':'c'*64}):
                with self.assertRaises(ValueError):publication.publish(state,state,plan)
            self.assertFalse(any('commit' in c for c in calls))

class ReleaseTests(unittest.TestCase):
    def test_failed_verification_keeps_pointer(self):
        with tempfile.TemporaryDirectory() as tmp:
            state=Path(tmp);old=state/'old';old.mkdir();(state/'current').symlink_to(old)
            with patch.object(release,'sync',side_effect=ValueError('bad checksum')):
                with self.assertRaises(ValueError):release.activate(state,{'repository':release.REPOSITORY,'commit':'a'*40,'index_sha256':'b'*64},'sha256:'+'0'*64)
            self.assertEqual((state/'current').resolve(),old.resolve())

if __name__=='__main__':unittest.main()
