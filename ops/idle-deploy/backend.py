"""Measured, journaled cutover of a pre-created stopped container."""
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import time
import uuid

from common import ROOT,LOCKS,activity,api,atomic,connections,digest,inspect,key,lock_available,owners,preserve_owners,prefix_digest,probe,run,writers
from core import validate_timing,TERMINAL_OUTCOMES,DeploymentBlocked
from gates import Gates


class DockerConnection(http.client.HTTPConnection):
    def __init__(self):super().__init__('localhost',timeout=3)
    def connect(self):
        self.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);self.sock.settimeout(3);self.sock.connect('/var/run/docker.sock')


class DrainConnection:
    """One authenticated socket opened before admission closes; never reconnect."""
    def __init__(self,pid):
        self.connection=http.client.HTTPConnection('127.0.0.1',8317,timeout=1)
        self.connection.auto_open=False
        self.headers={'Authorization':'Bearer '+key('CLI_PROXY_MANAGEMENT_KEY')}
        try:
            with Path('/proc/self/ns/net').open('rb') as original,Path(f'/proc/{pid}/ns/net').open('rb') as target:
                try:
                    os.setns(target.fileno(),os.CLONE_NEWNET)
                    self.connection.connect()
                finally:
                    try:os.setns(original.fileno(),os.CLONE_NEWNET)
                    except BaseException:
                        # Only the independent host-namespace watchdog may recover
                        # if this worker can no longer prove its network namespace.
                        os._exit(1)
            self.socket=self.connection.sock
            self.exclude=(self.socket.getpeername()[:2],self.socket.getsockname()[:2])
        except BaseException:
            self.close();raise

    def activity(self):
        if self.connection.sock is not self.socket or self.socket.fileno()<0:raise RuntimeError('pre-established management socket is unavailable')
        self.connection.request('GET','/v8/management/account-policy/dashboard?range=1h',headers=self.headers)
        response=self.connection.getresponse();raw=response.read()
        if response.status!=200 or response.will_close or self.connection.sock is not self.socket:raise RuntimeError('pre-established management socket is not healthy')
        activity=json.loads(raw)['activity']
        if activity['available'] is not True:raise RuntimeError('request accounting is unavailable')
        counts=[account['active_requests'] for account in activity['accounts']]
        if any(type(value) is not int or value<0 for value in counts):raise RuntimeError('request accounting is invalid')
        return sum(counts)

    def close(self):self.connection.close()


def create_stopped(old,name,image):
    allowed=('Env','Cmd','Entrypoint','ExposedPorts','WorkingDir','Labels','User','Healthcheck','StopSignal','StopTimeout','Volumes','Tty','OpenStdin')
    config={k:old['Config'][k] for k in allowed if k in old['Config']}
    config['Image']=image;config['Labels']=dict(config.get('Labels',{}));config['Labels']['com.docker.compose.image']=image
    config['HostConfig']=old['HostConfig']
    network=old['HostConfig']['NetworkMode']
    if network not in ('default','bridge','host','none'):
        aliases=old.get('NetworkSettings',{}).get('Networks',{}).get(network,{}).get('Aliases') or ['cli-proxy-api']
        config['NetworkingConfig']={'EndpointsConfig':{network:{'Aliases':[a for a in aliases if a not in (old['Id'],old['Id'][:12])]}}}
    c=DockerConnection();c.request('POST','/containers/create?name='+name,json.dumps(config),{'Content-Type':'application/json'});r=c.getresponse();body=r.read();c.close()
    if r.status!=201:raise RuntimeError('stopped candidate preparation failed')
    return json.loads(body)['Id']


