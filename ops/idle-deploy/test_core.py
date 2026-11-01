import unittest
from core import IdleWindow, validate_timing, Cutover


class Clock:
    def __init__(self): self.now = 1000.0
    def __call__(self): return self.now


class Backend:
    def __init__(self, fail=None): self.events=[]; self.fail=fail; self.old_running=True; self.candidate_running=False; self.committed=False
    def event(self,name):
        self.events.append(name)
        if self.fail==name: raise RuntimeError(name)
    def preflight(self): self.event('preflight')
    def watchdog(self): self.event('watchdog')
    def gate(self): self.event('gate')
    def drained_and_quiet(self): self.event('drain')
    def checkpoint(self): self.event('checkpoint')
    def stop_old(self): self.event('stop_old'); self.old_running=False
    def assert_no_writers(self): self.event('no_writers')
    def start_candidate(self): self.event('start_candidate'); self.candidate_running=True
    def candidate_ready(self): self.event('ready')
    def preserve_state(self): self.event('preserve_state')
    def commit(self): self.event('commit'); self.committed=True
    def ungate(self): self.events.append('ungate')
    def cancel_watchdog(self): self.events.append('cancel_watchdog')
    def recover(self):
        self.events.append('recover')
        if self.old_running: return
        self.candidate_running=False; self.old_running=True


class IdleTests(unittest.TestCase):
    def test_explicit_immediate_mode_keeps_capture_safety(self):
        c=Clock(); w=IdleWindow(now=c,immediate=True)
        e={'healthy':True,'timestamp':c.now,'last_business':c.now,'pending':0}
        self.assertTrue(w.ready(e))
        for change in ({'healthy':False},{'pending':1},{'timestamp':c.now-4},{'timestamp':c.now+1},{'last_business':c.now+1}):
            self.assertFalse(w.ready(dict(e,**change)))
        with self.assertRaises(ValueError):IdleWindow(0,c)
    def test_needs_five_observed_minutes_and_fresh_capture(self):
        c=Clock(); w=IdleWindow(300,c)
        evidence={'healthy':True,'timestamp':1000,'last_business':900,'pending':0}
        self.assertFalse(w.ready(evidence))
        c.now=1300; evidence.update(timestamp=1300)
        self.assertTrue(w.ready(evidence))
        c.now=1304; self.assertFalse(w.ready(evidence))
    def test_new_requests_capture_failure_and_pending_block(self):
        c=Clock(); w=IdleWindow(300,c); c.now+=301
        e={'healthy':True,'timestamp':c.now,'last_business':1000,'pending':0}
        self.assertTrue(w.ready(e))
        e['pending']=1; self.assertFalse(w.ready(e))
        e['pending']=0; e['last_business']=c.now; self.assertFalse(w.ready(e))
        e['last_business']=0; e['healthy']=False; self.assertFalse(w.ready(e))
    def test_invalid_or_future_evidence_fails_closed(self):
        c=Clock(); w=IdleWindow(300,c); c.now+=301
        for e in [{},{'healthy':True,'timestamp':c.now+10,'last_business':0,'pending':0},{'healthy':True,'timestamp':c.now,'last_business':float('nan'),'pending':0}]:
            self.assertFalse(w.ready(e))
    def test_budget_includes_recovery_margin(self):
        self.assertGreater(validate_timing(6,7,2,40),0)
        for values in [(18,18,4,40),(6,7,2,0),(-1,7,2,40)]:
            with self.assertRaises(ValueError):validate_timing(*values)


class CutoverTests(unittest.TestCase):
    def test_order_requires_admission_drain_and_writer_exclusion(self):
        b=Backend(); Cutover(b).run()
        self.assertEqual(b.events,['preflight','watchdog','gate','drain','checkpoint','stop_old','no_writers','start_candidate','ready','preserve_state','commit','ungate','cancel_watchdog'])
        self.assertTrue(b.committed)
    def test_preflight_failure_cannot_touch_production(self):
        b=Backend('preflight')
        with self.assertRaises(RuntimeError):Cutover(b).run()
        self.assertEqual(b.events,['preflight'])
    def test_failure_before_stop_retains_old_and_removes_gate(self):
        for phase in ['gate','drain','checkpoint']:
            b=Backend(phase)
            with self.assertRaises(RuntimeError):Cutover(b).run()
            self.assertTrue(b.old_running)
            self.assertNotIn('stop_old',b.events)
            self.assertIn('ungate',b.events)
    def test_failure_after_stop_restores_original_before_ungating(self):
        for phase in ['no_writers','start_candidate','ready','preserve_state','commit']:
            b=Backend(phase)
            with self.assertRaises(RuntimeError):Cutover(b).run()
            self.assertTrue(b.old_running)
            self.assertFalse(b.candidate_running)
            self.assertLess(b.events.index('recover'),b.events.index('ungate'))


if __name__=='__main__':unittest.main()
