"""Inference must prepare independent decoders, never count a guessed plaintext."""
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya/luna_decoder')]
import synthetic
import runtime
from adaptive.common import atomic,read,digest,canonical
from adaptive.engine import explore

class Registry:
    def __init__(self,root,recipes=()):
        self.path=Path(root);self.records=[]
        for name in recipes:
            t=synthetic.build(name);sha=digest(name)
            self.records.append({'family':t['family'],'id':sha,'selector':t['selector'],'code':name})
            atomic(self.path/'bundles'/sha/'verification.json',{'cases':t['cases']})
    def active(self):return self.records

def execute(recipe,inputs,*args,**kwargs):
    return {'values':[{'value':synthetic.reference_decode(recipe,text),'error':None} for text in inputs]}

class InferenceTests(unittest.TestCase):
    def test_two_hex_hypotheses_unique_and_ambiguous(self):
        self.assertEqual(synthetic.hex_hypotheses('E4F25FE4F28318'),{'x9-unwrapped':'うおw'})
        self.assertEqual(synthetic.reference_decode('hex-or-x9','E38186E3818A77'),'うおw')
        self.assertEqual(synthetic.hex_hypotheses('41414141'),{'hex':'AAAA','x9-unwrapped':'XXXX'})
        self.assertIsNone(synthetic.reference_decode('hex-or-x9','41414141'))
        self.assertEqual(synthetic.infer_chain('41414141'),{'state':'ambiguous_candidates','plans':[]})
    def test_mixed_layers_are_inferred_in_order_without_observations(self):
        body='uo、それはないよww'
        text=synthetic.encode('base64',synthetic.encode('percent',synthetic.encode('hex-or-x9',body)))
        result=synthetic.infer_chain(text)
        self.assertEqual(result['state'],'terminal')
        self.assertEqual([p['recipe'] for p in result['plans']],['base64','percent','hex-or-x9'])
        self.assertNotIn(body,json.dumps(result,ensure_ascii=False));self.assertNotIn(text,json.dumps(result))
        self.assertEqual(synthetic.infer_chain(text,max_depth=2)['state'],'budget')
    def test_no_spec_invention_or_label_dependent_preference(self):
        for text in ['hello world','ordinary:!??!!??!','https://example.org','41414141']:
            self.assertEqual(synthetic.infer_chain(text)['plans'],[])
        for body in ['うおw','うお！ありがとう！','「うおw」の意味','ordinary message']:
            text=synthetic.encode('hex-or-x9',body)
            self.assertEqual(synthetic.infer_chain(text)['plans'][0]['recipe'],'hex-or-x9')
    def test_contract_uses_both_forward_encoders_and_rejection_gold(self):
        t=synthetic.build('hex-or-x9')
        self.assertEqual(len(t['cases']),40)
        for case in t['cases']:
            self.assertEqual(synthetic.reference_decode('hex-or-x9',case['text']),case['expected'])
        self.assertIn({'text':'41414141','expected':None},t['cases'])
        self.assertEqual(synthetic.template_recipe(t),'hex-or-x9')
        self.assertNotIn('うお',synthetic.prompt(t))
        self.assertLessEqual(len(synthetic.prompt(t)),1700)
    def make_runtime(self,tmp,recipes=()):
        registry=Registry(Path(tmp)/'registry',recipes);seen=[]
        def predict(route,value):
            seen.append((route,value['text']))
            return {'suspected':False,'has_authored_target':True} if route=='detect' else {'label':'mockery','units':1}
        r=runtime.Runtime(Path(tmp)/'current','image',predict=predict,execute=execute,fixed=lambda text,words:{'candidates':[],'literal_units':0})
        r.snapshot=lambda:('a'*64,registry)
        return r,registry,seen
    def request(self,text):return {'text':text,'revision':'b'*64,'words':['うお','uo']}
    def test_prefixless_requires_validated_dm_adopted_decoder_before_judgment(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,registry,seen=self.make_runtime(tmp)
            out=r.analyze(self.request('E4F25FE4F28318'))
            self.assertTrue(out['unknown']);self.assertEqual(out['units'],0);self.assertEqual(seen,[])
            t=synthetic.build('hex-or-x9');record=read(Path(tmp)/'discovery'/(t['family']+'.json'))
            self.assertEqual(record['recipe'],'hex-or-x9');self.assertNotIn('E4F25FE4F28318',json.dumps(record))
            folder=synthetic.prepare(tmp,synthetic.AUTOMATIC_RECIPES)
            self.assertEqual(read(folder/(t['family']+'.json')),t)
            r,registry,seen=self.make_runtime(tmp,['hex-or-x9'])
            out=r.analyze(self.request('E4F25FE4F28318'))
            self.assertEqual((out['unknown'],out['label'],out['units']),(False,'mockery',1))
            self.assertEqual(seen[-1],('judge','うおw'))
    def test_historical_hex_contract_is_not_candidate_approval(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,registry,seen=self.make_runtime(tmp,['hex'])
            t=synthetic.build('hex-or-x9')
            atomic(Path(tmp)/'discovery'/(t['family']+'.json'),{'family':t['family'],'selector':t['selector'],'state':'known_rejection'})
            result=r.analyze(self.request('E4F25FE4F28318'))
            self.assertTrue(result['unknown']);self.assertEqual(seen,[])
            self.assertEqual(read(Path(tmp)/'discovery'/(t['family']+'.json'))['recipe'],'hex-or-x9')
    def test_multi_layer_missing_inner_then_adoption(self):
        with tempfile.TemporaryDirectory() as tmp:
            text=synthetic.encode('base64',synthetic.encode('percent',synthetic.encode('hex-or-x9','うおw')))
            r,_,seen=self.make_runtime(tmp,['base64'])
            out=r.analyze(self.request(text))
            self.assertEqual(len(out['families']),2);self.assertEqual(seen,[])
            r,_,seen=self.make_runtime(tmp,['base64','percent','hex-or-x9'])
            out=r.analyze(self.request(text));self.assertEqual((out['unknown'],out['units']),(False,1))
            self.assertEqual(seen[-1],('judge','うおw'))
    def test_ambiguous_adopted_decoder_does_not_count(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,_,seen=self.make_runtime(tmp,['hex-or-x9'])
            out=r.analyze(self.request('41414141'))
            self.assertTrue(out['unknown']);self.assertEqual(out['units'],0)
            self.assertFalse(any(route=='judge' for route,_ in seen))
    def test_existing_decoder_frontier_is_discovered(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,_,seen=self.make_runtime(tmp)
            with patch.object(runtime,'explore',return_value={'status':'terminal','text':'E4F25FE4F28318','chain':['legacy']}):
                out=r.analyze(self.request('unrecognized outer encoding'))
            self.assertEqual(out['families'],[synthetic.build('hex-or-x9')['family']]);self.assertEqual(seen,[])

if __name__=='__main__':unittest.main()
