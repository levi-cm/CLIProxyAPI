#!/usr/bin/env python3
"""Operator-authorized hard cutover that interrupts in-flight client traffic.

Only the idle-window and zero-activity requirements are removed. Release,
container and configuration identity, rehearsal, measured timing, the
independent watchdog, admission gates, writer fencing, readiness, state
preservation, rollback and verification are the unchanged Backend/Cutover path.
"""
import fcntl
import json
import os
from pathlib import Path
import signal
import sys
import time

from backend import Backend,DrainConnection
from common import ROOT,connections,digest,inspect,probe,run,writers
from core import Cutover,DeploymentBlocked,validate_timing
from main import codex_verify,failure_details,publish,release_path

REDEMPTION_WAIT_SECONDS=120
KILL_WINDOW_SECONDS=3
PRE_STOP_SECONDS=6


def kill_expression(ports,exclude):
    """Every client socket on the router ports except the exact control tuple."""
    server,client=exclude
    ports=' or '.join(f'sport = :{port}' for port in ports)
    return f'( ( {ports} ) and not ( src {server[0]} and sport = :{server[1]} and dst {client[0]} and dport = :{client[1]} ) )'


class InterruptBackend(Backend):
    def acquire_redemption(self):
        # Never interrupt automatic reset redemption: wait (bounded) for an
        # in-flight redemption to finish, then fence it exactly as Backend does.
        lock=(self.state/'redemption.lock').open('rb');deadline=time.monotonic()+REDEMPTION_WAIT_SECONDS
        try:
            while True:
                try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB);break
                except BlockingIOError:
                    if time.monotonic()>=deadline:raise DeploymentBlocked('automatic_redemption_in_flight',waited_seconds=REDEMPTION_WAIT_SECONDS)
                    time.sleep(.5)
        except BaseException:
            lock.close();raise
        self.redemption=lock

    def preflight(self):
        # Fence redemption first, so every check below is fresh after any wait.
        self.acquire_redemption()
        try:
            m=self.m;old=inspect(m['old_id']);candidate=inspect(m['candidate_id'])
            if old['HostConfig']['NetworkMode']=='host':raise RuntimeError('interrupt requires a dedicated container network namespace')
            # Identical to Backend.preflight, minus activity, connection and idle evidence.
            if not old['State']['Running'] or old['Name']!='/'+self.service or old['Image']!=m['old_image_id'] or old['State']['Pid']!=m['old_pid']:raise RuntimeError('serving container changed; restaging required')
            if candidate['State']['Running'] or candidate['Image']!=m['candidate_image_id']:raise RuntimeError('candidate identity/state changed')
            if any(digest(self.repo/n)!=m[n+'_sha256'] for n in ('config.yaml','.env')):raise DeploymentBlocked('configuration_changed',requires_restage=True)
            if digest(self.dir/'rehearsal.json')!=m['rehearsal_sha256']:raise DeploymentBlocked('rehearsal_changed',requires_restage=True)
            age=time.time()-m['prepared_at']
            if age>86400:raise DeploymentBlocked('rehearsal_expired',requires_restage=True,age_seconds=int(age))
            if any(digest(Path(__file__).parent/n)!=sha for n,sha in m['controller_files'].items()):raise DeploymentBlocked('controller_changed',requires_restage=True)
            if 'interrupt.py' not in m['controller_files']:raise DeploymentBlocked('interrupt_controller_unpinned',requires_restage=True)
            if not json.loads((self.dir/'rehearsal.json').read_text()).get('complete_cycles'):raise RuntimeError('complete cutover/watchdog rehearsal missing')
            self.candidate_budget=validate_timing(**m['timing']);t=m['timing']
            # validate_timing reserves 4s for the default 3.5s pre-stop window;
            # the longer interrupt window must fit the same measured budget.
            if t['candidate']*1.5+t['stop']+PRE_STOP_SECONDS+.5>=self.candidate_budget:raise DeploymentBlocked('interrupt_budget_insufficient',requires_restage=True)
            probe(m['old_commit']);writers(self.state,True)
            self.reset_attempt();self.snapshot();self.phase('preflight',mode='interrupt')
        except BaseException:
            self.release_redemption();raise

    def gate(self):
        # Backend.gate without its zero-activity requirement; accounting on the
        # control socket must still answer before admission closes.
        self.started=time.monotonic();self.phase('gating',started=self.started)
        self.drain_connection=DrainConnection(self.m['old_pid'])
        self.drain_connection.activity()
        self.gates.enable()

    def drained_and_quiet(self):
        control=self.drain_connection
        if control is None:raise RuntimeError('pre-established management socket is unavailable')
        pid,ports=self.m['old_pid'],self.m['ports']
        try:
            self.gates.freeze_idle_clients(control.exclude)
            requests=control.activity();clients=connections(pid,ports,exclude=control.exclude)
            # The phase stays 'gating': any failure here retains the running original.
            self.phase('gating',interrupted_requests=requests,interrupted_connections=clients)
            command=['nsenter','-t',str(pid),'-n','ss','-K',kill_expression(ports,control.exclude)]
            deadline=min(time.monotonic()+KILL_WINDOW_SECONDS,self.started+PRE_STOP_SECONDS-.5)
            while True:
                run(command,timeout=2)
                remaining=connections(pid,ports,exclude=control.exclude)
                if remaining==0:break
                if time.monotonic()>=deadline:raise DeploymentBlocked('client_connections_after_interrupt',client_connections=remaining)
                time.sleep(.1)
        finally:
            control.close();self.drain_connection=None
        # As in Backend: close the read-only control socket before the writer may stop.
        while time.monotonic()<self.started+PRE_STOP_SECONDS:
            if connections(pid,ports)==0:return
            time.sleep(.05)
        raise RuntimeError('management socket did not drain; original router retained')

    def checkpoint(self):
        # Backend.checkpoint with the budget-checked interrupt pre-stop window.
        if time.monotonic()>self.started+PRE_STOP_SECONDS:raise RuntimeError('final drain exceeded its pre-stop budget')
        if any(digest(self.repo/n)!=self.m[n+'_sha256'] for n in ('config.yaml','.env')):raise RuntimeError('configuration changed during final drain')
        self.phase('checkpointed')


