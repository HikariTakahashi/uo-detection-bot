import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

spec = importlib.util.spec_from_file_location("updater", Path(__file__).with_name("update-bot.py"))
u = importlib.util.module_from_spec(spec)
spec.loader.exec_module(u)
OLD = "a" * 40
NEW = "b" * 40

class UpdateTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        for name in ["app", "state"]: (base / name).mkdir()
        self.patches = [patch.object(u, "ROOT", base / "app"), patch.object(u, "STATE", base / "state"), patch.object(u, "STABILITY_SECONDS", 1), patch.object(u.time, "sleep")]
        for p in self.patches: p.start(); self.addCleanup(p.stop)
        self.binary = b'\x7fELF\x02\x01' + b'\0'*12 + b'\xb7\x00' + b'bot'
        self.release = {"draft": False,"tag_name":"deploy-"+NEW,"assets":[{"name":u.ASSET},{"name":"SHA256SUMS"}]}

    def systemctl(self, *args):
        return subprocess.CompletedProcess(args, 0, stdout=b"123\n" if args[0] == "show" else b"")

    def test_deploy_success(self):
        u.switch(OLD)
        with patch.object(u, "systemctl", side_effect=self.systemctl): u.deploy(NEW,{})
        self.assertEqual(u.current_sha(), NEW)
        self.assertEqual(u.read_state(), {"current": NEW})

    def test_failed_start_rolls_back_and_blocks_repeat(self):
        u.switch(OLD)
        restarted = []
        def control(*args):
            if args[0] == 'restart':
                restarted.append(u.current_sha())
                if u.current_sha() == NEW: raise RuntimeError("start failed")
            return self.systemctl(*args)
        with patch.object(u,"systemctl",side_effect=control):
            with self.assertRaises(RuntimeError): u.deploy(NEW,{})
        self.assertEqual(u.current_sha(),OLD)
        self.assertEqual(restarted,[NEW,OLD])
        self.assertEqual(u.read_state()["failed"], NEW)
        with patch.object(u,"get_json",return_value={"object":{"sha":NEW}}), patch.object(u,"prepare") as prepare:
            u.update(); prepare.assert_not_called()

    def test_first_install_failure_stops_bot(self):
        calls=[]
        def control(*args):
            calls.append(args[0])
            if args[0]=='restart':raise RuntimeError('start failed')
            return self.systemctl(*args)
        with patch.object(u,'systemctl',side_effect=control):
            with self.assertRaises(RuntimeError):u.deploy(NEW,{})
        self.assertIsNone(u.current_sha())
        self.assertIn('stop',calls)

    def test_interrupted_update_recovers_before_network(self):
        u.switch(NEW)
        u.write_state({'pending':{'sha':NEW,'previous':OLD}})
        def network(*_):
            self.assertEqual(u.current_sha(),OLD)
            return {'object':{'sha':NEW}}
        with patch.object(u,'systemctl',side_effect=self.systemctl),patch.object(u,'get_json',side_effect=network):u.update()
        self.assertEqual(u.read_state()['failed'],NEW)
        self.assertNotIn('pending',u.read_state())

    def test_crashing_process_rolls_back(self):
        u.switch(OLD)
        pids=iter([b'123',b'456'])
        def control(*args):
            if args[0]=='show':return subprocess.CompletedProcess(args,0,stdout=next(pids))
            return self.systemctl(*args)
        with patch.object(u,'systemctl',side_effect=control):
            with self.assertRaises(RuntimeError):u.deploy(NEW,{})
        self.assertEqual(u.current_sha(),OLD)

    def test_download_checks_checksum_before_switch(self):
        u.switch(OLD)
        checksum=hashlib.sha256(self.binary).hexdigest()
        with patch.object(u,'request',side_effect=[(checksum+'  '+u.ASSET+'\n').encode(),self.binary]):u.prepare(NEW,self.release)
        self.assertEqual(u.current_sha(),OLD)
        self.assertEqual((u.ROOT/'releases'/NEW/'discord-bot').read_bytes(),self.binary)
        with patch.object(u,'request',side_effect=[('0'*64+'  '+u.ASSET+'\n').encode(),self.binary]):
            with self.assertRaisesRegex(ValueError,'checksum mismatch'):u.prepare(NEW,self.release)
        self.assertEqual(u.current_sha(),OLD)

    def test_incomplete_release_does_not_deploy(self):
        release=dict(self.release);release['draft']=True
        with self.assertRaises(ValueError):u.prepare(NEW,release)

    def test_ci_pending_leaves_current(self):
        u.switch(OLD)
        error=urllib.error.HTTPError('url',404,'not found',{},io.BytesIO())
        with patch.object(u,'get_json',side_effect=[{'object':{'sha':NEW}},error]),patch.object(u,'deploy') as deploy:
            u.update();deploy.assert_not_called()
        self.assertEqual(u.current_sha(),OLD)

    def test_newer_main_during_download_is_not_deployed(self):
        u.switch(OLD)
        with patch.object(u,'get_json',side_effect=[{'object':{'sha':NEW}},self.release,{'object':{'sha':'c'*40}}]),patch.object(u,'prepare'),patch.object(u,'deploy') as deploy:
            u.update();deploy.assert_not_called()
        self.assertEqual(u.current_sha(),OLD)

    def test_network_failure_does_not_restart(self):
        u.switch(OLD)
        with patch.object(u,'get_json',side_effect=TimeoutError()),patch.object(u,'systemctl') as ctl:
            with self.assertRaises(TimeoutError):u.update()
            ctl.assert_not_called()
        self.assertEqual(u.current_sha(),OLD)

if __name__ == '__main__':unittest.main()
