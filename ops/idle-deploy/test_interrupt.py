import fcntl
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import MagicMock,patch

import backend
import interrupt
import maintenance
from common import ROOT,atomic,digest,lock_available
from core import Cutover,DeploymentBlocked,IdleWindow


class Clock:
    def __init__(self,now=100.):self.now=now
    def __call__(self):return self.now
    def sleep(self,seconds):self.now+=seconds


class Control:
    exclude=(('127.0.0.1',8317),('127.0.0.1',34567))
    def __init__(self,active=0,events=None):self.active=active;self.closed=False;self.events=events if events is not None else []
    def activity(self):
        if self.closed:raise RuntimeError('control socket closed')
        self.events.append('activity');return self.active
    def close(self):self.closed=True


def release_dir(tmp):
    d=Path(tmp);(d/'config.yaml').write_text('fixture: true\n')
    for name in ('.env','env.before'):(d/name).write_text('CLI_PROXY_IMAGE=old\n')
    atomic(d/'rehearsal.json',{'complete_cycles':{'cycles':[]}})
    state=d/'state';(state/'history').mkdir(parents=True)
    for name in ('owner.lock','history/writer.lock','redemption.lock'):(state/name).touch()
    atomic(state/'conversation-bindings.json',{'records':[]})
    return d


def make_backend(d,cls=interrupt.InterruptBackend):
    b=cls.__new__(cls)
    b.path=d/'release.json';b.dir=d;b.repo=d;b.state=d/'state';b.journal=d/'transaction.json';b.service='cli-proxy-api';b.token='tok'
    b.timer='cliproxy-idle-watchdog-tok';b.started=None;b.redemption=None;b.drain_connection=None;b.idle=None;b.evidence=None;b.gates=MagicMock()
    here=Path(interrupt.__file__).parent
    b.m={'token':'tok','old_id':'old','candidate_id':'next','old_pid':42,'ports':[8085,8317],'old_image_id':'img-old','candidate_image_id':'img-new',
         'old_commit':'old-sha','candidate_commit':'new-sha','prepared_at':time.time(),'rehearsal_sha256':digest(d/'rehearsal.json'),
         'controller_files':{n:digest(here/n) for n in ('backend.py','interrupt.py')},
         'timing':{'candidate':4,'rollback':4,'stop':2,'budget':40,'recovery_total':8},
         'config.yaml_sha256':digest(d/'config.yaml'),'.env_sha256':digest(d/'.env')}
    return b


def containers(network='bridge'):
    def inspect(name,optional=False):
        if name=='old':return {'Id':'old','Name':'/cli-proxy-api','Image':'img-old','State':{'Running':True,'Pid':42},'HostConfig':{'NetworkMode':network}}
        return {'Id':'next','Name':'/cli-proxy-api-idle-next-tok','Image':'img-new','State':{'Running':False,'Pid':0},'HostConfig':{'NetworkMode':network}}
    return inspect


def started_journal(b):
    def reset():atomic(b.journal,{'phase':'preflight','attempt':1})
    b.reset_attempt=MagicMock(side_effect=reset);b.snapshot=MagicMock()


