import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'services/decoder'))
import approval
from adaptive.common import atomic, read

class ApprovalTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name);self.state=self.root/'state';self.shared=self.root/'shared'
        for name in ('requests','decisions','results'):(self.shared/name).mkdir(parents=True)
        self.base={'repository':approval.REPO,'commit':'a'*40,'index_sha256':'b'*64}
        self.new={'repository':approval.REPO,'commit':'c'*40,'index_sha256':'d'*64}
        atomic(self.state/'current/active-production.json',self.base)
        atomic(self.state/'publication-pushed.json',self.new)
        self.calls=[];self.ci=patch.object(approval,'ci_success');self.ci.start();self.addCleanup(self.ci.stop)
    def verify(self,*args):return {'families':['synthetic'],'bundles':2,'checks':10}
    def activate(self,state,pin,image,expected_base):
        self.assertEqual(approval.current(state),expected_base);self.calls.append(pin)
        atomic(state/'current/active-production.json',pin)
    def tick(self,now=1000,**kwargs):
        approval.advance(self.state,self.shared,'image',now=now,bot_uid=os.getuid(),verify=self.verify,activate=self.activate,**kwargs)
    def request(self):
        rid=read(self.shared/'requests/current.json')['id'];return read(self.shared/'requests'/(rid+'.json'))
    def result(self):return read(self.shared/'results'/(self.request()['id']+'.json'))['state']
    def decide(self,action='approve',**changes):
        r=self.request();raw=(self.shared/'requests'/(r['id']+'.json')).read_bytes()
        d={'request_id':r['id'],'request_sha256':hashlib.sha256(raw).hexdigest(),'actor':approval.APPROVER,'action':action,'decided_at':1001,'channel_id':'111111111111111111','message_id':'222222222222222222'}
        d.update(changes);approval.shared_write(self.shared/'decisions'/(r['id']+'.json'),d)
    def test_verified_candidate_waits_for_exact_approval(self):
        self.tick();r=self.request();self.assertEqual(self.calls,[])
        self.assertEqual(r['expires_at']-r['created_at'],7*86400)
        self.decide();self.tick(1002);self.tick(1003)
        self.assertEqual(self.calls,[self.new]);self.assertEqual(self.result(),'active')
    def test_reject_and_expire_never_resend(self):
        self.tick();rid=self.request()['id'];self.decide('reject');self.tick(1002);self.tick(999999)
        self.assertEqual(self.result(),'rejected');self.assertEqual(rid,self.request()['id']);self.assertEqual(self.calls,[])
    def test_expiration_retry_revalidates_new_request(self):
        self.tick();rid=self.request()['id'];self.tick(1000+7*86400)
        self.assertEqual(self.result(),'expired');self.tick(1001+7*86400,retry_request=rid)
        self.assertNotEqual(rid,self.request()['id']);self.assertEqual(self.calls,[])
    def test_other_actor_rejected(self):
        self.tick();self.decide(actor='333333333333333333')
        with self.assertRaises(ValueError):self.tick(1002)
        self.assertEqual(self.calls,[])
    def test_changed_request_and_late_decision_rejected(self):
        self.tick();self.decide(request_sha256='0'*64)
        with self.assertRaises(ValueError):self.tick(1002)
        self.decide(decided_at=1000+7*86400)
        with self.assertRaises(ValueError):self.tick(1001+7*86400)
    def test_live_base_change_supersedes(self):
        self.tick();self.decide();atomic(self.state/'current/active-production.json',{**self.base,'commit':'e'*40})
        self.tick(1002);self.assertEqual(self.result(),'superseded');self.assertEqual(self.calls,[])
    def test_new_candidate_invalidates_old_buttons(self):
        self.tick();old=self.request()['id'];self.decide()
        atomic(self.state/'publication-pushed.json',{**self.new,'commit':'e'*40})
        self.tick(1002);self.assertNotEqual(old,self.request()['id'])
        self.assertEqual(read(self.shared/'results'/(old+'.json'))['state'],'superseded');self.assertEqual(self.calls,[])
    def test_crash_after_activation_recovers_without_reapply(self):
        self.tick();self.decide()
        def crash(state,pin,image,expected_base):
            self.activate(state,pin,image,expected_base);raise RuntimeError('interrupted after switch')
        with self.assertRaises(RuntimeError):
            approval.advance(self.state,self.shared,'image',now=1002,bot_uid=os.getuid(),verify=self.verify,activate=crash)
        self.assertEqual(self.result(),'applying');self.tick(1003)
        self.assertEqual(self.result(),'active');self.assertEqual(len(self.calls),1)
    def test_failed_verification_keeps_previous(self):
        self.tick();self.decide()
        with patch.object(approval,'ci_success',side_effect=ValueError('failed')):self.tick(1002)
        self.assertEqual(self.result(),'failed');self.assertEqual(approval.current(self.state),self.base)
    def test_pending_ci_sends_no_request(self):
        def bad(*args):raise ValueError('CI pending')
        with self.assertRaises(ValueError):approval.advance(self.state,self.shared,'image',now=1000,bot_uid=os.getuid(),verify=bad)
        self.assertFalse((self.shared/'requests/current.json').exists())
    def test_current_bootstrap_not_reverted(self):
        atomic(self.state/'current/active-production.json',self.new)
        self.tick();self.assertFalse((self.shared/'requests/current.json').exists());self.assertEqual(self.calls,[])
    def test_symlink_and_untrusted_permissions_rejected(self):
        p=self.root/'target';p.write_text('{}');link=self.root/'link';link.symlink_to(p)
        with self.assertRaises(OSError):approval.secure_read(link)
        p.chmod(0o666)
        with self.assertRaises(ValueError):approval.secure_read(p)

if __name__=='__main__':unittest.main()
