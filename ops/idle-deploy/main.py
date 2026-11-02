#!/usr/bin/env python3
"""Prepare, observe, deploy once while idle, and independently verify a reviewed image."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pwd
import select
import signal
import subprocess
import sys
import time
import traceback

from common import ROOT,atomic,digest,inspect,probe,run
from core import IdleWindow,Cutover,TERMINAL_OUTCOMES,DeploymentBlocked
from backend import Backend


class Capture:
    def __init__(self,pid):
        self.process=subprocess.Popen(['nsenter','-t',str(pid),'-n','/usr/bin/python3',Path(__file__).parent/'traffic.py','--port','8317'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
        os.set_blocking(self.process.stdout.fileno(),False);self.buffer=b'';self.latest={}
    def read(self,timeout=0):
        if select.select([self.process.stdout],[],[],timeout)[0]:
            chunk=os.read(self.process.stdout.fileno(),65536)
            if not chunk:self.latest={};return self.latest
            self.buffer+=chunk
            if len(self.buffer)>131072:self.latest={};self.buffer=b'';return self.latest
            while b'\n' in self.buffer:
                line,self.buffer=self.buffer.split(b'\n',1)
                try:self.latest=json.loads(line)
                except (ValueError,TypeError):self.latest={}
        if self.process.poll() is not None:self.latest={}
        return self.latest
    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:self.process.wait(2)
            except subprocess.TimeoutExpired:self.process.kill();self.process.wait()


def release_path(value):
    p=Path(value).resolve()
    if not p.is_relative_to((ROOT/'.tmp/idle-deploy').resolve()) or p.name!='release.json':raise RuntimeError('release must be in the private project deployment directory')
    return p


def report_dir(m):
    base=ROOT/'output/verification/idle-deploy';st=ROOT.stat()
    for parent in (ROOT/'output',ROOT/'output/verification',base):
        parent.mkdir(exist_ok=True,mode=0o755);os.chown(parent,st.st_uid,st.st_gid);os.chmod(parent,0o755)
    p=base/m['token'];p.mkdir(exist_ok=True,mode=0o700);os.chown(p,st.st_uid,st.st_gid)
    return p


def publish(m,name,value):
    p=report_dir(m)/name;atomic(p,value);st=ROOT.stat();os.chown(p,st.st_uid,st.st_gid)


def launch(path,immediate=False):
    m=json.loads(path.read_text())
    if inspect(m['candidate_id'])['State']['Running']:raise RuntimeError('prepared candidate is already running')
    name='cliproxy-idle-deploy-'+m['token']
    command=['systemd-run','--quiet','--collect','--unit',name,'--property=Nice=10','--property=KillMode=control-group','/usr/bin/python3',Path(__file__).resolve(),'watch','--release',path]
    if immediate:command.append('--now')
    run(command,timeout=5)
    return {'unit':name,'release':str(path),'idle_seconds':0 if immediate else 300,'outage_target_seconds':40,'retire_idle_clients':m.get('retire_idle_clients',False)}


def codex_verify(path,m):
    from verify import after
    directory=report_dir(m)
    try:
        live=after(path.parent,m['candidate_commit']);live['passed']=True
    except Exception as error:
        live={'passed':False,'error_class':type(error).__name__,'verification':'inconclusive; no active sessions will be interrupted'}
    publish(m,'live-verification.json',live)
    schema={'type':'object','properties':{'passed':{'type':'boolean'},'checks':{'type':'array','items':{'type':'string'}},'failures':{'type':'array','items':{'type':'string'}}},'required':['passed','checks','failures'],'additionalProperties':False}
    publish(m,'verifier-schema.json',schema)
    prompt=(f'Verify the CLIProxyAPI deployment at /DATA/AppData/CLIProxyAPI. Expected source commit {m["candidate_commit"]}. '
            f'Read {directory}/deployment.json and {directory}/live-verification.json. '
            f'Independently run /usr/bin/python3 {Path(__file__).resolve()} probe --commit {m["candidate_commit"]} --all-interfaces. '
            'Verify actual running image/commit, all three network interfaces, mandatory automatic resets, history health, and recorded genuine encrypted continuation with unchanged owner, complete SSE and two WS turns. '
            'This is strictly a verification session: NEVER stop/restart/recreate containers, write routing/config/credentials/account-policy state, redeem credits, change firewall rules or repair production. '
            'Do not print environment files, credentials, prompts, encrypted data, tokens or raw session/account IDs. Use the supplied helper, which keeps credentials private. '
            'Return passed=true only if all recorded checks and independent probe pass; otherwise list exact limitations. Write only in this report directory. Do not claim future behavior is flawless.')
    user=pwd.getpwuid(ROOT.stat().st_uid).pw_name
    command=['runuser','-u',user,'--','env','TMPDIR='+str(ROOT/'.tmp'),'/home/levi/.local/bin/cod','exec','--ephemeral','--sandbox','workspace-write','-c','sandbox_workspace_write.network_access=true','-c','approval_policy="never"','-C',str(directory),'--skip-git-repo-check','--output-schema',str(directory/'verifier-schema.json'),'-o',str(directory/'codex-verdict.json'),'-']
    # Raw CLI events can include response details; retain privately, never in journalctl.
    log=directory/'codex-session.log'
    with log.open('wb') as f:
        os.chmod(log,0o600);os.chown(log,ROOT.stat().st_uid,ROOT.stat().st_gid)
        try:r=subprocess.run(command,input=prompt.encode(),stdout=f,stderr=subprocess.STDOUT,timeout=1200)
        except subprocess.TimeoutExpired:publish(m,'verification-status.json',{'passed':False,'status':'codex_timeout'});return
    verdict=directory/'codex-verdict.json'
    try:passed=r.returncode==0 and json.loads(verdict.read_text())['passed'] is True and live['passed']
    except (OSError,ValueError,KeyError):passed=False
    publish(m,'verification-status.json',{'passed':passed,'codex_exit':r.returncode,'status':'verified' if passed else 'verification_failed_or_inconclusive','production_restarted_for_verification':False})


def deployment_backend(path,evidence,idle):
    m=json.loads(path.read_text())
    if m.get('retire_idle_clients',False):
        from maintenance import MaintenanceBackend
        return MaintenanceBackend(path,evidence,idle)
    return Backend(path,evidence,idle)


def failure_details(error):
    details={'reason':'unexpected_controller_failure','error_class':type(error).__name__,'at':time.time(),'requires_restage':True}
    if isinstance(error,DeploymentBlocked):
        details.update(reason=error.reason,requires_restage=False)
        details.update(error.details)
    frames=traceback.extract_tb(error.__traceback__)
    if frames:
        frame=frames[-1]
        details['source']={'file':Path(frame.filename).name,'line':frame.lineno,'function':frame.name}
    return details


def watch_status(idle,evidence,last_block=None,retire_idle_clients=False):
    status={'phase':'waiting_for_drain' if idle.seconds==0 else 'waiting_for_idle',
            'immediate':idle.seconds==0,
            'idle_seconds':round(max(0,idle.now()-max(idle.started,evidence.get('last_business',idle.now()))),1),
            'idle_required_seconds':idle.seconds,
            'capture_healthy':evidence.get('healthy') is True,
            'pending':evidence.get('pending',0),
            'capture_drops':evidence.get('capture_drops',0),
            'retire_idle_clients':retire_idle_clients,
            'updated_at':time.time()}
    if last_block:
        status['last_block']=last_block
        if idle.ready(evidence):status['phase']='blocked'
    return status


def watch(path,immediate=False):
    if os.geteuid()!=0:raise RuntimeError('root is required for namespace observation and admission exclusion')
    m=json.loads(path.read_text());lock=ROOT/'.tmp/idle-deploy/watcher.lock'
    with lock.open('a+') as owner:
        fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
        worker=(path.parent/'worker.lock').open('a+');fcntl.flock(worker,fcntl.LOCK_EX|fcntl.LOCK_NB)
        capture=Capture(m['old_pid']);idle=IdleWindow(immediate=immediate);deadline=time.monotonic()+7*86400;last_block=None
        def stop(*_):raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM,stop)
        try:
            while time.monotonic()<deadline:
                evidence=capture.read(1)
                status=watch_status(idle,evidence,last_block,m.get('retire_idle_clients',False))
                publish(m,'status.json',status)
                if not idle.ready(evidence):continue
                backend=deployment_backend(path,capture.read,idle)
                try:Cutover(backend).run()
                except Exception as error:
                    backend.release_redemption()
                    transaction=json.loads(backend.journal.read_text()) if backend.journal.exists() else {}
                    if transaction.get('phase')=='committed':
                        backend.ungate();capture.close();publish(m,'deployment.json',transaction);codex_verify(path,m);return
                    last_block=failure_details(error)
                    publish(m,'failure.json',dict(last_block,transaction_phase=transaction.get('phase'),attempt=transaction.get('attempt')))
                    print(json.dumps({'deployment_blocked':last_block}),flush=True)
                    if transaction:backend.phase(transaction['phase'],last_failure=last_block)
                    status=watch_status(idle,evidence,last_block,m.get('retire_idle_clients',False));status['phase']='blocked'
                    publish(m,'status.json',status)
                    # If a transaction stopped the original, recovery ended the release.
                    if transaction.get('phase') in ('rolled_back','recovering') or (transaction.get('phase')=='original_retained' and not transaction.get('ungated_at')):
                        publish(m,'deployment.json',transaction);return
                    if last_block.get('requires_restage'):
                        status['phase']='restaging_required';publish(m,'status.json',status);return
                    if inspect(m['old_id'])['State']['Pid']!=m['old_pid']:raise RuntimeError('original process changed; restaging required')
                    time.sleep(2);continue
                capture.close();transaction=json.loads(backend.journal.read_text());publish(m,'deployment.json',transaction)
                codex_verify(path,m);return
            publish(m,'status.json',{'phase':'expired','reason':'seven-day wait limit reached'})
        finally:capture.close()


def rescue(path):
    m=json.loads(path.read_text());backend=Backend(path)
    # Serialize the irreversible commit decision. A committed release is never
    # stopped by a watchdog, even if its timer raced admission reopening.
    with (path.parent/'commit.lock').open('a+') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX)
        transaction=json.loads(backend.journal.read_text()) if backend.journal.exists() else {}
        if transaction.get('phase') in TERMINAL_OUTCOMES:backend.ungate();return
        rescue_started=time.monotonic()
        (path.parent/'rescue-requested').touch(mode=0o600)
        unit=m.get('watcher_unit','cliproxy-idle-deploy-'+m['token'])
        run(['systemctl','kill','--kill-whom=all','--signal=SIGKILL',unit],timeout=2,check=False)
        deadline=time.monotonic()+2
        with (path.parent/'worker.lock').open('a+') as worker:
            while True:
                try:fcntl.flock(worker,fcntl.LOCK_EX|fcntl.LOCK_NB);break
                except BlockingIOError:
                    if time.monotonic()>deadline:raise RuntimeError('worker did not terminate; refusing concurrent rollback')
                    time.sleep(.05)
            # Durable state is reread only after the worker has stopped.
            transaction=json.loads(backend.journal.read_text()) if backend.journal.exists() else {}
            if transaction.get('phase') not in TERMINAL_OUTCOMES:backend.phase(transaction.get('phase','rescue'),rescue_started=rescue_started)
            try:
                if transaction.get('phase') not in TERMINAL_OUTCOMES:backend.recover()
            finally:backend.ungate()
    publish(m,'deployment.json',json.loads(backend.journal.read_text()))


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('command',choices=['prepare','launch','watch','rescue','probe','status','fixture-worker']);p.add_argument('--release');p.add_argument('--image');p.add_argument('--commit');p.add_argument('--all-interfaces',action='store_true');p.add_argument('--now',action='store_true',help='explicitly skip the five-minute delay, retaining drain and recovery gates');p.add_argument('--retire-idle-clients',action='store_true',help='pin idle socket retirement after five quiet minutes and zero active requests');a=p.parse_args()
    if a.retire_idle_clients and a.command!='prepare':p.error('--retire-idle-clients must be pinned during prepare')
    if a.now and a.command not in ('launch','watch'):p.error('--now is only valid with launch or watch')
    if a.command=='probe':print(json.dumps(probe(a.commit,a.all_interfaces)));return
    if a.command=='prepare':
        from prepare import prepare
        from verify import before
        path=prepare(a.image,a.commit,retire_idle_clients=a.retire_idle_clients);m=json.loads(path.read_text());proof=before(path.parent);publish(m,'continuity-preflight.json',proof)
        print(json.dumps({'release':str(path),'timing':m['timing'],'candidate_commit':m['candidate_commit']}));return
    path=release_path(a.release)
    if a.command=='fixture-worker':
        from prepare import fixture_worker
        fixture_worker(path);return
    if a.command=='launch':print(json.dumps(launch(path,a.now)))
    elif a.command=='watch':watch(path,a.now)
    elif a.command=='rescue':rescue(path)
    elif a.command=='status':
        m=json.loads(path.read_text());s=report_dir(m)/'status.json';print(s.read_text() if s.exists() else '{"phase":"prepared"}')


if __name__=='__main__':
    try:main()
    except KeyboardInterrupt:sys.exit(130)
    except Exception as error:
        print(json.dumps({'failed':True,'error_class':type(error).__name__,'reason':str(error)[:200] if type(error) is RuntimeError else 'see private diagnostic log'}),file=sys.stderr)
        import traceback
        target=ROOT/'.tmp/idle-deploy/private-error.log';target.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
        with target.open('a') as log:os.chmod(target,0o600);traceback.print_exc(file=log)
        sys.exit(1)
