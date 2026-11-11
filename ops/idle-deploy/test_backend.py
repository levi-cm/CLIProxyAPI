import fcntl
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch,MagicMock

import backend
import gates
import main
import prepare
from core import IdleWindow
from common import ROOT,LOCKS,writers,lock_available,atomic


class WriterTests(unittest.TestCase):
    def test_all_writer_domains_are_excluded_without_writing_live_state(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            state=Path(tmp);(state/'history').mkdir()
            held=[]
            for name in LOCKS:
                p=state/name;p.touch();held.append(p.open('rb'))
            fcntl.flock(held[0],fcntl.LOCK_EX|fcntl.LOCK_NB);fcntl.flock(held[1],fcntl.LOCK_EX|fcntl.LOCK_NB)
            writers(state,True)
            self.assertTrue(lock_available(state/'redemption.lock'))
            with self.assertRaises(RuntimeError):writers(state,False)
            for f in held:f.close()
            writers(state,False)
    def test_gate_covers_both_families_host_root_and_namespace(self):
        with patch.object(gates,'run',return_value=b'-N DOCKER-USER\n'),patch.object(gates.subprocess,'run',return_value=MagicMock(returncode=0)):
            rules=list(gates.Gates('token',[8317],42,[42345]).rules())
        self.assertEqual(len(rules),8)
        for prefix,chain,match in rules:
            self.assertIn('--syn',match);self.assertNotIn('--uid-owner',match)
            self.assertIn('42345' if len(prefix)==1 else '8317',match)
        self.assertEqual({prefix[0] for prefix,_,_ in rules if len(prefix)==1},{'iptables','ip6tables'})


class RecoveryTests(unittest.TestCase):
    def fake(self,tmp,phase):
        b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.state=b.dir/'state';b.repo=b.dir;b.service='fixture-service';b.redemption=None
        b.m={'old_id':'old','candidate_id':'next','old_commit':'old-sha','timing':{'rollback':1}}
        b.state.mkdir();atomic(b.state/'conversation-bindings.json',{'records':[]})
        atomic(b.journal,{'phase':phase});(b.dir/'env.before').write_bytes(b'UNCHANGED=yes\n');(b.dir/'.env').write_bytes(b'UNCHANGED=yes\n')
        b.assert_no_writers=MagicMock();return b
    def test_pending_docker_stop_must_be_settled_before_restart(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=self.fake(tmp,'stopping_old');commands=[];state={'running':True}
            def inspect(name,*_):
                return {'Id':'old','Name':'/fixture-service','State':{'Running':state['running'],'Pid':77}} if name=='old' else None
            def run(args,**_):
                commands.append(args)
                if args[:2]==['docker','stop']:state['running']=False
                if args[:2]==['docker','start']:state['running']=True
                return b'{}'
            with patch.object(backend,'inspect',side_effect=inspect),patch.object(backend,'run',side_effect=run),patch.object(backend,'writers'):
                b.recover()
            self.assertIn(['docker','stop','--time','2','old'],commands)
            self.assertIn(['docker','start','old'],commands)
            self.assertEqual(json.loads(b.journal.read_text())['phase'],'rolled_back')
    def test_committed_candidate_is_never_stopped(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=self.fake(tmp,'committed')
            with patch.object(backend,'inspect',return_value={'State':{'Running':False}}),patch.object(backend,'run') as run:
                b.recover();run.assert_not_called()
    def test_stop_was_never_issued_retains_running_original(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=self.fake(tmp,'gating')
            with patch.object(backend,'inspect',return_value={'State':{'Running':True}}),patch.object(backend,'run') as run:
                b.recover();run.assert_not_called()
    def test_rescue_rereads_commit_after_worker_exit_before_rollback(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d=Path(tmp);path=d/'release.json';atomic(path,{'token':'test'})
            b=MagicMock();b.journal=d/'transaction.json';atomic(b.journal,{'phase':'starting_candidate'})
            def run(args,**_):
                # Simulate a worker committing just before its death. The durable
                # terminal state wins over the watchdog's initial observation.
                atomic(b.journal,{'phase':'committed'});return b''
            with patch.object(main,'Backend',return_value=b),patch.object(main,'run',side_effect=run),patch.object(main,'publish'),\
                    patch.object(main,'WATCHER_LOCK',d/'watcher.lock'):
                main.rescue(path)
            b.recover.assert_not_called();b.ungate.assert_called_once()
    def test_rescue_recovers_even_when_host_lock_is_held(self):
        import fcntl
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d=Path(tmp);path=d/'release.json';atomic(path,{'token':'test'})
            b=MagicMock();b.journal=d/'transaction.json';atomic(b.journal,{'phase':'starting_candidate'})
            with (d/'watcher.lock').open('a+') as other:
                fcntl.flock(other,fcntl.LOCK_EX|fcntl.LOCK_NB)
                with patch.object(main,'Backend',return_value=b),patch.object(main,'run',return_value=b''),patch.object(main,'publish'),\
                        patch.object(main,'WATCHER_LOCK',d/'watcher.lock'):
                    main.rescue(path)
            b.recover.assert_called_once();b.ungate.assert_called_once()
    def test_rescue_does_not_signal_already_committed_worker(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            d=Path(tmp);path=d/'release.json';atomic(path,{'token':'test'})
            b=MagicMock();b.journal=d/'transaction.json';atomic(b.journal,{'phase':'committed'})
            with patch.object(main,'Backend',return_value=b),patch.object(main,'run') as run:
                main.rescue(path)
            run.assert_not_called();b.recover.assert_not_called()


if __name__=='__main__':unittest.main()


class CleanupTests(unittest.TestCase):
    def test_admission_drops_new_connections_without_reset(self):
        g=gates.Gates('token',[8317,1455],42,[8317])
        for host in (False,True):
            rule=g.match(host)
            self.assertEqual(rule[-2:],['-j','DROP'])
            self.assertIn('--syn',rule)
            self.assertNotIn('REJECT',rule)
            self.assertNotIn('tcp-reset',rule)
        self.assertIn('8317',g.match(True))
        self.assertIn('1455,8317',g.match(False))
    def test_firewall_operational_failure_cannot_be_reported_as_ungated(self):
        failure=MagicMock(returncode=4)
        with patch.object(gates.subprocess,'run',return_value=failure),patch.object(gates,'run',return_value=b''):
            with self.assertRaises(RuntimeError):gates.Gates('token',[8317],42).disable()
    def test_surviving_firewall_token_cannot_be_reported_as_ungated(self):
        absent=MagicMock(returncode=1)
        with patch.object(gates.subprocess,'run',return_value=absent),patch.object(gates,'run',return_value=b'-A OUTPUT --comment cliproxy-idle-token -j DROP'):
            with self.assertRaises(RuntimeError):gates.Gates('token',[8317],42).disable()
    def test_missing_rules_are_idempotent_when_host_dump_is_clean(self):
        absent=MagicMock(returncode=1)
        with patch.object(gates.subprocess,'run',return_value=absent),patch.object(gates,'run',return_value=b''):
            gates.Gates('token',[8317],42).disable()
    def test_failed_cleanup_keeps_interruption_open(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.redemption=None;b.started=0
            atomic(b.journal,{'phase':'committed'})
            b.gates=MagicMock();b.gates.disable.side_effect=RuntimeError('cleanup failed')
            with self.assertRaises(RuntimeError):b.ungate()
            self.assertNotIn('ungated_at',json.loads(b.journal.read_text()))


class TerminalRetryTests(unittest.TestCase):
    def test_repeated_recovery_after_rollback_or_retention_only_cleans_admission(self):
        for phase in ('rolled_back','original_retained'):
            with self.subTest(phase=phase),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.redemption=None;b.m={'old_id':'old','candidate_id':'next'}
                atomic(b.journal,{'phase':phase})
                with patch.object(backend,'inspect') as inspect,patch.object(backend,'run') as run:
                    b.recover();b.recover();inspect.assert_not_called();run.assert_not_called()
    def test_rescue_terminal_rollback_never_signals_worker_or_stops_router(self):
        for phase in ('rolled_back','original_retained'):
            with self.subTest(phase=phase),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                d=Path(tmp);path=d/'release.json';atomic(path,{'token':'test'});b=MagicMock();b.journal=d/'transaction.json';atomic(b.journal,{'phase':phase})
                with patch.object(main,'Backend',return_value=b),patch.object(main,'run') as run:
                    main.rescue(path)
                run.assert_not_called();b.recover.assert_not_called();b.ungate.assert_called_once()


class AttemptTests(unittest.TestCase):
    def test_finished_attempt_clears_rescue_marker_before_retry(self):
        for phase in ('committed','rolled_back','original_retained'):
            with self.subTest(phase=phase),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture'
                atomic(b.journal,{'phase':phase,'ungated_at':12,'attempt':1})
                marker=b.dir/'rescue-requested';marker.touch()
                with patch.object(backend,'run',return_value=b'inactive\ninactive\n'):b.reset_attempt()
                self.assertFalse(marker.exists())
                self.assertEqual(json.loads(b.journal.read_text())['attempt'],2)
    def test_unfinished_attempt_keeps_rescue_marker(self):
        for journal in ({'phase':'recovering'},{'phase':'original_retained'},{'phase':'original_retained','ungated_at':0}):
            with self.subTest(journal=journal),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture'
                atomic(b.journal,journal);marker=b.dir/'rescue-requested';marker.touch()
                with patch.object(backend,'run',return_value=b'inactive\ninactive\n'):
                    with self.assertRaises(RuntimeError):b.reset_attempt()
                self.assertTrue(marker.exists());self.assertEqual(json.loads(b.journal.read_text()),journal)
    def test_running_or_unverified_watchdog_keeps_finished_attempt_and_marker(self):
        for status in (b'inactive\nactive\n',b'inactive\n',b'',b'inactive\nunrecognized\n'):
            with self.subTest(status=status),tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
                b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture'
                old={'phase':'original_retained','ungated_at':12};atomic(b.journal,old)
                marker=b.dir/'rescue-requested';marker.touch()
                with patch.object(backend,'run',return_value=status):
                    with self.assertRaises(RuntimeError):b.reset_attempt()
                self.assertTrue(marker.exists());self.assertEqual(json.loads(b.journal.read_text()),old)
    def test_orphaned_rescue_marker_requires_restaging(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture'
            marker=b.dir/'rescue-requested';marker.touch()
            with patch.object(backend,'run',return_value=b'inactive\ninactive\n'):
                with self.assertRaises(RuntimeError):b.reset_attempt()
            self.assertTrue(marker.exists());self.assertFalse(b.journal.exists())
    def test_retry_clears_prior_timing_and_retains_previous_evidence(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture';b.started=4
            atomic(b.journal,{'phase':'original_retained','ungated_at':12,'interruption_seconds':8,'rescue_started':10,'attempt':1})
            with patch.object(backend,'run',return_value=b'inactive\ninactive\n'):b.reset_attempt()
            data=json.loads(b.journal.read_text());self.assertEqual(data['attempt'],2)
            for key in ('ungated_at','interruption_seconds','rescue_started','started'):self.assertNotIn(key,data)
            self.assertEqual(len(list((b.dir/'attempts').glob('*.json'))),1);self.assertIsNone(b.started)
    def test_active_prior_watchdog_cannot_share_a_new_attempt(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.timer='fixture'
            with patch.object(backend,'run',return_value=b'inactive\nactive\n'):
                with self.assertRaises(RuntimeError):b.reset_attempt()
            self.assertFalse(b.journal.exists())
    def test_gate_discovery_failure_is_not_treated_as_missing_ipv6_chain(self):
        with patch.object(gates.subprocess,'run',return_value=MagicMock(returncode=4)):
            with self.assertRaises(RuntimeError):list(gates.Gates('test',[8317],1).rules())


class FinalDrainTests(unittest.TestCase):
    class Control:
        exclude=(('127.0.0.1',8317),('127.0.0.1',41234))
        def __init__(self,active=0):self.active=active;self.closed=False;self.queried=False
        def activity(self):self.queried=True;return self.active
        def close(self):self.closed=True

    def make_backend(self,active=0):
        b=backend.Backend.__new__(backend.Backend);b.started=100.;b.candidate_budget=20.;b.m={'old_pid':123,'ports':[8317]}
        b.idle=IdleWindow(now=lambda:100.,immediate=True)
        b.evidence=lambda:{'healthy':True,'timestamp':100.,'last_business':100.,'pending':0}
        b.drain_connection=self.Control(active);return b

    def test_immediate_final_drain_rejects_remaining_request_lease(self):
        b=self.make_backend(1);control=b.drain_connection
        with patch.object(backend.time,'monotonic',return_value=100.),patch.object(backend.time,'sleep'),patch.object(backend,'connections',return_value=0):
            with self.assertRaises(RuntimeError):b.drained_and_quiet()
        self.assertTrue(control.queried);self.assertTrue(control.closed)

    def test_only_verified_control_socket_is_excluded_then_closed(self):
        b=self.make_backend();control=b.drain_connection
        clock={'now':100.}
        def sleep(seconds):clock['now']+=seconds
        def connections(pid,ports,exclude=None):
            self.assertEqual((pid,ports),(123,[8317]))
            if exclude is not None:
                self.assertEqual(exclude,control.exclude);self.assertFalse(control.closed);return 0
            return 0 if control.closed else 1
        with patch.object(backend.time,'monotonic',side_effect=lambda:clock['now']),patch.object(backend.time,'sleep',side_effect=sleep),patch.object(backend,'connections',side_effect=connections):
            b.drained_and_quiet()
        self.assertTrue(control.queried);self.assertTrue(control.closed);self.assertIsNone(b.drain_connection)

    def test_missing_established_control_socket_aborts_final_drain(self):
        b=self.make_backend();control=b.drain_connection
        def connections(pid,ports,exclude=None):
            if exclude is not None:raise RuntimeError('exact established control socket missing')
            return 0
        with patch.object(backend.time,'monotonic',return_value=100.),patch.object(backend.time,'sleep'),patch.object(backend,'connections',side_effect=connections):
            with self.assertRaises(RuntimeError):b.drained_and_quiet()
        self.assertTrue(control.closed)


class FixtureFinalDrainTests(FinalDrainTests):
    def setUp(self):
        redirect=patch('common.connections',side_effect=lambda *args,**kwargs:backend.connections(*args,**kwargs))
        redirect.start();self.addCleanup(redirect.stop)
    def make_backend(self,active=0):
        b=super().make_backend(active);b.__class__=prepare.FixtureBackend;return b


class DrainConnectionTests(unittest.TestCase):
    class Socket:
        def fileno(self):return 17
        def getpeername(self):return ('127.0.0.1',8317)
        def getsockname(self):return ('127.0.0.1',41234)

    class Response:
        status=200;will_close=False
        def __init__(self,available=True,counts=(0,0)):
            self.body=json.dumps({'activity':{'available':available,'accounts':[{'active_requests':count} for count in counts]}}).encode()
        def read(self):return self.body

    class Transport:
        def __init__(self,response):self.sock=DrainConnectionTests.Socket();self.response=response;self.requests=[];self.closed=False;self.auto_open=True
        def connect(self):pass
        def request(self,method,path,headers):self.requests.append((method,path,headers))
        def getresponse(self):return self.response
        def close(self):self.closed=True;self.sock=None

    def control(self,response=None):
        transport=self.Transport(response or self.Response())
        control=backend.DrainConnection.__new__(backend.DrainConnection)
        control.connection=transport;control.socket=transport.sock;control.headers={'Authorization':'Bearer fixture-management-key'}
        return control,transport

    def test_activity_uses_authenticated_existing_socket(self):
        control,transport=self.control(self.Response(counts=(0,2,3)))
        self.assertEqual(control.activity(),5)
        self.assertEqual(transport.requests,[('GET','/v8/management/account-policy/dashboard?range=1h',{'Authorization':'Bearer fixture-management-key'})])

    def test_unavailable_or_invalid_accounting_blocks_cutover(self):
        for response in (self.Response(available=False),self.Response(counts=(-1,)),self.Response(counts=(True,)),self.Response(counts=('0',))):
            with self.subTest(response=response.body):
                control,_=self.control(response)
                with self.assertRaises(RuntimeError):control.activity()

    def test_closed_or_replaced_management_socket_cannot_reconnect(self):
        for replacement in (None,self.Socket()):
            with self.subTest(replacement=replacement):
                control,transport=self.control();transport.sock=replacement
                with self.assertRaises(RuntimeError):control.activity()
                self.assertEqual(transport.requests,[])

    def test_http_failure_or_nonpersistent_socket_blocks_cutover(self):
        for status,will_close in ((401,False),(200,True)):
            with self.subTest(status=status,will_close=will_close):
                response=self.Response();response.status=status;response.will_close=will_close
                control,_=self.control(response)
                with self.assertRaises(RuntimeError):control.activity()

    def test_namespace_restored_and_reconnection_disabled_after_connect(self):
        transport=self.Transport(self.Response());original=MagicMock();target=MagicMock()
        original.__enter__.return_value=original;original.fileno.return_value=10
        target.__enter__.return_value=target;target.fileno.return_value=11
        namespaces=[]
        with patch.object(backend,'key',return_value='fixture-management-key'),patch.object(backend.http.client,'HTTPConnection',return_value=transport),patch.object(backend.Path,'open',side_effect=(original,target)),patch.object(backend.os,'setns',side_effect=lambda fd,kind:namespaces.append(fd)):
            control=backend.DrainConnection(123)
        self.assertEqual(namespaces,[11,10]);self.assertFalse(transport.auto_open)
        self.assertEqual(control.exclude,(('127.0.0.1',8317),('127.0.0.1',41234)))

    def test_failed_connect_restores_namespace_and_closes_socket(self):
        transport=self.Transport(self.Response());original=MagicMock();target=MagicMock()
        original.__enter__.return_value=original;original.fileno.return_value=10
        target.__enter__.return_value=target;target.fileno.return_value=11
        namespaces=[]
        with patch.object(backend,'key',return_value='fixture-management-key'),patch.object(backend.http.client,'HTTPConnection',return_value=transport),patch.object(backend.Path,'open',side_effect=(original,target)),patch.object(backend.os,'setns',side_effect=lambda fd,kind:namespaces.append(fd)),patch.object(transport,'connect',side_effect=OSError('fixture connect failure')):
            with self.assertRaises(OSError):backend.DrainConnection(123)
        self.assertEqual(namespaces,[11,10]);self.assertTrue(transport.closed)

    def test_cancel_during_namespace_entry_still_restores_original(self):
        transport=self.Transport(self.Response());original=MagicMock();target=MagicMock()
        original.__enter__.return_value=original;original.fileno.return_value=10
        target.__enter__.return_value=target;target.fileno.return_value=11
        namespaces=[]
        def setns(fd,kind):
            namespaces.append(fd)
            if fd==11:raise KeyboardInterrupt()
        with patch.object(backend,'key',return_value='fixture-management-key'),patch.object(backend.http.client,'HTTPConnection',return_value=transport),patch.object(backend.Path,'open',side_effect=(original,target)),patch.object(backend.os,'setns',side_effect=setns):
            with self.assertRaises(KeyboardInterrupt):backend.DrainConnection(123)
        self.assertEqual(namespaces,[11,10]);self.assertTrue(transport.closed)

    def test_failed_namespace_restore_ends_worker_for_independent_rescue(self):
        transport=self.Transport(self.Response());original=MagicMock();target=MagicMock()
        original.__enter__.return_value=original;original.fileno.return_value=10
        target.__enter__.return_value=target;target.fileno.return_value=11
        def setns(fd,kind):
            if fd==10:raise OSError('fixture restoration failure')
        with patch.object(backend,'key',return_value='fixture-management-key'),patch.object(backend.http.client,'HTTPConnection',return_value=transport),patch.object(backend.Path,'open',side_effect=(original,target)),patch.object(backend.os,'setns',side_effect=setns),patch.object(backend.os,'_exit',side_effect=SystemExit(1)):
            with self.assertRaises(SystemExit):backend.DrainConnection(123)

    def test_connection_setup_abort_can_finish_cleanup_and_retry(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as tmp:
            b=backend.Backend.__new__(backend.Backend);b.dir=Path(tmp);b.journal=b.dir/'transaction.json';b.m={'old_pid':123};b.timer='fixture'
            b.started=None;b.redemption=None;b.drain_connection=None;b.gates=MagicMock()
            with patch.object(backend,'DrainConnection',side_effect=RuntimeError('fixture unavailable')),patch.object(backend.time,'monotonic',return_value=100.):
                with self.assertRaises(RuntimeError):b.gate()
                b.phase('original_retained');b.ungate()
            transaction=json.loads(b.journal.read_text());self.assertEqual(transaction['ungated_at'],100.)
            with patch.object(backend,'run',return_value=b'inactive\ninactive\n'):b.reset_attempt()
            self.assertEqual(json.loads(b.journal.read_text())['phase'],'preflight')