class InterruptPreflightTests(unittest.TestCase):
    def test_active_requests_connections_and_missing_idle_evidence_do_not_block(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp));started_journal(b)
            with patch.object(interrupt,'inspect',side_effect=containers()),patch.object(interrupt,'probe') as probe,patch.object(interrupt,'writers'),\
                 patch.object(backend,'activity',return_value=5) as active,patch.object(backend,'connections',return_value=9) as clients:
                b.preflight()
            try:
                active.assert_not_called();clients.assert_not_called();probe.assert_called_once_with('old-sha')
                self.assertFalse(lock_available(b.state/'redemption.lock'))
                journal=json.loads(b.journal.read_text())
                self.assertEqual((journal['phase'],journal['mode']),('preflight','interrupt'))
            finally:b.release_redemption()

    def test_gate_closes_admission_despite_active_requests(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp));control=Control(active=3)
            with patch.object(interrupt,'DrainConnection',return_value=control),patch.object(interrupt.time,'monotonic',return_value=100.):
                b.gate()
            b.gates.enable.assert_called_once();self.assertIs(b.drain_connection,control);self.assertEqual(b.started,100.)

    def test_in_flight_redemption_is_awaited_then_fenced(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp));started_journal(b)
            holder=(b.state/'redemption.lock').open('rb');fcntl.flock(holder,fcntl.LOCK_EX|fcntl.LOCK_NB)
            clock=Clock();polls=[]
            def sleep(seconds):
                clock.sleep(seconds);polls.append(seconds)
                if len(polls)==3:holder.close()  # The redemption finishes.
            inspect=MagicMock(side_effect=containers())
            with patch.object(interrupt.time,'monotonic',side_effect=clock),patch.object(interrupt.time,'sleep',side_effect=sleep),\
                 patch.object(interrupt,'inspect',inspect),patch.object(interrupt,'probe'),patch.object(interrupt,'writers'):
                b.preflight()
            try:
                self.assertEqual(len(polls),3);self.assertTrue(inspect.called)
                self.assertFalse(lock_available(b.state/'redemption.lock'))
                self.assertEqual(json.loads(b.journal.read_text())['phase'],'preflight')
            finally:b.release_redemption()

    def test_redemption_still_busy_after_bound_blocks_without_touching_production(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp));started_journal(b)
            holder=(b.state/'redemption.lock').open('rb');fcntl.flock(holder,fcntl.LOCK_EX|fcntl.LOCK_NB)
            clock=Clock()
            try:
                with patch.object(interrupt.time,'monotonic',side_effect=clock),patch.object(interrupt.time,'sleep',side_effect=clock.sleep),\
                     patch.object(interrupt,'inspect') as inspect,patch.object(interrupt,'probe') as probe,patch.object(interrupt,'run') as run:
                    with self.assertRaises(DeploymentBlocked) as raised:b.preflight()
                self.assertEqual(raised.exception.reason,'automatic_redemption_in_flight')
                self.assertGreaterEqual(clock.now-100.,interrupt.REDEMPTION_WAIT_SECONDS)
                inspect.assert_not_called();probe.assert_not_called();run.assert_not_called();b.reset_attempt.assert_not_called()
                self.assertIsNone(b.redemption);self.assertFalse(b.journal.exists())
            finally:holder.close()
            self.assertTrue(lock_available(b.state/'redemption.lock'))

    def test_host_network_unpinned_controller_or_short_budget_refuse_and_release_redemption(self):
        for change in ('host','unpinned','budget'):
            with self.subTest(change=change),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=make_backend(release_dir(tmp));started_journal(b)
                if change=='unpinned':b.m['controller_files'].pop('interrupt.py')
                # Still accepted by validate_timing, but too short for the longer pre-stop window.
                if change=='budget':b.m['timing']['candidate']=11.5
                with patch.object(interrupt,'inspect',side_effect=containers('host' if change=='host' else 'bridge')),patch.object(interrupt,'probe') as probe,patch.object(interrupt,'writers'):
                    with self.assertRaises(RuntimeError):b.preflight()
                probe.assert_not_called();self.assertIsNone(b.redemption);self.assertFalse(b.journal.exists())
                self.assertTrue(lock_available(b.state/'redemption.lock'))

    def test_long_interrupt_budget_keeps_rollback_reserve(self):
        for candidate,allowed in ((30,True),(40,False)):
            with self.subTest(candidate=candidate),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=make_backend(release_dir(tmp));started_journal(b)
                b.m['timing']={'candidate':candidate,'rollback':4,'stop':2,'budget':90,'recovery_total':8,'rollback_wait':31}
                with patch.object(interrupt,'inspect',side_effect=containers()),patch.object(interrupt,'probe'),patch.object(interrupt,'writers'):
                    if allowed:
                        b.preflight()
                        try:self.assertEqual(b.candidate_budget,90-35)
                        finally:b.release_redemption()
                    else:
                        with self.assertRaises(RuntimeError):b.preflight()
                        self.assertIsNone(b.redemption)

    def test_release_identity_checks_are_kept(self):
        cases={'container':lambda b,d:b.m.update(old_pid=7),'config':lambda b,d:(d/'config.yaml').write_text('drift\n'),
               'rehearsal':lambda b,d:atomic(d/'rehearsal.json',{}),'expired':lambda b,d:b.m.update(prepared_at=time.time()-90000),
               'controller':lambda b,d:b.m['controller_files'].update({'backend.py':'0'*64})}
        for name,change in cases.items():
            with self.subTest(name=name),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                d=release_dir(tmp);b=make_backend(d);started_journal(b);change(b,d)
                with patch.object(interrupt,'inspect',side_effect=containers()),patch.object(interrupt,'probe') as probe,patch.object(interrupt,'writers'):
                    with self.assertRaises(RuntimeError):b.preflight()
                probe.assert_not_called();self.assertIsNone(b.redemption)


