"""Deterministic namespace TCP accounting; never read live /proc state."""
import unittest
from pathlib import Path
from unittest.mock import patch

from common import connections


HEADER = '  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n'
CONTROL = (('127.0.0.1', 8317), ('127.0.0.1', 45000))


def row(local, remote, state='01'):
    return f'  0: {local} {remote} {state} 00000000:00000000 00:00000000 00000000 1000 0 123 1\n'


class ConnectionTests(unittest.TestCase):
    def count(self, tcp='', tcp6='', exclude=None):
        fixture = {'/proc/42/net/tcp': HEADER + tcp, '/proc/42/net/tcp6': HEADER + tcp6}
        with patch('common.Path.read_text', autospec=True, side_effect=lambda path: fixture[str(path)]):
            return connections(42, (8317,), exclude=exclude)

    def test_legacy_accounting_keeps_other_active_states_and_ignores_closed(self):
        tcp = ''.join(row('0100007F:207D', '0100007F:AFC8', state) for state in ('01', '03', '08', '06', '07', '0A'))
        tcp += row('0100007F:1234', '0100007F:AFC8')
        self.assertEqual(self.count(tcp), 3)

    def test_excludes_only_the_exact_established_server_tuple(self):
        tcp = row('0100007F:207D', '0100007F:AFC8')
        tcp += row('0100007F:207D', '0100007F:AFC9')
        tcp += row('0200007F:207D', '0100007F:AFC8')
        tcp += row('0100007F:AFC8', '0100007F:207D')
        self.assertEqual(self.count(tcp, exclude=CONTROL), 2)

    def test_missing_control_tuple_fails_closed(self):
        with self.assertRaises(RuntimeError):
            self.count(row('0100007F:207D', '0100007F:AFC9'), exclude=CONTROL)

    def test_non_established_exact_tuple_cannot_be_excluded(self):
        for state in ('02', '03', '06', '07', '08', '0A'):
            with self.subTest(state=state), self.assertRaises(RuntimeError):
                self.count(row('0100007F:207D', '0100007F:AFC8', state), exclude=CONTROL)

    def test_duplicate_exact_tuple_fails_closed(self):
        tcp = row('0100007F:207D', '0100007F:AFC8') * 2
        with self.assertRaises(RuntimeError):
            self.count(tcp, exclude=CONTROL)

    def test_ipv6_native_word_byte_order_and_compressed_address(self):
        # 2001:db8::1 and 2001:db8::abcd as Linux little-endian u32 words.
        tcp6 = row('B80D0120000000000000000001000000:207D', 'B80D01200000000000000000CDAB0000:AFC8')
        tcp6 += row('B80D0120000000000000000001000000:207D', 'B80D01200000000000000000CEAB0000:AFC8')
        self.assertEqual(self.count(tcp6=tcp6, exclude=(('2001:db8:0:0:0:0:0:1', 8317), ('2001:db8::abcd', 45000))), 1)

    def test_ipv4_mapped_ipv6_matches_the_exact_ipv4_identity(self):
        tcp6 = row('0000000000000000FFFF00000100007F:207D', '0000000000000000FFFF00000100007F:AFC8')
        self.assertEqual(self.count(tcp6=tcp6, exclude=CONTROL), 0)
        tcp = row('0100007F:207D', '0100007F:AFC8')
        mapped = (('::ffff:127.0.0.1', 8317), ('::ffff:127.0.0.1', 45000))
        self.assertEqual(self.count(tcp=tcp, exclude=mapped), 0)

    def test_ambiguous_ipv4_and_mapped_rows_fail_closed(self):
        tcp = row('0100007F:207D', '0100007F:AFC8')
        tcp6 = row('0000000000000000FFFF00000100007F:207D', '0000000000000000FFFF00000100007F:AFC8')
        with self.assertRaises(RuntimeError):
            self.count(tcp, tcp6, CONTROL)

    def test_ipv6_loopback_is_not_guessed_to_be_ipv4_loopback(self):
        tcp6 = row('00000000000000000000000001000000:207D', '00000000000000000000000001000000:AFC8')
        with self.assertRaises(RuntimeError):
            self.count(tcp6=tcp6, exclude=CONTROL)

    def test_wrong_server_direction_or_port_is_not_excluded(self):
        tcp = row('0100007F:207D', '0100007F:AFC8')
        for exclude in (CONTROL[::-1], (('127.0.0.1', 8318), CONTROL[1])):
            with self.subTest(exclude=exclude), self.assertRaises(RuntimeError):
                self.count(tcp, exclude=exclude)

    def test_malformed_exclusion_fails_closed(self):
        for exclude in ((), [], [CONTROL[0], CONTROL[1]], (CONTROL[0],),
                        (('localhost', 8317), CONTROL[1]),
                        (('127.0.0.1', '8317'), CONTROL[1]),
                        (('127.0.0.1', True), CONTROL[1]),
                        (('127.0.0.1', 0), CONTROL[1]),
                        (('127.0.0.1', 65536), CONTROL[1]),
                        (('::1%lo', 8317), ('::1%lo', 45000)),
                        (('::1', 8317, 0, 0), ('::1', 45000, 0, 0))):
            with self.subTest(exclude=exclude), self.assertRaises(RuntimeError):
                self.count(exclude=exclude)

    def test_malformed_target_row_cannot_satisfy_exclusion(self):
        with self.assertRaises(RuntimeError):
            self.count(row('NOTANIP:207D', '0100007F:AFC8'), exclude=CONTROL)


