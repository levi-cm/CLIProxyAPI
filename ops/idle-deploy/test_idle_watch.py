import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import main
import maintenance
from core import IdleWindow


class IdleWatchTests(unittest.TestCase):
    def test_blocking_reason_survives_a_new_status_heartbeat(self):
        now=[1000.]
        idle=IdleWindow(now=lambda:now[0]);now[0]=1301.
        evidence={'healthy':True,'timestamp':1301.,'last_business':1000.,'pending':0,'capture_drops':7}
        block={'reason':'client_connections','client_connections':2,'at':1300.}
        self.assertTrue(hasattr(main,'watch_status'),'watch status must retain blocking evidence')
        status=main.watch_status(idle,evidence,last_block=block,retire_idle_clients=False)
        self.assertEqual(status['phase'],'blocked')
        self.assertEqual(status['last_block'],block)
        self.assertEqual(status['capture_drops'],7)
        self.assertEqual(status['idle_required_seconds'],300)
        now[0]=1302.;evidence.update(timestamp=1302.,last_business=1302.)
        following=main.watch_status(idle,evidence,last_block=block,retire_idle_clients=False)
        self.assertEqual(following['phase'],'waiting_for_idle')
        self.assertEqual(following['last_block'],block)

    def test_retirement_is_selected_only_by_the_prepared_manifest(self):
        for enabled in (False,True):
            with self.subTest(enabled=enabled),tempfile.TemporaryDirectory() as tmp:
                path=Path(tmp)/'release.json';path.write_text(json.dumps({'retire_idle_clients':enabled}))
                with patch.object(main,'Backend') as regular,patch.object(maintenance,'MaintenanceBackend') as retirement:
                    self.assertTrue(hasattr(main,'deployment_backend'),'backend selection must honor the prepared idle-client policy')
                    main.deployment_backend(path,'evidence','idle')
                (retirement if enabled else regular).assert_called_once_with(path,'evidence','idle')
                (regular if enabled else retirement).assert_not_called()

    def test_launch_reports_the_pinned_retirement_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'release.json';path.write_text(json.dumps({'token':'fixture','candidate_id':'candidate','retire_idle_clients':True}))
            with patch.object(main,'inspect',return_value={'State':{'Running':False}}),patch.object(main,'run'),\
                    patch.object(main,'require_no_active_deployment') as guard,patch.object(main,'confirm_controller') as confirm:
                receipt=main.launch(path)
            guard.assert_called_once_with();confirm.assert_called_once_with('cliproxy-idle-deploy-fixture')
            self.assertTrue(receipt.get('single_flight_lock_held'))
            self.assertTrue(receipt.get('retire_idle_clients'))
            self.assertEqual(receipt['idle_seconds'],300)


class SingleFlightLaunchTests(unittest.TestCase):
    def test_launch_refuses_while_another_controller_is_active(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'release.json';path.write_text(json.dumps({'token':'fixture','candidate_id':'candidate'}))
            with patch.object(main,'inspect',return_value={'State':{'Running':False}}),patch.object(main,'run') as run,\
                    patch.object(main,'require_no_active_deployment',side_effect=RuntimeError('held')):
                with self.assertRaises(RuntimeError):main.launch(path)
            run.assert_not_called()