class InterruptDrainTests(unittest.TestCase):
    def test_kill_expression_excludes_only_the_control_socket_like_maintenance(self):
        expression=interrupt.kill_expression([8085,8317],Control.exclude)
        self.assertEqual(expression,'( ( sport = :8085 or sport = :8317 ) and not ( src 127.0.0.1 and sport = :8317 and dst 127.0.0.1 and dport = :34567 ) )')
        m=maintenance.MaintenanceBackend.__new__(maintenance.MaintenanceBackend)
        m.drain_connection=Control();m.gates=MagicMock();m.m={'old_pid':42,'ports':[8085,8317]}
        m.idle=IdleWindow(now=lambda:1000.,immediate=True);m.evidence=lambda:{'healthy':True,'timestamp':1000.,'last_business':1000.,'pending':0}
        with patch.object(maintenance,'run') as run,patch.object(backend.Backend,'drained_and_quiet'):m.drained_and_quiet()
        self.assertEqual(run.call_args[0][0][-1],expression)

    def test_freezes_records_and_kills_until_only_control_remains(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp));b.started=100.;events=[];control=Control(active=3,events=events);b.drain_connection=control
            b.gates.freeze_idle_clients.side_effect=lambda exclude:events.append('freeze')
            counts=iter([5,2,0]);clock=Clock(100.)
            def connections(pid,ports,exclude=None):
                self.assertEqual((pid,ports),(42,[8085,8317]))
                if exclude is not None:
                    self.assertEqual(exclude,control.exclude);self.assertFalse(control.closed);return next(counts)
                return 0 if control.closed else 1
            def kill(args,**_):events.append('kill')
            with patch.object(interrupt,'connections',side_effect=connections),patch.object(interrupt,'run',side_effect=kill) as run,\
                 patch.object(interrupt.time,'monotonic',side_effect=clock),patch.object(interrupt.time,'sleep',side_effect=clock.sleep):
                b.drained_and_quiet()
            self.assertEqual(events,['freeze','activity','kill','kill'])
            command=run.call_args[0][0]
            self.assertEqual(command[:6],['nsenter','-t','42','-n','ss','-K']);self.assertEqual(command[6],interrupt.kill_expression([8085,8317],control.exclude))
            journal=json.loads(b.journal.read_text())
            self.assertEqual((journal['phase'],journal['interrupted_requests'],journal['interrupted_connections']),('gating',3,5))
            self.assertTrue(control.closed);self.assertIsNone(b.drain_connection)


