"""Pure, deterministic safety decisions for one reviewed idle deployment."""
import math
import time


class DeploymentBlocked(RuntimeError):
    """A sanitized, durable explanation of a deployment safety gate."""
    def __init__(self, reason, **details):
        super().__init__(reason)
        self.reason = reason
        self.details = details


class IdleWindow:
    def __init__(self, seconds=300, now=time.monotonic, immediate=False):
        if seconds < 300: raise ValueError('at least five minutes of observed idle is required')
        self.seconds=0 if immediate else seconds; self.now=now; self.started=now()

    def ready(self, evidence):
        try:
            now=self.now(); stamp=float(evidence['timestamp']); business=float(evidence['last_business'])
            if not all(map(math.isfinite,[now,stamp,business])): return False
            return (evidence['healthy'] is True and evidence['pending']==0 and
                    0 <= now-stamp <= 3 and business <= stamp and
                    now-max(self.started,business) >= self.seconds)
        except (KeyError,ValueError,TypeError): return False


def validate_timing(candidate,rollback,stop,budget=40,recovery_total=None):
    values=[candidate,rollback,stop,budget]
    if not all(math.isfinite(v) and v>0 for v in values) or budget>40:
        raise ValueError('invalid measured deployment timings or outage budget')
    reserve=max(8,rollback*1.5+stop+3,(recovery_total or 0)*1.5+3)
    candidate_budget=budget-reserve
    if candidate*1.5+stop+4 >= candidate_budget:
        raise ValueError('rehearsal leaves insufficient rollback margin below 40 seconds')
    return candidate_budget


class Cutover:
    """No stop before admission is closed, drained, quiet, and checkpointed."""
    def __init__(self,backend): self.backend=backend
    def run(self):
        b=self.backend
        b.preflight()
        b.watchdog()
        recovered=False
        try:
            b.gate(); b.drained_and_quiet(); b.checkpoint()
            b.stop_old(); b.assert_no_writers(); b.start_candidate()
            b.candidate_ready(); b.preserve_state(); b.commit()
            recovered=True
        except BaseException:
            b.recover(); recovered=True
            raise
        finally:
            b.ungate()
            if recovered:b.cancel_watchdog()

# Once a writer is serving again, cleanup retries must never restart it.
TERMINAL_OUTCOMES=('committed','rolled_back','original_retained')
