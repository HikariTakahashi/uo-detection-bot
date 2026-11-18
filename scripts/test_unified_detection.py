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
import production_policy as policy
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

    def test_readable_japanese_is_not_held_by_detector_alone(self):
        cases=json.loads((ROOT/'scripts/fixtures/readable-japanese.json').read_text())
        cases += [{'id':'greeting','text':'こんにちは！今日もよろしくお願いします。'},
                  {'id':'quotation','text':'「うおw」はどういう意味？'},
                  {'id':'conversation','text':'収益化まだかな'}]
        for suspected,technical_hold in [(True,False),(False,True)]:
            for case in cases:
                with self.subTest(case=case['id'],suspected=suspected,technical_hold=technical_hold),tempfile.TemporaryDirectory() as tmp:
                    r,seen=self.make(tmp)
                    def predict(route,value):
                        seen.append((route,value['text']))
                        if route=='detect':
                            return {'suspected':suspected,'technical_hold':technical_hold,'has_authored_target':False}
                        # No model is needed for a target-free ordinary post.
                        return policy.judge(None,value['text'],value['words'])
                    r.predict=predict
                    out=r.analyze(self.request(case['text']))
                    self.assertEqual((out['unknown'],out['label'],out['units'],out['families']),(False,'other',0,[]))
                    self.assertEqual(seen,[('detect',case['text']),('judge',case['text'])])
                    self.assertFalse((Path(tmp)/'discovery').exists())

    def test_restored_readable_japanese_is_not_held_by_detector_alone(self):
        cases=json.loads((ROOT/'scripts/fixtures/readable-japanese.json').read_text())
        for case in cases:
            with self.subTest(case=case['id']),tempfile.TemporaryDirectory() as tmp:
                r,seen=self.make(tmp)
                def predict(route,value):
                    seen.append((route,value['text']))
                    if route=='detect':return {'suspected':True,'technical_hold':False,'has_authored_target':False}
                    return policy.judge(None,value['text'],value['words'])
                r.predict=predict
                text=synthetic.encode('base64',synthetic.encode('percent',case['text']))
                out=r.analyze(self.request(text))
                self.assertEqual((out['unknown'],out['label'],out['units']),(False,'other',0))
                self.assertEqual(seen[-1],('judge',case['text']))
                self.assertFalse((Path(tmp)/'discovery').exists())

    def test_japanese_prose_with_encoded_target_still_reaches_judge(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp)
            out=r.analyze(self.request('まじか、%E3%81%86%E3%81%8Aw'))
            self.assertEqual((out['unknown'],out['label'],out['units']),(False,'mockery',1))
            self.assertEqual(seen[-1],('judge','まじか、うおw'))

    def test_selectorless_estimation_is_separate_and_never_counted(self):
        import estimation
        for text in ['q7|p2^R9~s4!T6','う\u200bお\u200bw']:
            with self.subTest(text=text),tempfile.TemporaryDirectory() as tmp:
                r,seen=self.make(tmp)
                out=r.analyze({**self.request(text),'channel_id':'123456789012345678'})
                self.assertEqual((out['unknown'],out['label'],out['units'],out['families']),(True,'uncertain',0,[]))
                self.assertEqual(out['estimation']['state'],'queued');self.assertEqual(seen,[])
                queue=estimation.Queue(tmp);job=estimation.read(queue.path(out['estimation']['id']))
                queue.save(job,'unverified','independent_evidence_required')
                # Even an apparently successful unverified result is never a decoder.
                job=estimation.read(queue.path(job['id']));job['candidates']=[{'code':"def decode(text): return 'うお'"}]
                queue.save(job)
                again=r.analyze({**self.request(text),'channel_id':'123456789012345678'})
                self.assertEqual((again['unknown'],again['units']),(True,0))
                self.assertEqual(again['estimation']['state'],'unverified');self.assertEqual(seen,[])

    def test_ordinary_japanese_with_channel_still_never_queues(self):
        cases=json.loads((ROOT/'scripts/fixtures/readable-japanese.json').read_text())
        for case in cases:
            with self.subTest(case=case['id']),tempfile.TemporaryDirectory() as tmp:
                r,seen=self.make(tmp)
                def predict(route,value):
                    if route=='detect':return {'suspected':True,'has_authored_target':False}
                    return policy.judge(None,value['text'],value['words'])
                r.predict=predict
                out=r.analyze({**self.request(case['text']),'channel_id':'123456789012345678'})
                self.assertEqual((out['unknown'],out['label'],out['units']),(False,'other',0))
                self.assertNotIn('estimation',out)
                self.assertFalse((Path(tmp)/'estimation').exists())

    def test_explicit_reverse_request_creates_queue_without_a_selector(self):
        with tempfile.TemporaryDirectory() as tmp:
            r,seen=self.make(tmp)
            status=r.estimate({'text':'wおう','revision':'b'*64,'channel_id':'123456789012345678','start':0,'end':3})
            self.assertEqual((status['state'],status['reason']),('queued','external_disabled'))
            self.assertEqual(status,r.estimate_status(status['id']));self.assertEqual(seen,[])

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