class Backend:
    def __init__(self,path,evidence=None,idle=None,repo=ROOT):
        self.path=Path(path);self.dir=self.path.parent;self.m=json.loads(self.path.read_text());self.evidence=evidence;self.idle=idle
        self.repo=Path(repo);self.token=self.m['token'];self.state=self.repo/'account-policy-state';self.started=None;self.redemption=None;self.drain_connection=None
        if self.m.get('fixture_repo'):
            fixture=Path(self.m['fixture_repo']).resolve()
            if not fixture.is_relative_to((ROOT/'.tmp/idle-deploy').resolve()) or fixture.name!='fixture':raise RuntimeError('invalid fixture namespace')
            for identity in ('old_id','candidate_id'):
                info=inspect(self.m[identity])
                if info['HostConfig']['NetworkMode']!='none' or not info['Name'].startswith('/cliproxy-idle-fixture-'):raise RuntimeError('fixture tried to control production')
                for mount in info['Mounts']:
                    if mount['Type']=='bind' and not Path(mount['Source']).resolve().is_relative_to(fixture):raise RuntimeError('fixture mounts external state')
            self.repo=fixture;self.state=fixture/'account-policy-state'
        self.service=self.m.get('service_name','cli-proxy-api')
        self.gates=Gates(self.token,self.m['ports'],self.m['old_pid'],self.m.get('host_ports'));self.timer='cliproxy-idle-watchdog-'+self.token
        self.journal=self.dir/'transaction.json'
        if self.journal.exists():self.started=json.loads(self.journal.read_text()).get('started')

    def phase(self,name,**extra):
        with (self.dir/'journal.lock').open('a+') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            data=json.loads(self.journal.read_text()) if self.journal.exists() else {}
            data.update(phase=name,updated_at=time.time(),**extra);atomic(self.journal,data)

    def preflight(self,require_no_connections=True):
        m=self.m;old=inspect(m['old_id']);candidate=inspect(m['candidate_id'])
        if not old['State']['Running'] or old['Name']!='/'+self.service or old['Image']!=m['old_image_id'] or old['State']['Pid']!=m['old_pid']:raise RuntimeError('serving container changed; restaging required')
        if candidate['State']['Running'] or candidate['Image']!=m['candidate_image_id']:raise RuntimeError('candidate identity/state changed')
        if any(digest(self.repo/n)!=m[n+'_sha256'] for n in ('config.yaml','.env')):raise DeploymentBlocked('configuration_changed',requires_restage=True)
        if digest(self.dir/'rehearsal.json')!=m['rehearsal_sha256']:raise DeploymentBlocked('rehearsal_changed',requires_restage=True)
        age=time.time()-m['prepared_at']
        if age>86400:raise DeploymentBlocked('rehearsal_expired',requires_restage=True,age_seconds=int(age))
        if any(digest(Path(__file__).parent/n)!=sha for n,sha in m['controller_files'].items()):raise DeploymentBlocked('controller_changed',requires_restage=True)
        if not json.loads((self.dir/'rehearsal.json').read_text()).get('complete_cycles'):raise RuntimeError('complete cutover/watchdog rehearsal missing')
        self.candidate_budget=validate_timing(**m['timing'])
        active=activity()
        if active!=0:raise DeploymentBlocked('active_requests',active_requests=active)
        clients=connections(m['old_pid'],m['ports']) if require_no_connections else 0
        if clients:raise DeploymentBlocked('client_connections',client_connections=clients)
        if self.idle is None or not self.idle.ready(self.evidence()):raise DeploymentBlocked('traffic_observation')
        probe(m['old_commit']);writers(self.state,True)
        self.redemption=(self.state/'redemption.lock').open('rb')
        try:fcntl.flock(self.redemption,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:
            self.redemption.close();self.redemption=None;raise DeploymentBlocked('automatic_redemption_in_flight')
        self.reset_attempt()
        self.snapshot()
        self.phase('preflight')

    def reset_attempt(self):
        # A previous watchdog must be inactive before reusing this release's
        # token. Keep its evidence separately and start timing from a clean journal.
        status=run(['systemctl','is-active',self.timer+'.timer',self.timer+'.service'],timeout=2,check=False).decode().splitlines()
        if len(status)!=2 or any(v not in ('inactive','failed','unknown') for v in status):raise RuntimeError('previous recovery worker is active or its state is unverified')
        with (self.dir/'journal.lock').open('a+') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            old=json.loads(self.journal.read_text()) if self.journal.exists() else {}
            if old:
                if old.get('phase') not in TERMINAL_OUTCOMES or not old.get('ungated_at'):raise RuntimeError('previous transaction has not finished cleanup')
                atomic(self.dir/'attempts'/(uuid.uuid4().hex+'.json'),old)
                (self.dir/'rescue-requested').unlink(missing_ok=True)
            elif (self.dir/'rescue-requested').exists():raise RuntimeError('orphaned recovery marker requires restaging')
            atomic(self.journal,{'phase':'preflight','attempt':old.get('attempt',0)+1,'updated_at':time.time()})
        self.started=None

    def watchdog(self):
        # A different systemd cgroup survives termination of the primary watcher.
        script=Path(__file__).parent/'main.py'
        try:
            run(['systemd-run','--quiet','--collect','--unit',self.timer,'--on-active='+str(self.candidate_budget)+'s','--timer-property=AccuracySec=50ms','--timer-property=RandomizedDelaySec=0','--property=Restart=on-failure','--property=RestartSec=1','/usr/bin/python3',script,'rescue','--release',self.path],timeout=4)
        except BaseException:
            self.release_redemption();raise

    def gate(self):
        self.started=time.monotonic();self.phase('gating',started=self.started)
        self.drain_connection=DrainConnection(self.m['old_pid'])
        if self.drain_connection.activity()!=0:raise DeploymentBlocked('active_requests_before_gate')
        self.gates.enable()

    def drained_and_quiet(self):
        deadline=min(self.started+3,self.started+self.candidate_budget)
        control=self.drain_connection
        if control is None:raise RuntimeError('pre-established management socket is unavailable')
        try:
            while time.monotonic()<deadline:
                if connections(self.m['old_pid'],self.m['ports'],exclude=control.exclude)==0:
                    # Include the observer's next fresh heartbeat after admission closed.
                    time.sleep(1.1)
                    if connections(self.m['old_pid'],self.m['ports'],exclude=control.exclude)!=0 or not self.idle.ready(self.evidence()):raise DeploymentBlocked('traffic_during_drain')
                    if control.activity()!=0:raise DeploymentBlocked('active_requests_during_drain')
                    break
                time.sleep(.1)
            else:raise DeploymentBlocked('client_connections_during_drain')
        finally:
            control.close();self.drain_connection=None
        # No clients remain and admission is closed, so no new lease can appear.
        # Close our read-only socket too before the original writer may stop.
        while time.monotonic()<deadline:
            if connections(self.m['old_pid'],self.m['ports'])==0:
                if not self.idle.ready(self.evidence()):raise DeploymentBlocked('traffic_during_drain')
                return
            time.sleep(.05)
        raise RuntimeError('management socket did not drain; original router retained')

    def snapshot(self):
        self.before=owners(self.state);atomic(self.dir/'owners-before.json',self.before)
        atomic(self.dir/'auth-files-before.json',[p.name for p in (self.repo/'auths').iterdir() if p.is_file()])
        # No historical data is copied, removed or rewritten during the outage.
        history=[]
        for p in (self.state/'history').glob('*.jsonl'):
            size=p.stat().st_size
            st=p.stat();sha=prefix_digest(p,size)
            history.append({'name':p.name,'size':size,'sha256':sha,'device':st.st_dev,'inode':st.st_ino})
        atomic(self.dir/'history-before.json',history)
        if any(digest(self.repo/n)!=self.m[n+'_sha256'] for n in ('config.yaml','.env')):raise RuntimeError('configuration changed during final drain')

    def checkpoint(self):
        if time.monotonic()>self.started+3.5:raise RuntimeError('final drain exceeded its pre-stop budget')
        if any(digest(self.repo/n)!=self.m[n+'_sha256'] for n in ('config.yaml','.env')):raise RuntimeError('configuration changed during final drain')
        self.phase('checkpointed')

    def stop_old(self):
        self.phase('stopping_old');run(['docker','stop','--time','2',self.m['old_id']],timeout=4)
        if inspect(self.m['old_id'])['State']['Running']:raise RuntimeError('original writer did not exit')
        self.phase('old_stopped')

    def release_redemption(self):
        if self.redemption is not None:self.redemption.close();self.redemption=None

    def assert_no_writers(self):
        self.release_redemption()
        for name in LOCKS:
            if not lock_available(self.state/name):raise RuntimeError('a credential/policy/history writer remains')

    def start_candidate(self):
        old=inspect(self.m['old_id']);candidate=inspect(self.m['candidate_id'])
        if old['State']['Running'] or candidate['State']['Running']:raise RuntimeError('writer fence failed')
        run(['docker','rename',self.m['old_id'],self.service+'-rollback-'+self.token],timeout=2)
        run(['docker','rename',self.m['candidate_id'],self.service],timeout=2)
        self.phase('starting_candidate');run(['docker','start',self.m['candidate_id']],timeout=3)

    def candidate_ready(self):
        deadline=self.started+self.candidate_budget-1
        while time.monotonic()<deadline:
            info=inspect(self.m['candidate_id'])
            if not info['State']['Running']:raise RuntimeError('candidate exited during startup')
            try:
                report=json.loads(run(['nsenter','-t',str(info['State']['Pid']),'-n','/usr/bin/python3',Path(__file__).parent/'main.py','probe','--commit',self.m['candidate_commit']],timeout=3))
                writers(self.state,True);self.phase('candidate_ready',readiness=report);return
            except (RuntimeError,subprocess.TimeoutExpired):time.sleep(.15)
        raise RuntimeError('candidate readiness budget exhausted; restoring original')

    def preserve_state(self):
        count=preserve_owners(self.before,self.state)
        for name in json.loads((self.dir/'auth-files-before.json').read_text()):
            if not (self.repo/'auths'/name).is_file():raise RuntimeError('credential file disappeared')
        for item in json.loads((self.dir/'history-before.json').read_text()):
            st=(self.state/'history'/item['name']).stat()
            if st.st_dev!=item['device'] or st.st_ino!=item['inode'] or st.st_size<item['size']:raise RuntimeError('retained raw history replaced or truncated')
        self.phase('state_preserved',owners_preserved=count)

    def _commit(self):
        # Only the deployment image pointer changes; live routing/refill config does not.
        p=self.repo/'.env';old=p.read_bytes()
        if hashlib.sha256(old).hexdigest()!=self.m['.env_sha256']:raise RuntimeError('deployment image pointer changed concurrently')
        lines=old.decode().splitlines();image='CLI_PROXY_IMAGE='+self.m['candidate_image_id'];found=False
        for i,line in enumerate(lines):
            if line.startswith('CLI_PROXY_IMAGE='):lines[i]=image;found=True
        if not found:lines.append(image)
        raw=('\n'.join(lines)+'\n').encode();inode=p.stat().st_ino
        self.phase('updating_image_pointer',env_after_sha256=hashlib.sha256(raw).hexdigest())
        with p.open('r+b') as f:f.write(raw);f.truncate();f.flush();os.fsync(f.fileno())
        if p.stat().st_ino!=inode:raise RuntimeError('environment bind inode changed')
        self.phase('committed')

    def commit(self):
        with (self.dir/'commit.lock').open('a+') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            if (self.dir/'rescue-requested').exists():raise RuntimeError('watchdog owns this transaction')
            self._commit()

    def ungate(self):
        if getattr(self,'drain_connection',None) is not None:self.drain_connection.close();self.drain_connection=None
        self.release_redemption();self.gates.disable()
        if self.started is not None:
            with (self.dir/'journal.lock').open('a+') as lock:
                fcntl.flock(lock,fcntl.LOCK_EX)
                data=json.loads(self.journal.read_text())
                if 'ungated_at' not in data:
                    data['ungated_at']=time.monotonic();data['interruption_seconds']=round(data['ungated_at']-self.started,3);atomic(self.journal,data)

    def cancel_watchdog(self):
        run(['systemctl','stop',self.timer+'.timer',self.timer+'.service'],timeout=3,check=False)

    def recover(self):
        self.release_redemption()
        transaction=json.loads(self.journal.read_text()) if self.journal.exists() else {}
        phase=transaction.get('phase')
        if phase in TERMINAL_OUTCOMES:return
        old=inspect(self.m['old_id']);candidate=inspect(self.m['candidate_id'],True)
        if old['State']['Running'] and phase in ('preflight','gating','checkpointed',None):
            self.phase('original_retained');return
        self.phase('recovering')
        # Once stop was issued, settle that Docker operation before starting the
        # original. Running alone is not evidence that a pending stop is gone.
        if old['State']['Running']:run(['docker','stop','--time','2',old['Id']],timeout=4)
        if candidate and candidate['State']['Running']:run(['docker','stop','--time','2',candidate['Id']],timeout=4)
        if candidate and candidate['Name']=='/'+self.service:run(['docker','rename',candidate['Id'],self.service+'-failed-'+self.token],timeout=2)
        self.assert_no_writers()
        old=inspect(self.m['old_id'])
        if old['Name']!='/'+self.service:run(['docker','rename',old['Id'],self.service],timeout=2)
        # Restore only our exact image-pointer update, never credentials or durable state.
        before=(self.dir/'env.before').read_bytes();p=self.repo/'.env'
        journal=json.loads(self.journal.read_text())
        if journal.get('env_after_sha256') and digest(p)==journal['env_after_sha256']:
            with p.open('r+b') as f:f.write(before);f.truncate();f.flush();os.fsync(f.fileno())
        run(['docker','start',old['Id']],timeout=3)
        deadline=time.monotonic()+max(5,self.m['timing']['rollback']*1.5+2,self.m['timing'].get('rollback_wait') or 0)
        while time.monotonic()<deadline:
            info=inspect(old['Id'])
            try:
                run(['nsenter','-t',str(info['State']['Pid']),'-n','/usr/bin/python3',Path(__file__).parent/'main.py','probe','--commit',self.m['old_commit']],timeout=3)
                writers(self.state,True)
                before=json.loads((self.dir/'owners-before.json').read_text()) if (self.dir/'owners-before.json').exists() else {}
                preserve_owners(before,self.state);self.phase('rolled_back');return
            except (RuntimeError,subprocess.TimeoutExpired):time.sleep(.15)
        raise RuntimeError('original image did not become ready within measured recovery margin')
