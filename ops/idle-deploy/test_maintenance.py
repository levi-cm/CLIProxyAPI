import unittest
from unittest.mock import MagicMock,patch
from maintenance import MaintenanceBackend
from backend import Backend


class MaintenanceTests(unittest.TestCase):
    def test_only_idle_connection_requirement_is_relaxed(self):
        b=MaintenanceBackend.__new__(MaintenanceBackend)
        b.m={'old_id':'old'}
        with patch.object(Backend,'preflight') as check,patch('maintenance.inspect',return_value={'HostConfig':{'NetworkMode':'bridge'}}):
            b.preflight()
        check.assert_called_once_with(require_no_connections=False)

    def test_freezes_existing_clients_before_idle_disconnect_then_uses_full_drain(self):
        b=MaintenanceBackend.__new__(MaintenanceBackend)
        b.drain_connection=MagicMock();b.drain_connection.exclude=(('127.0.0.1',8317),('127.0.0.1',34567))
        b.drain_connection.activity.return_value=0;b.gates=MagicMock();b.m={'old_pid':42,'ports':[8317,8085]}
        from core import IdleWindow
        b.idle=IdleWindow(now=lambda:1000.,immediate=True);b.evidence=lambda:{'healthy':True,'timestamp':1000.,'last_business':1000.,'pending':0}
        with patch('maintenance.run') as run,patch.object(Backend,'drained_and_quiet') as drain:
            b.drained_and_quiet()
        b.gates.freeze_idle_clients.assert_called_once_with(b.drain_connection.exclude)
        self.assertIn('ss',run.call_args[0][0]);self.assertIn('34567',run.call_args[0][0][-1])
        drain.assert_called_once()

    def test_active_lease_retains_original_without_disconnecting_clients(self):
        b=MaintenanceBackend.__new__(MaintenanceBackend)
        b.drain_connection=MagicMock();b.drain_connection.activity.return_value=1;b.gates=MagicMock()
        with patch('maintenance.run') as run:
            with self.assertRaises(RuntimeError):b.drained_and_quiet()
        run.assert_not_called()

    def test_lease_appearing_after_freeze_prevents_disconnect(self):
        now=[1000.];b=self.waiting_backend(now);now[0]=1301.
        b.drain_connection.activity.side_effect=[0,1]
        with patch('maintenance.run') as run:
            with self.assertRaises(RuntimeError):b.drained_and_quiet()
        b.gates.freeze_idle_clients.assert_called_once()
        run.assert_not_called()

    def waiting_backend(self,now):
        from core import IdleWindow
        b=MaintenanceBackend.__new__(MaintenanceBackend)
        b.drain_connection=MagicMock();b.drain_connection.exclude=(('127.0.0.1',8317),('127.0.0.1',34567))
        b.drain_connection.activity.return_value=0;b.gates=MagicMock();b.m={'old_pid':42,'ports':[8317]}
        b.idle=IdleWindow(now=lambda:now[0]);b.evidence=lambda:{'healthy':True,'timestamp':now[0],'last_business':1000.,'pending':0}
        return b

    def test_new_activity_before_freeze_prevents_idle_disconnect(self):
        now=[1000.];b=self.waiting_backend(now);now[0]=1301.
        b.evidence=lambda:{'healthy':True,'timestamp':now[0],'last_business':now[0],'pending':0}
        with patch('maintenance.run') as run,patch.object(Backend,'drained_and_quiet'):
            with self.assertRaises(RuntimeError):b.drained_and_quiet()
        run.assert_not_called()

    def test_expected_retirement_reset_keeps_capture_and_lease_gates(self):
        now=[1000.];b=self.waiting_backend(now);now[0]=1301.
        evidence={'healthy':True,'timestamp':now[0],'last_business':1000.,'pending':0};b.evidence=lambda:evidence
        def disconnect(*args,**kwargs):evidence['last_business']=now[0]
        def drain():
            self.assertTrue(b.idle.ready(evidence))
            self.assertFalse(b.idle.ready(dict(evidence,healthy=False)))
            self.assertFalse(b.idle.ready(dict(evidence,pending=1)))
            self.assertFalse(b.idle.ready(dict(evidence,timestamp=now[0]-4)))
        with patch('maintenance.run',side_effect=disconnect),patch.object(Backend,'drained_and_quiet',side_effect=drain):
            b.drained_and_quiet()


if __name__=='__main__':unittest.main()

class FixtureRetirementTests(unittest.TestCase):
    def test_fixture_preflight_accepts_the_same_retirement_gate_as_production(self):
        import prepare
        import tempfile
        from pathlib import Path
        class FixtureMaintenanceBackend(MaintenanceBackend,prepare.FixtureBackend):
            pass
        with tempfile.TemporaryDirectory() as tmp:
            b=FixtureMaintenanceBackend.__new__(FixtureMaintenanceBackend)
            b.repo=Path(tmp);b.state=Path(tmp);(b.state/'redemption.lock').touch();b.redemption=None
            b.m={'fixture_repo':tmp,'old_id':'fixture','fixture_candidate_budget':15}
            b.snapshot=MagicMock();b.phase=MagicMock()
            try:
                with patch('maintenance.inspect',return_value={'HostConfig':{'NetworkMode':'none'}}),patch('prepare.inspect',return_value={'State':{'Running':True}}),patch('prepare.writers'):
                    try:b.preflight()
                    except TypeError as error:self.fail('fixture retirement preflight must support the production gate: '+str(error))
                b.snapshot.assert_called_once()
            finally:b.release_redemption()