class InterruptCutoverTests(unittest.TestCase):
    """Real gate/drain/checkpoint/stop/fence/start/recover with fake Docker and sockets."""
    def run_cutover(self,d,client_counts,candidate_ready=None):
        b=make_backend(d);b.candidate_budget=25.;clock=Clock(100.)
        b.preflight=MagicMock();b.watchdog=MagicMock();b.cancel_watchdog=MagicMock()
        b.candidate_ready=candidate_ready or MagicMock(side_effect=RuntimeError('candidate readiness failed'))
        atomic(b.journal,{'phase':'preflight','mode':'interrupt','attempt':1})
        control=Control(active=2);docker=[];kills=[]
        running={'old':True,'next':False};names={'old':'/cli-proxy-api','next':'/cli-proxy-api-idle-next-tok'}
        def inspect(name,optional=False):return {'Id':name,'Name':names[name],'State':{'Running':running[name],'Pid':42 if name=='old' else 43}}
        def run(args,**_):
            args=[str(a) for a in args];docker.append(args)
            if args[:2]==['docker','stop']:running[args[-1]]=False
            if args[:2]==['docker','start']:running[args[-1]]=True
            if args[:2]==['docker','rename']:names[args[2]]='/'+args[3]
            return b'{}'
        def connections(pid,ports,exclude=None):
            if exclude is None:return 0 if control.closed else 1
            return next(client_counts)
        with patch.object(interrupt,'DrainConnection',return_value=control),patch.object(interrupt,'connections',side_effect=connections),\
             patch.object(interrupt,'run',side_effect=lambda args,**_:kills.append(args)),patch.object(backend,'run',side_effect=run),\
             patch.object(backend,'inspect',side_effect=inspect),patch.object(backend,'writers'),\
             patch.object(interrupt.time,'monotonic',side_effect=clock),patch.object(interrupt.time,'sleep',side_effect=clock.sleep):
            with self.assertRaises(RuntimeError) as raised:Cutover(b).run()
        return b,raised.exception,docker,kills,control,running

    def test_connections_that_will_not_close_block_before_stop_and_retain_original(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b,error,docker,kills,control,running=self.run_cutover(release_dir(tmp),iter(lambda:1,None))
            self.assertIsInstance(error,DeploymentBlocked);self.assertEqual(error.reason,'client_connections_after_interrupt')
            self.assertGreater(len(kills),1);self.assertEqual(docker,[]);self.assertTrue(running['old'])
            journal=json.loads(b.journal.read_text())
            self.assertEqual((journal['phase'],journal['mode']),('original_retained','interrupt'));self.assertIn('ungated_at',journal)
            self.assertTrue(control.closed);b.gates.disable.assert_called_once();b.cancel_watchdog.assert_called_once()

    def test_failure_after_stop_rolls_back_to_original(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b,error,docker,kills,control,running=self.run_cutover(release_dir(tmp),iter([4,0]))
            self.assertEqual(str(error),'candidate readiness failed');self.assertEqual(len(kills),1)
            self.assertLess(docker.index(['docker','stop','--time','2','old']),docker.index(['docker','start','next']))
            self.assertIn(['docker','stop','--time','2','next'],docker);self.assertEqual(docker[-2][:3],['docker','start','old'])
            self.assertEqual(running,{'old':True,'next':False})
            journal=json.loads(b.journal.read_text())
            self.assertEqual((journal['phase'],journal['mode'],journal['interrupted_connections']),('rolled_back','interrupt',4))
            self.assertIn('ungated_at',journal);b.gates.disable.assert_called_once();b.cancel_watchdog.assert_called_once()


class LaunchTests(unittest.TestCase):
    def test_launch_runs_worker_in_its_own_control_group_unit(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            path=Path(tmp)/'release.json';atomic(path,{'token':'tok'})
            with patch.object(interrupt,'launch_controller') as guard:
                receipt=interrupt.launch(path)
            self.assertEqual(guard.call_args[0][:2],(path,'cliproxy-idle-deploy-tok'))
            args=guard.call_args[0][2]
            self.assertEqual(args[:6],['systemd-run','--quiet','--collect','--unit','cliproxy-idle-deploy-tok','--property=KillMode=control-group'])
            self.assertEqual(args[-3:],[Path(interrupt.__file__).resolve(),'worker',path])
            self.assertEqual((receipt['unit'],receipt['mode']),('cliproxy-idle-deploy-tok','interrupt'))


class DefaultModeUnchangedTests(unittest.TestCase):
    def test_backend_preflight_still_requires_zero_active_requests(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp),backend.Backend)
            with patch.object(backend,'inspect',side_effect=containers()),patch.object(backend,'activity',return_value=2),patch.object(backend,'probe') as probe:
                with self.assertRaises(DeploymentBlocked) as raised:b.preflight()
            self.assertEqual(raised.exception.reason,'active_requests');probe.assert_not_called();self.assertIsNone(b.redemption)

    def test_backend_preflight_still_fails_immediately_on_redemption(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp),backend.Backend)
            b.idle=IdleWindow(now=lambda:100.,immediate=True);b.evidence=lambda:{'healthy':True,'timestamp':100.,'last_business':100.,'pending':0}
            holder=(b.state/'redemption.lock').open('rb');fcntl.flock(holder,fcntl.LOCK_EX|fcntl.LOCK_NB)
            try:
                with patch.object(backend,'inspect',side_effect=containers()),patch.object(backend,'activity',return_value=0),patch.object(backend,'connections',return_value=0),\
                     patch.object(backend,'probe'),patch.object(backend,'writers'),patch.object(backend.time,'sleep') as sleep:
                    with self.assertRaises(DeploymentBlocked) as raised:b.preflight()
            finally:holder.close()
            self.assertEqual(raised.exception.reason,'automatic_redemption_in_flight');sleep.assert_not_called();self.assertFalse(b.journal.exists())

    def test_backend_gate_still_requires_zero_active_requests(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=make_backend(release_dir(tmp),backend.Backend)
            with patch.object(backend,'DrainConnection',return_value=Control(active=1)),patch.object(backend.time,'monotonic',return_value=100.):
                with self.assertRaises(DeploymentBlocked):b.gate()
            b.gates.enable.assert_not_called()

    def test_only_interrupt_checkpoint_has_the_longer_pre_stop_window(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d=release_dir(tmp);default=make_backend(d,backend.Backend);hard=make_backend(d);default.started=hard.started=100.
            with patch.object(backend.time,'monotonic',return_value=103.6):
                with self.assertRaises(RuntimeError):default.checkpoint()
                hard.checkpoint()
            with patch.object(backend.time,'monotonic',return_value=106.1):
                with self.assertRaises(RuntimeError):hard.checkpoint()

    def test_default_drain_still_requires_zero_active_requests(self):
        b=backend.Backend.__new__(backend.Backend);b.started=100.;b.candidate_budget=20.;b.m={'old_pid':42,'ports':[8317]}
        b.idle=IdleWindow(now=lambda:100.,immediate=True);b.evidence=lambda:{'healthy':True,'timestamp':100.,'last_business':100.,'pending':0}
        b.drain_connection=Control(active=1)
        with patch.object(backend.time,'monotonic',return_value=100.),patch.object(backend.time,'sleep'),patch.object(backend,'connections',return_value=0):
            with self.assertRaises(DeploymentBlocked):b.drained_and_quiet()


if __name__=='__main__':unittest.main()