if __name__ == '__main__':
    unittest.main()


class SingleFlightTests(unittest.TestCase):
    """Real flock calls on private lock files; never the production lock."""

    def setUp(self):
        import tempfile
        self.dir = tempfile.TemporaryDirectory(dir='/DATA/AppData/CLIProxyAPI/.tmp')
        self.addCleanup(self.dir.cleanup)
        self.lock = self.dir.name + '/watcher.lock'
        self.launch_lock = self.dir.name + '/launch.lock'
        self.release = self.dir.name + '/rel/release.json'
        Path(self.release).parent.mkdir()

    def hold(self, path=None):
        import fcntl
        f = open(path or self.lock, 'a+')
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.addCleanup(f.close)
        return f

    def clock(self):
        ticks = iter(range(10000))
        return lambda: next(ticks)

    def ack(self, invocation):
        import json
        Path(self.release).with_name('controller-ack.json').write_text(json.dumps({'invocation_id': invocation, 'pid': 1}))

    def test_probe_releases_a_free_lock(self):
        from common import single_flight_held
        self.assertFalse(single_flight_held(self.lock))
        self.hold()  # the probe must not have kept the lock
        self.assertTrue(single_flight_held(self.lock))

    def test_worker_tolerates_a_momentary_probe(self):
        import fcntl
        from common import acquire_single_flight
        probe = self.hold()
        calls = []

        def sleep(_):
            calls.append(1)
            if len(calls) == 2:
                fcntl.flock(probe, fcntl.LOCK_UN)
        with open(self.lock, 'a+') as worker:
            acquire_single_flight(worker, wait=50, clock=self.clock(), sleep=sleep)
            self.assertTrue(len(calls) >= 2)

    def test_worker_exits_behind_a_long_lived_controller(self):
        from common import acquire_single_flight
        self.hold()
        with open(self.lock, 'a+') as worker, self.assertRaises(RuntimeError):
            acquire_single_flight(worker, wait=3, clock=self.clock(), sleep=lambda _: None)

    def test_confirmation_requires_this_invocation(self):
        from common import confirm_controller
        self.ack('mine')
        self.assertTrue(confirm_controller(self.release, 'u', clock=self.clock(), sleep=lambda _: None, state=lambda: 'active', invocation=lambda: 'mine'))

    def test_foreign_lock_holder_or_stale_ack_is_not_success(self):
        from common import confirm_controller
        self.hold()  # an unrelated controller holds the host lock
        self.ack('someone-else')
        states = []

        def state():
            states.append(1)
            return 'failed'
        with self.assertRaises(RuntimeError):
            confirm_controller(self.release, 'u', clock=self.clock(), sleep=lambda _: None, state=state, invocation=lambda: 'mine')
        self.assertTrue(states, 'the launched unit state must be checked')
        with self.assertRaises(RuntimeError):
            confirm_controller(self.release, 'u', timeout=3, clock=self.clock(), sleep=lambda _: None, state=lambda: 'active', invocation=lambda: 'mine')
        with self.assertRaises(RuntimeError):
            confirm_controller(self.release, 'u', clock=self.clock(), sleep=lambda _: None, state=lambda: 'active', invocation=lambda: '')

    def launch(self, run, recovery=''):
        import common
        with patch.object(common, 'run', side_effect=run), patch.object(common, 'active_recovery_units', return_value=[recovery] if recovery else []):
            return common.launch_controller(self.release, 'u', ['systemd-run'], launch_lock=self.launch_lock, watcher_lock=self.lock,
                                            clock=self.clock(), sleep=lambda _: None, state=lambda: 'active', invocation=lambda: 'mine')

    def test_launch_starts_unit_and_waits_for_its_ack(self):
        started = []

        def run(args, **_):
            started.append(args)
            self.ack('mine')
            return b''
        self.assertTrue(self.launch(run))
        self.assertEqual(started, [['systemd-run']])

    def test_competing_launch_is_refused_before_starting_a_unit(self):
        import fcntl
        started = []

        def first(args, **_):
            started.append('first')
            worker = open(self.lock, 'a+')
            fcntl.flock(worker, fcntl.LOCK_EX | fcntl.LOCK_NB)  # first worker now runs
            self.addCleanup(worker.close)
            self.ack('mine')
            return b''
        self.launch(first)

        def second(args, **_):
            started.append('second')
            return b''
        with self.assertRaises(RuntimeError):
            self.launch(second)
        self.assertEqual(started, ['first'])

    def test_live_watchdog_refuses_launch(self):
        started = []
        with self.assertRaises(RuntimeError):
            self.launch(lambda args, **_: started.append(args), recovery='cliproxy-idle-watchdog-x.timer')
        self.assertEqual(started, [])

    def test_stale_ack_is_removed_before_launch(self):
        self.ack('old')

        def run(args, **_):
            self.assertFalse(Path(self.release).with_name('controller-ack.json').exists())
            self.ack('mine')
            return b''
        self.launch(run)

    def test_recovery_unit_listing(self):
        from common import active_recovery_units
        listing = ('cliproxy-idle-watchdog-a.timer loaded active waiting x\n'
                   'cliproxy-idle-watchdog-b.service loaded inactive dead x\n'
                   'cliproxy-idle-watchdog-c.service loaded activating start x\n')
        self.assertEqual(active_recovery_units(listing), ['cliproxy-idle-watchdog-a.timer', 'cliproxy-idle-watchdog-c.service'])
