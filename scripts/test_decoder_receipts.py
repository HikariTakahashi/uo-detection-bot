"""Retain fresh receiving-side evidence through sync and approval summaries."""
import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch
ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT/'experiments/laya/luna_decoder'), str(ROOT/'services/decoder')]
from adaptive.common import atomic, digest, read
from adaptive.registry import Registry
from adaptive.discovery import candidate
from adaptive.delivery import package, sync
import approval
IMAGE = 'sha256:' + '0'*64

def reply(code='def decode(s):\n    return None\n'):
    return {'status':'REPLIED','request_id':'luna-test-request-123456789','reply_author_id':'1500798681006211252','original_id':'1','reply_id':'2','reply':'```python\n'+code+'```','sent_at':1,'reply_at':2,'deadline_at':3}


class ReceiverEvidenceTests(unittest.TestCase):
    def make_repo(self,root):
        repo=root/'repo';reg=Registry(repo/'registry',IMAGE)
        selector=candidate('%41%42')['selector'];family=candidate('%41%42')['family']
        self.cases=[{'text':'%41%42','expected':'AB'},{'text':'bad','expected':None}]
        self.result={'values':[{'value':'AB','error':None},{'value':None,'error':None}]}
        with patch('adaptive.registry.evaluate',return_value=self.result):
            sha=reg.stage(family,reply('def decode(s):\n    return "AB" if s == "%41%42" else None\n'),self.cases,selector)
        contract={'schema':1,'scope':'synthetic-evaluation-only','family':family,'selector':selector,'cases':self.cases}
        path=repo/'contracts'/'pending.json';atomic(path,contract);ch=digest(path.read_bytes());path.rename(path.with_name(ch+'.json'))
        self.item={'family':family,'sha256':sha,'contract_sha256':ch}
        atomic(repo/'index.json',{'schema':2,'scope':'synthetic-evaluation-only','bundles':[self.item]})
        runtime=repo/'experiments/laya/luna_decoder';runtime.mkdir(parents=True)
        (runtime/'adaptive_service.py').write_text('print("synthetic evaluation only")\n')
        (repo/'README.md').write_text('evaluation')
        return repo

    def fake_gh(self,assets,commit):
        def call(*args):
            if args[:2]==('release','view'):return json.dumps({'isDraft':False,'tagName':'evaluation-'+commit}).encode()
            if args[0]=='api':return json.dumps({'object':{'type':'commit','sha':commit}}).encode()
            if args[:2]==('release','download'):
                name=args[args.index('--pattern')+1];dest=Path(args[args.index('--dir')+1]);shutil.copyfile(assets/name,dest/name);return b''
            raise AssertionError(args)
        return call

    def test_sync_preserves_fresh_receipts_for_approval_summary(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo=self.make_repo(root);assets=root/'assets';commit='a'*40
            package(repo,assets,commit);ih=digest((assets/'index.json').read_bytes())
            state=root/'state';atomic(state/'current/installed/active.json',{})
            # Distinguish fresh receiver execution from bundled historical evidence.
            fresh={**self.result,'receiver_marker':'fresh'}
            with patch('adaptive.delivery.gh',side_effect=self.fake_gh(assets,commit)),patch('adaptive.registry.evaluate',return_value=fresh),patch.object(approval,'ci_success'):
                registry=Registry(root/'received',IMAGE)
                result=sync(approval.REPO,commit,ih,registry)
                sha=result['bundles'][self.item['family']]
                evidence=read(registry.path/'receipts'/(sha+'.json'))
                self.assertEqual(evidence['execution']['receiver_marker'],'fresh')
                summary=approval.verify_candidate(state,{'commit':commit,'index_sha256':ih},IMAGE)
                self.assertEqual(summary,{'families':[self.item['family']],'bundles':1,'checks':2})


if __name__ == '__main__': unittest.main()