def launch(path):
    m=json.loads(path.read_text());unit='cliproxy-idle-deploy-'+m['token']
    run(['systemd-run','--quiet','--collect','--unit',unit,'--property=KillMode=control-group','/usr/bin/python3',Path(__file__).resolve(),'worker',path],timeout=5)
    return {'unit':unit,'release':str(path),'mode':'interrupt','outage_target_seconds':40}


def worker(path):
    m=json.loads(path.read_text())
    with (ROOT/'.tmp/idle-deploy/watcher.lock').open('a+') as owner,(path.parent/'worker.lock').open('a+') as lock:
        fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        def stop(*_):raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM,stop)
        backend=InterruptBackend(path)
        try:Cutover(backend).run()
        except Exception as error:
            backend.release_redemption()
            transaction=json.loads(backend.journal.read_text()) if backend.journal.exists() else {}
            if transaction.get('phase')!='committed':
                publish(m,'failure.json',dict(failure_details(error),mode='interrupt',transaction_phase=transaction.get('phase')))
                if transaction:publish(m,'deployment.json',transaction)
                raise
            # As in main.watch: a committed release only needs admission cleanup.
            backend.ungate()
        publish(m,'deployment.json',json.loads(backend.journal.read_text()))
        codex_verify(path,m)


def main():
    if os.geteuid()!=0:raise RuntimeError('root is required for namespace control and admission exclusion')
    command,value=sys.argv[1:]
    path=release_path(value)
    if command=='launch':print(json.dumps(launch(path)))
    elif command=='worker':worker(path)
    else:raise RuntimeError('expected launch or worker')


if __name__=='__main__':main()
