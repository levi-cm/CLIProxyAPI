import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import common
import main
import traffic
from core import IdleWindow


class ProbeCaptureTests(unittest.TestCase):
    def test_real_readiness_probe_does_not_invalidate_a_quiet_window(self):
        now=[1000.]
        idle=IdleWindow(now=lambda:now[0]);observer=traffic.Observer(clock=lambda:now[0]);observer.healthy=True
        now[0]=1301.
        self.assertTrue(idle.ready(observer.snapshot()))
        commit='a'*40
        def api(path,address='127.0.0.1',management=False,body=None,headers=None,timeout=1):
            self.assertIsNone(body)
            request=('GET '+path+' HTTP/1.1\r\nHost: fixture\r\n\r\n').encode()
            connection=(address,path)
            observer.tcp(connection,100,0x18,request)
            observer.tcp(connection,100+len(request),0x04,b'')
            data={}
            if path=='/v8/management/account-policy/diagnostics':
                data={'settings':{'enabled':True,'automation_required':True,'automation':'auto_expiring','read_only':False,'expiry_guard_seconds':600,'credit_types':['codex_rate_limits']}}
            elif path=='/v1/account-policy/usage-history/status':data={'healthy':True}
            return 200,{'X-CPA-Commit':commit},json.dumps(data).encode()
        with patch.object(common,'api',side_effect=api):
            self.assertEqual(common.probe(commit)['interfaces'],3)
        self.assertEqual(observer.last_business,1000.,'the controller must not reset its own idle timer')
        self.assertTrue(idle.ready(observer.snapshot()))

    def test_only_exact_read_only_history_status_is_monitoring(self):
        safe=b'GET /v1/account-policy/usage-history/status HTTP/1.1\r\n\r\n'
        for payload,ignored in [(safe,True),(safe.replace(b'GET',b'POST',1),False),(safe.replace(b'/status ',b'/status/ ',1),False),(safe.replace(b'/status ',b'/reset ',1),False),(safe.replace(b'\r\n\r\n',b'\r\nContent-Length: 2\r\n\r\n{}'),False)]:
            with self.subTest(payload=payload):
                now=[1.];o=traffic.Observer(clock=lambda:now[0]);now[0]=2.
                o.tcp('client',100,0x18,payload)
                self.assertEqual(o.last_business,1. if ignored else 2.)

class FailureEvidenceTests(unittest.TestCase):
    def test_unknown_error_records_location_without_exposing_its_message(self):
        self.assertTrue(hasattr(main,'failure_details'),'aborts must retain safe diagnostic evidence')
        try:
            raise RuntimeError('Bearer PRIVATE-CREDENTIAL-VALUE')
        except RuntimeError as error:
            details=main.failure_details(error)
        self.assertNotIn('PRIVATE-CREDENTIAL-VALUE',json.dumps(details))
        self.assertEqual(details['error_class'],'RuntimeError')
        self.assertEqual(details['source']['file'],Path(__file__).name)
        self.assertEqual(details['source']['function'],'test_unknown_error_records_location_without_exposing_its_message')

    def test_expired_rehearsal_has_a_specific_restage_reason(self):
        import backend
        from core import DeploymentBlocked
        with tempfile.TemporaryDirectory() as tmp:
            directory=Path(tmp)
            for name in ['config.yaml','.env','rehearsal.json']:(directory/name).write_text('{}')
            b=backend.Backend.__new__(backend.Backend);b.repo=directory;b.dir=directory;b.service='fixture'
            b.m={'old_id':'old','candidate_id':'next','old_image_id':'old-image','old_pid':1,'candidate_image_id':'next-image','prepared_at':1,'rehearsal_sha256':common.digest(directory/'rehearsal.json')}
            for name in ['config.yaml','.env']:b.m[name+'_sha256']=common.digest(directory/name)
            def inspect(identity):
                if identity=='old':return {'Name':'/fixture','Image':'old-image','State':{'Running':True,'Pid':1}}
                return {'Image':'next-image','State':{'Running':False}}
            with patch.object(backend,'inspect',side_effect=inspect),patch.object(backend.time,'time',return_value=90000):
                with self.assertRaises(RuntimeError) as raised:b.preflight()
            self.assertIsInstance(raised.exception,DeploymentBlocked)
            self.assertEqual(raised.exception.reason,'rehearsal_expired')
            self.assertTrue(raised.exception.details['requires_restage'])


class FixtureObservationTests(unittest.TestCase):
    def test_only_initial_quiet_baseline_is_seeded_and_later_business_blocks(self):
        import prepare
        self.assertTrue(hasattr(prepare,'fixture_observation'),'rehearsals must observe actual request traffic')
        now=[1000.]
        raw={'healthy':True,'timestamp':1000.,'last_business':1000.,'pending':0,'capture_drops':0}
        class Capture:
            def read(self,timeout=0):return dict(raw)
        idle,evidence=prepare.fixture_observation(Capture(),now=lambda:now[0])
        self.assertTrue(idle.ready(evidence()))
        now[0]=1001.;raw.update(timestamp=1001.,last_business=1001.)
        self.assertFalse(idle.ready(evidence()))
        self.assertEqual(raw['last_business'],1001.)
