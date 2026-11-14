"""Regression tests for the 2026-11-14 outage: a cutover that never started the router."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch,MagicMock

import backend
import common
import main
import prepare
from common import ROOT,atomic,digest
from core import DeploymentBlocked


class Clock:
    def __init__(self):self.t=0.0
    def __call__(self):return self.t
    def sleep(self,s):self.t+=s


class ComposeLeftoverTests(unittest.TestCase):
    info={'Id':'live','Config':{'Labels':{'com.docker.compose.project':'cliproxyapi','com.docker.compose.service':'cli-proxy-api'}}}

    def test_other_labelled_containers_are_reported(self):
        listing='live cli-proxy-api running\nold cli-proxy-api-rollback-abc exited\n'
        self.assertEqual(common.compose_leftovers(self.info,listing),[{'id':'old','name':'cli-proxy-api-rollback-abc','state':'exited'}])

    def test_unlabelled_container_has_no_compose_peers(self):
        self.assertEqual(common.compose_leftovers({'Id':'x','Config':{'Labels':{}}},'y z exited\n'),[])

    def test_preflight_blocks_stale_containers_but_not_own_candidate(self):
        b=backend.Backend.__new__(backend.Backend);b.m={'candidate_id':'next'}
        with patch.object(backend,'compose_leftovers',return_value=[{'id':'next','name':'cli-proxy-api-idle-next-t','state':'created'}]):
            b.check_compose(self.info)
        with patch.object(backend,'compose_leftovers',return_value=[{'id':'old','name':'cli-proxy-api-rollback-t','state':'exited'}]):
            with self.assertRaises(DeploymentBlocked) as caught:b.check_compose(self.info)
        self.assertEqual(caught.exception.reason,'stale_compose_containers')
        self.assertTrue(caught.exception.details['requires_restage'])

    def test_prepare_removes_only_stopped_runner_leftovers(self):
        calls=[]
        with patch.object(prepare,'compose_leftovers',side_effect=[[{'id':'a','name':'cli-proxy-api-rollback-t','state':'exited'}],[]]),\
                patch.object(prepare,'run',side_effect=lambda args,**_:calls.append(args) or b''):
            self.assertEqual(prepare.remove_stale_runner_containers(self.info),['cli-proxy-api-rollback-t'])
        self.assertEqual(calls,[['docker','rm','a']])
        for item in ({'id':'b','name':'cli-proxy-api-rollback-t','state':'running'},{'id':'c','name':'something-else','state':'exited'}):
            with patch.object(prepare,'compose_leftovers',return_value=[item]),patch.object(prepare,'run') as run:
                with self.assertRaises(RuntimeError):prepare.remove_stale_runner_containers(self.info)
                run.assert_not_called()


class HostReadyTests(unittest.TestCase):
    def test_times_out_instead_of_reporting_success(self):
        clock=Clock()
        with patch.object(common,'inspect',return_value={'State':{'Running':False,'Status':'created'}}):
            with self.assertRaisesRegex(RuntimeError,'State|state created'):
                common.host_ready('c','sha',limit=60,clock=clock,sleep=clock.sleep,check=lambda:{'ok':True})
        self.assertGreaterEqual(clock.t,60)

    def test_failing_probe_times_out(self):
        clock=Clock()
        def check():raise OSError('refused')
        with patch.object(common,'inspect',return_value={'State':{'Running':True}}):
            with self.assertRaisesRegex(RuntimeError,'probe failed'):
                common.host_ready('c','sha',limit=5,clock=clock,sleep=clock.sleep,check=check)

    def test_running_and_probe_passing_succeeds(self):
        clock=Clock()
        with patch.object(common,'inspect',return_value={'State':{'Running':True}}):
            self.assertEqual(common.host_ready('c','sha',clock=clock,sleep=clock.sleep,check=lambda:{'commit':'sha'})['state'],'running')


class ConfigStagingTests(unittest.TestCase):
    def fake(self,tmp):
        d=Path(tmp);b=backend.Backend.__new__(backend.Backend);b.dir=d;b.repo=d;b.journal=d/'transaction.json';b.state=d/'state';b.service='fixture-service';b.redemption=None
        (d/'config.yaml').write_bytes(b'old: 1\n');(d/'config.before').write_bytes(b'old: 1\n');(d/'config.after').write_bytes(b'new: 2\n')
        (d/'env.before').write_bytes(b'A=1\n');(d/'.env').write_bytes(b'A=1\n')
        b.m={'old_id':'old','candidate_id':'next','old_commit':'sha','timing':{'rollback':1},'config.yaml_sha256':digest(d/'config.yaml'),'config_after_sha256':digest(d/'config.after')}
        b.state.mkdir();atomic(b.state/'conversation-bindings.json',{'records':[]});atomic(b.journal,{'phase':'old_stopped'})
        b.assert_no_writers=MagicMock();return b

    def test_staged_config_applied_in_place_and_restored_on_rollback(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=self.fake(tmp);inode=(b.repo/'config.yaml').stat().st_ino
            b.apply_config()
            self.assertEqual((b.repo/'config.yaml').read_bytes(),b'new: 2\n');self.assertEqual((b.repo/'config.yaml').stat().st_ino,inode)
            commands=[]
            def inspect(name,*_):return {'Id':name,'Name':'/fixture-service','State':{'Running':name=='old' and any(c[:2]==['docker','start'] for c in commands),'Pid':1}} if name=='old' else None
            def run(args,**_):commands.append(args);return b'{}'
            with patch.object(backend,'inspect',side_effect=inspect),patch.object(backend,'run',side_effect=run),patch.object(backend,'writers'):
                b.recover()
            self.assertEqual((b.repo/'config.yaml').read_bytes(),b'old: 1\n')
            self.assertEqual(json.loads(b.journal.read_text())['phase'],'rolled_back')

    def test_operator_edit_is_never_overwritten_by_rollback(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=self.fake(tmp);b.apply_config();(b.repo/'config.yaml').write_bytes(b'operator: 3\n')
            with patch.object(backend,'inspect',side_effect=lambda n,*_:{'Id':n,'Name':'/fixture-service','State':{'Running':False,'Pid':1}} if n=='old' else None),\
                    patch.object(backend,'run',return_value=b'{}'),patch.object(backend,'writers'),patch.object(backend.time,'monotonic',side_effect=[0,100]):
                with self.assertRaises(RuntimeError):b.recover()
            self.assertEqual((b.repo/'config.yaml').read_bytes(),b'operator: 3\n')


class SettleTests(unittest.TestCase):
    def setup(self,tmp,phase):
        d=Path(tmp);b=MagicMock();b.journal=d/'transaction.json';atomic(b.journal,{'phase':phase,'interruption_seconds':7})
        m={'token':'t','old_id':'old','candidate_id':'next','old_image_id':'sha256:old','candidate_image_id':'sha256:new','old_commit':'o','candidate_commit':'n'}
        b.leftovers.return_value=[];b.remove_runner_container.return_value='cli-proxy-api-rollback-t'
        return d,b,m

    def test_verified_commit_reports_deployed_and_removes_rollback_container(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'committed');b.host_verify.return_value={'state':'running'};published={}
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):
                self.assertTrue(main.settle(d/'release.json',m,b))
            b.remove_runner_container.assert_called_once_with('old','-rollback-')
            self.assertEqual(published['status.json']['phase'],'deployed');self.assertTrue(published['status.json']['final'])
            b.start_rescue.assert_not_called()

    def test_failed_host_verification_rolls_back_through_independent_unit(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'committed');b.host_verify.side_effect=RuntimeError('host verification failed');published={}
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):
                self.assertFalse(main.settle(d/'release.json',m,b))
            b.phase.assert_called_with('recovering',post_admission_failure=unittest.mock.ANY)
            b.start_rescue.assert_called_once();b.remove_runner_container.assert_not_called()
            self.assertEqual(published['status.json']['phase'],'rolling_back');self.assertIn('PRODUCTION',published['status.json']['alert'])

    def test_rescue_failure_raises_so_systemd_retries(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'rolled_back');b.host_verify.side_effect=RuntimeError('down')
            with patch.object(main,'publish'):
                with self.assertRaises(RuntimeError):main.settle(d/'release.json',m,b,in_rescue=True)
            b.phase.assert_called_with('recovering',post_admission_failure=unittest.mock.ANY);b.start_rescue.assert_not_called()

    def test_unit_start_failure_keeps_retrying_original_inline(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'committed');b.host_verify.side_effect=[RuntimeError('down'),{'state':'running'}]
            b.start_rescue.side_effect=RuntimeError('systemd-run failed')
            attempts=[]
            def recover():
                attempts.append(1)
                if len(attempts)<3:raise RuntimeError('original not ready')
                atomic(b.journal,{'phase':'rolled_back'})
            b.recover.side_effect=recover;published=[]
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.append(v.get('phase'))),patch.object(main.time,'sleep'):
                self.assertFalse(main.settle(d/'release.json',m,b))
            self.assertEqual(len(attempts),3);self.assertIn('rollback_failed_retrying',published);self.assertEqual(published[-1],'rolled_back')

    def test_verified_rollback_removes_failed_candidate(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'rolled_back');b.host_verify.return_value={'state':'running'};published={}
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):
                self.assertFalse(main.settle(d/'release.json',m,b))
            b.host_verify.assert_called_once_with('old','o');b.remove_runner_container.assert_called_once_with('next','-failed-')
            self.assertEqual(published['status.json']['phase'],'rolled_back')

    def test_unfinished_recovery_is_loud(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'recovering');published={}
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):main.settle(d/'release.json',m,b)
            self.assertEqual(published['status.json']['phase'],'rollback_failed_retrying');self.assertFalse(published['status.json']['final'])

    def test_cleanup_error_never_hides_verified_deploy(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d,b,m=self.setup(tmp,'committed');b.host_verify.return_value={'state':'running'};b.remove_runner_container.side_effect=RuntimeError('busy');published={}
            with patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):
                self.assertTrue(main.settle(d/'release.json',m,b))
            self.assertEqual(published['status.json']['phase'],'deployed');self.assertIn('cleanup_error',published['status.json'])

    def test_fixture_release_never_probes_production(self):
        b=MagicMock()
        self.assertFalse(main.settle(Path('x'),{'fixture_repo':'/f'},b));b.host_verify.assert_not_called()

    def test_rescue_failure_publishes_loud_status_and_reraises(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            path=Path(tmp)/'release.json';atomic(path,{'token':'t'});published={}
            with patch.object(main,'_rescue',side_effect=RuntimeError('original did not start')),patch.object(main,'publish',side_effect=lambda m,n,v:published.__setitem__(n,v)):
                with self.assertRaises(RuntimeError):main.rescue(path)
            self.assertEqual(published['status.json']['phase'],'rollback_failed_retrying')


class WatchdogTests(unittest.TestCase):
    def test_rescue_units_retry_without_a_start_limit(self):
        b=backend.Backend.__new__(backend.Backend);b.timer='cliproxy-idle-watchdog-t';b.candidate_budget=20;b.path=Path('r');b.redemption=None
        with patch.object(backend,'run') as run:b.watchdog();b.start_rescue()
        for call in run.call_args_list:
            args=[str(a) for a in call.args[0]]
            self.assertIn('--property=StartLimitIntervalSec=0',args);self.assertIn('--property=Restart=on-failure',args);self.assertIn('rescue',args)


class ComposeDryRunTests(unittest.TestCase):
    def rendered(self,source):
        return {'name':'x','services':{'cli-proxy-api':{'image':'old','build':{},'container_name':'cli-proxy-api','restart':'unless-stopped',
                'ports':[{'target':8317,'published':'8317','host_ip':'127.0.0.1'}],'volumes':[{'type':'bind','source':source,'target':'/CLIProxyAPI/config.yaml'}]}}}

    def test_only_project_port_restart_and_mounts_change(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            fixture=Path(tmp);(fixture/'config.yaml').write_text('x')
            out=prepare.dryrun_compose(self.rendered(str(ROOT/'config.yaml')),'cliproxy-dryrun-t',fixture,40001,'sha256:new')
            svc=out['services']['cli-proxy-api']
            self.assertEqual(svc['container_name'],'cliproxy-dryrun-t');self.assertEqual(svc['restart'],'no');self.assertNotIn('build',svc)
            self.assertEqual([p['published'] for p in svc['ports']],['40001']);self.assertEqual(svc['volumes'][0]['source'],str(fixture/'config.yaml'))

    def test_refuses_mounts_outside_the_fixture(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            with self.assertRaises((RuntimeError,ValueError)):prepare.dryrun_compose(self.rendered('/etc/passwd'),'p',Path(tmp),40001,'i')
            with self.assertRaises(RuntimeError):prepare.dryrun_compose(self.rendered(str(ROOT/'config.yaml')),'p',Path(tmp),40001,'i')

    def test_created_after_successful_compose_up_fails(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            fixture=Path(tmp);(fixture/'config.yaml').write_text('x');times=iter([0,1,5,5,5])
            with patch.object(prepare,'render_compose',return_value={}),patch.object(prepare,'dryrun_compose',return_value={}),\
                    patch.object(prepare.subprocess,'run',return_value=MagicMock(returncode=0)),\
                    patch.object(prepare,'inspect',return_value={'Image':'img','State':{'Status':'created'}}),\
                    patch.object(prepare.time,'monotonic',side_effect=lambda:next(times)),patch.object(prepare.time,'sleep'):
                with self.assertRaisesRegex(RuntimeError,'State created'):
                    prepare.compose_up('p',fixture/'c.json',fixture,40001,'img',b'y')

    def test_nonzero_compose_exit_fails(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            fixture=Path(tmp);(fixture/'config.yaml').write_text('x')
            with patch.object(prepare,'render_compose',return_value={}),patch.object(prepare,'dryrun_compose',return_value={}),\
                    patch.object(prepare.subprocess,'run',return_value=MagicMock(returncode=1,stdout=b'')):
                with self.assertRaisesRegex(RuntimeError,'exited 1'):prepare.compose_up('p',fixture/'c.json',fixture,40001,'img',b'y')


if __name__=='__main__':unittest.main()
