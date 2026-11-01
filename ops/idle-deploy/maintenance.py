#!/usr/bin/env python3
"""Explicit maintenance: allow idle sockets to disconnect after active leases reach zero."""
import fcntl
import json
from pathlib import Path
import signal
import sys
import time

from backend import Backend
from common import ROOT,run,inspect
from core import Cutover,IdleWindow
from main import Capture,codex_verify,publish,release_path


class MaintenanceBackend(Backend):
    def preflight(self):
        # Only an explicit operator maintenance command permits idle disconnects.
        # Base preflight still requires authoritative active_requests == 0.
        if inspect(self.m['old_id'])['HostConfig']['NetworkMode']=='host':raise RuntimeError('maintenance requires a dedicated container network namespace')
        super().preflight(require_no_connections=False)

    def drained_and_quiet(self):
        control=self.drain_connection
        if control is None:raise RuntimeError('pre-established management socket is unavailable')
        if control.activity()!=0:raise RuntimeError('active requests appeared; original router retained')
        if self.idle is None or not self.idle.ready(self.evidence()):raise RuntimeError('quiet observation changed before idle retirement')
        self.gates.freeze_idle_clients(control.exclude)
        if control.activity()!=0:raise RuntimeError('active requests appeared; original router retained')
        server,client=control.exclude
        ports=' or '.join(f'sport = :{port}' for port in self.m['ports'])
        expression=f'( ( {ports} ) and not ( src {server[0]} and sport = :{server[1]} and dst {client[0]} and dport = :{client[1]} ) )'
        # Five quiet minutes were proved before freezing every client path.
        # Our intentional disconnects may reset last_business; require fresh,
        # healthy, complete capture during the gated drain without restarting
        # the quiet interval. The final authoritative lease check still runs.
        self.idle=IdleWindow(now=self.idle.now,immediate=True)
        run(['nsenter','-t',str(self.m['old_pid']),'-n','ss','-K',expression],timeout=2)
        # Preserve the standard authoritative lease and complete TCP drain proof.
        # Established sockets cannot submit another turn while INPUT is frozen.
        super().drained_and_quiet()


def worker(path):
    m=json.loads(path.read_text())
    with (ROOT/'.tmp/idle-deploy/watcher.lock').open('a+') as owner,(path.parent/'worker.lock').open('a+') as lock:
        fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        capture=Capture(m['old_pid']);idle=IdleWindow(immediate=True)
        def stop(*_):raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM,stop)
        try:
            deadline=time.monotonic()+3
            while not idle.ready(capture.read(.2)):
                if time.monotonic()>deadline:raise RuntimeError('fresh traffic observation is unavailable')
            backend=MaintenanceBackend(path,capture.read,idle)
            try:Cutover(backend).run()
            finally:
                if backend.journal.exists():publish(m,'deployment.json',json.loads(backend.journal.read_text()))
        finally:capture.close()
        codex_verify(path,m)


def main():
    command,value=sys.argv[1:]
    path=release_path(value);m=json.loads(path.read_text())
    if command=='launch':
        unit='cliproxy-idle-deploy-'+m['token']
        run(['systemd-run','--quiet','--collect','--unit',unit,'--property=KillMode=control-group','/usr/bin/python3',Path(__file__).resolve(),'worker',path],timeout=5)
        print(json.dumps({'unit':unit,'maintenance':True,'outage_target_seconds':40}))
    elif command=='worker':worker(path)
    else:raise RuntimeError('expected launch or worker')


if __name__=='__main__':main()
