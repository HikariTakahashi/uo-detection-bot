"""Isolated composition tests: real Go transforms, independent decoder oracle.

Generated Python is never imported here. On-device acceptance additionally uses
the approved registry's Docker executor and the real Laya service.
"""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT/'services/decoder'),str(ROOT/'experiments/laya/luna_decoder')]
import runtime
import synthetic
from adaptive.engine import explore
from test_candidate_inference import Registry, execute


class UnifiedDetectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build=tempfile.TemporaryDirectory()
        cls.helper=Path(os.environ.get('UO_FIXED_HELPER',str(Path(cls.build.name)/'bot')))
        if 'UO_FIXED_HELPER' not in os.environ:
            subprocess.run(['go','build','-o',str(cls.helper),'./cmd/discord-bot'],cwd=ROOT,check=True)

    @classmethod
    def tearDownClass(cls):cls.build.cleanup()

    def fixed(self,text,words):
        return json.loads(subprocess.run([str(self.helper),'-restore-view'],input=json.dumps({'text':text,'words':words}).encode(),capture_output=True,check=True).stdout)

    def make(self,tmp,recipes=('hex-or-x9','base64')):
        seen=[]
        def predict(route,value):
            seen.append((route,value['text']))
            return {'suspected':False,'has_authored_target':True} if route=='detect' else {'label':'mockery','units':1}
        registry=Registry(Path(tmp)/'registry',recipes)
        r=runtime.Runtime(Path(tmp)/'current','image',predict=predict,execute=execute,fixed=self.fixed)
        r.snapshot=lambda:('a'*64,registry)
        return r,seen

    def request(self,text):return {'text':text,'revision':'b'*64,'words':['うお','uo']}

    def test_reusable_adopted_decoder_passes_restored_body_to_laya(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp)
            for _ in range(2):
                out=r.analyze(self.request('E4F25FE4F28318'))
                self.assertEqual((out['unknown'],out['label'],out['units']),(False,'mockery',1))
            self.assertEqual([body for route,body in seen if route=='judge'],['うおw']*2)
            self.assertFalse((Path(tmp)/'discovery').exists())

    def test_fixed_exact_and_repetition_skip_model(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp,())
            for text,units in [('e38186e3818a',1),('f1c043f1c045',1),('c7030dc70315',1),
                               ('%E3%81%86%E3%81%8A',1),(r'\u3046\u304a',1),
                               ('j+2sFJVj',1),('うおうお'.encode().hex(),2)]:
                with self.subTest(text=text):
                    out=r.analyze(self.request(text))
                    self.assertEqual((out['unknown'],out['label'],out['units']),(False,'mockery',units))
            self.assertEqual(seen,[])

    def test_interleaved_fixed_and_adopted_layers(self):
        inner='E4F25FE4F28318'
        percent=lambda text:''.join('%%%02X'%b for b in text.encode())
        cases=[percent(inner),base64.a85encode(inner.encode()).decode(),
               base64.b64encode(percent(inner).encode()).decode(),
               synthetic.encode('hex-or-x9','%E3%81%86%E3%81%8Aw')]
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp)
            for text in cases:
                with self.subTest(text=text):
                    out=r.analyze(self.request(text))
                    self.assertEqual((out['unknown'],out['units']),(False,1))
                    self.assertEqual(seen[-1],('judge','うおw'))

    def test_competing_fixed_and_adopted_outputs_hold(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp)
            r.execute=lambda *args,**kwargs:{'values':[{'value':'uo','error':None}]}
            out=r.analyze(self.request('E38186E3818A')) # fixed -> うお, decoder -> uo
            self.assertTrue(out['unknown']);self.assertEqual(out['units'],0);self.assertEqual(seen,[])

    def test_ambiguous_and_unrestored_inner_wait_then_adopt(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp,())
            text=''.join('%%%02X'%b for b in b'E4F25FE4F28318')
            result=r.analyze(self.request(text))
            self.assertTrue(result['unknown']);self.assertEqual(result['families'],[synthetic.build('hex-or-x9')['family']])
            self.assertEqual(seen,[])
            r,seen=self.make(tmp)
            self.assertEqual(r.analyze(self.request(text))['units'],1)
            seen.clear()
            result=r.analyze(self.request('41414141'))
            self.assertTrue(result['unknown']);self.assertEqual(result['units'],0);self.assertEqual(seen,[])

    def test_graph_budget_and_duplicate_paths(self):
        view=lambda text:{'candidates':{'a':['b','c'],'b':['d'],'c':['d']}.get(text,[]),'literal_units':0}
        self.assertEqual(explore('a',[],'image',view=view)['text'],'d')
        self.assertEqual(explore('a',[],'image',view=view,max_depth=1)['status'],'budget')
        self.assertEqual(explore('a',[],'image',view=view,max_nodes=2)['status'],'budget')
        looping=lambda text:{'candidates':{'a':['b','c'],'b':['a']}.get(text,[]),'literal_units':0}
        self.assertEqual(explore('a',[],'image',view=looping)['status'],'cycle')

    def test_helper_invalid_input_and_no_bot_credentials(self):
        run=subprocess.run([str(self.helper),'-restore-view'],input=b'{"text":"uo","words":[]}',capture_output=True,env={})
        self.assertNotEqual(run.returncode,0)
        run=subprocess.run([str(self.helper),'-restore-view'],input=b'{"text":"uo","words":["uo"]}',capture_output=True,env={})
        self.assertEqual(json.loads(run.stdout)['literal_units'],1)

if __name__=='__main__':unittest.main()
