"""Offline candidate/rollback rehearsals; fixture mounts never alias production."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import uuid
import socket
from backend import Backend
from core import Cutover,IdleWindow

from common import ROOT,LOCKS,atomic,digest,inspect,lock_available,owners,preserve_owners,probe,run,writers
from backend import create_stopped
from core import INTERRUPT_MAX_BUDGET,INTERRUPT_ROLLBACK_RESERVE,validate_interrupt_timing,validate_timing


SOURCES={'config.yaml':'config.yaml','account-policy-state':'account-policy-state','auths':'auths','plugins':'plugins','static':'static'}


def fixture_container(fixture,name,image,command=None):
    args=['docker','create','--name',name,'--network','none','--memory','1024m','--memory-swap','1024m','--cpus','2','--log-opt','max-size=10m','--tmpfs','/tmp:rw,size=128m']
    for source,target in SOURCES.items():
        p=(fixture/source).resolve()
        if not p.is_relative_to(fixture.resolve()) or p.samefile(ROOT/source):raise RuntimeError('fixture path aliases production')
        args+=['--mount','type=bind,source='+str(p)+',target=/CLIProxyAPI/'+target+(',readonly' if source=='config.yaml' else '')]
    args+=['--mount','type=bind,source='+str(fixture/'logs')+',target=/CLIProxyAPI/logs']
    args+=[image]+(command or ['./CLIProxyAPI','--local-model'])
    return run(args).decode().strip()


def startup(container,commit,limit=16):
    start=time.monotonic();run(['docker','start',container],timeout=4)
    while time.monotonic()-start<limit:
        info=inspect(container)
        if not info['State']['Running']:raise RuntimeError('isolated candidate exited')
        try:
            raw=run(['nsenter','-t',str(info['State']['Pid']),'-n','/usr/bin/python3',Path(__file__).parent/'main.py','probe','--commit',commit],timeout=3)
            return time.monotonic()-start,json.loads(raw)
        except (RuntimeError,subprocess.TimeoutExpired):time.sleep(.15)
    raise RuntimeError('isolated startup exceeded budget; automatic deployment is blocked')


def stop(container,state):
    start=time.monotonic();run(['docker','stop','--time','2',container],timeout=4)
    if inspect(container)['State']['Running']:raise RuntimeError('fixture writer did not stop')
    for name in LOCKS:
        if not lock_available(state/name):raise RuntimeError('fixture ownership lock was not released')
    return time.monotonic()-start


def gate_rehearsal(container,token):
    """Reject a new socket while an established HTTP socket stays usable."""
    pid=inspect(container)['State']['Pid'];tag='cliproxy-fixture-'+token
    script='''import http.client,json,sys
c=http.client.HTTPConnection('127.0.0.1',8317,timeout=2)
c.request('GET','/healthz');r=c.getresponse();r.read();assert r.status==200
print('ready',flush=True);sys.stdin.readline()
c.request('GET','/healthz');r=c.getresponse();r.read();assert r.status==200
c.close()
try:
 n=http.client.HTTPConnection('127.0.0.1',8317,timeout=1);n.request('GET','/healthz');n.getresponse()
except (OSError,http.client.HTTPException):print('guarded',flush=True)
else:raise AssertionError('new connection passed closed admission')
print('holding',flush=True)
h=http.client.HTTPConnection('127.0.0.1',8317,timeout=12)
h.request('GET','/healthz');r=h.getresponse();r.read();assert r.status==200
print('held',flush=True)
'''
    child=subprocess.Popen(['nsenter','-t',str(pid),'-n','python3','-c',script],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
    rule=['-p','tcp','--dport','8317','--syn','-m','comment','--comment',tag,'-j','DROP']
    installed=False
    try:
        import select
        def line(wait):
            if not select.select([child.stdout],[],[],wait)[0]:return ''
            return child.stdout.readline().strip()
        if line(3)!='ready':raise RuntimeError('fixture keepalive did not become ready')
        for exe in ('iptables','ip6tables'):run(['nsenter','-t',str(pid),'-n',exe,'-I','INPUT','1']+rule)
        installed=True
        child.stdin.write('go\n');child.stdin.flush()
        if line(4)!='guarded':raise RuntimeError('admission gate did not preserve established HTTP')
        if line(2)!='holding':raise RuntimeError('held-connection probe did not start')
        # A connection opened while admission is closed must be served once it
        # reopens, after a kernel SYN retransmission rather than a reset.
        time.sleep(1.5)
        for exe in ('iptables','ip6tables'):run(['nsenter','-t',str(pid),'-n',exe,'-D','INPUT']+rule)
        installed=False
        if line(12)!='held' or child.wait(timeout=2):raise RuntimeError('connection opened during admission was not served after reopening')
    finally:
        if installed:
            for exe in ('iptables','ip6tables'):run(['nsenter','-t',str(pid),'-n',exe,'-D','INPUT']+rule,check=False)
        if child.poll() is None:child.kill();child.wait()


def prepare(image,commit,retire_idle_clients=False,outage_budget=40):
    if os.geteuid()!=0:raise RuntimeError('preparation needs local root for isolated namespace probes')
    interrupt_budget=outage_budget>40
    if interrupt_budget and not 40<outage_budget<=INTERRUPT_MAX_BUDGET:raise RuntimeError('unsupported outage budget')
    # Larger budgets are only usable by the interrupting cutover; idle modes reject them.
    limit=45 if interrupt_budget else 16
    os.umask(0o077)
    old=inspect('cli-proxy-api')
    if not old['State']['Running']:raise RuntimeError('production is not running')
    _,hdr,_=__import__('common').api('/v8/management/config',management=True)
    old_commit=next((v for k,v in hdr.items() if k.lower()=='x-cpa-commit'),None)
    if not old_commit or len(old_commit)!=40:raise RuntimeError('cannot establish current serving revision')
    probe(old_commit)
    original_hashes={n:digest(ROOT/n) for n in ('config.yaml','.env')}
    image_id=run(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip()
    label=run(['docker','image','inspect',image_id,'--format','{{index .Config.Labels "org.opencontainers.image.revision"}}']).decode().strip()
    if label!=commit:raise RuntimeError('candidate image/source identity mismatch')
    token=uuid.uuid4().hex[:10];directory=ROOT/'.tmp/idle-deploy'/token;directory.mkdir(parents=True,mode=0o700)
    fixture=directory/'fixture';fixture.mkdir(mode=0o700)
    for source in SOURCES:
        target=fixture/source
        if source=='config.yaml':shutil.copy2(ROOT/source,target)
        else:
            target.mkdir(mode=0o700);run(['rsync','-a','--numeric-ids',str(ROOT/source)+'/',str(target)+'/'],timeout=120)
    (fixture/'logs').mkdir(mode=0o700)
    # Flush the fixture copy now: otherwise its writeback stalls Docker's
    # metadata fsync and the bounded container create below times out.
    run(['sync','-f',str(fixture/'account-policy-state')],timeout=120)
    shutil.copy2(ROOT/'.env',directory/'env.before')
    atomic(directory/'old-container.json',old)
    before=owners(fixture/'account-policy-state');created=[];results=[];max_stop=.1
    try:
        for kind,img,sha in [('candidate',image_id,commit),('rollback',old['Image'],old_commit)]:
            c=fixture_container(fixture,'cliproxy-idle-fixture-'+token+'-'+kind,img);created.append(c)
            elapsed,report=startup(c,sha,limit);writers(fixture/'account-policy-state',True)
            count=preserve_owners(before,fixture/'account-policy-state')
            if kind=='candidate':gate_rehearsal(c,token)
            stop_time=stop(c,fixture/'account-policy-state');max_stop=max(max_stop,stop_time)
            results.append(dict(kind=kind,start_seconds=round(elapsed,3),stop_seconds=round(stop_time,3),owners_preserved=count,**report))
        # Force an unsuccessful candidate start, then recover original against
        # candidate-written fixture state. No production state is mounted.
        failed=fixture_container(fixture,'cliproxy-idle-fixture-'+token+'-failed',image_id,['/bin/false']);created.append(failed)
        run(['docker','start',failed]);run(['docker','wait',failed],timeout=4)
        recovery=fixture_container(fixture,'cliproxy-idle-fixture-'+token+'-recovery',old['Image']);created.append(recovery)
        forced_start=time.monotonic();elapsed,_=startup(recovery,old_commit,limit)
        preserve_owners(before,fixture/'account-policy-state');max_stop=max(max_stop,stop(recovery,fixture/'account-policy-state'))
        results.append({'kind':'forced_failure_recovery','start_seconds':round(elapsed,3),'total_seconds':round(time.monotonic()-forced_start,3)})
    finally:
        for c in created:run(['docker','rm','-f',c],check=False)
    timing={'candidate':results[0]['start_seconds'],'rollback':max(results[1]['start_seconds'],results[2]['start_seconds']),'stop':max(2,round(max_stop,3)),'budget':outage_budget}
    check=validate_interrupt_timing if interrupt_budget else validate_timing
    if interrupt_budget:
        # Recovery waits for an original that may repeat an interrupted import.
        timing['rollback_wait']=INTERRUPT_ROLLBACK_RESERVE-timing['stop']-2
    check(**timing)
    cycles=full_cycles(directory,fixture,token,image_id,commit,old['Image'],old_commit,retire_idle_clients=retire_idle_clients)
    timing['recovery_total']=cycles['recovery_seconds']
    check(**timing)
    atomic(directory/'rehearsal.json',{'results':results,'timing':timing,'complete_cycles':cycles,'fixture_only':True,'candidate_image_id':image_id,'old_image_id':old['Image']})
    old_again=inspect('cli-proxy-api')
    if old_again['Id']!=old['Id'] or old_again['State']['StartedAt']!=old['State']['StartedAt']:raise RuntimeError('production changed during rehearsal')
    source=Path(__file__).parent
    manifest={'schema':1,'retire_idle_clients':retire_idle_clients,'token':token,'prepared_at':time.time(),'candidate_commit':commit,'candidate_image_id':image_id,'candidate_image':image,'old_id':old['Id'],'old_image_id':old['Image'],'old_commit':old_commit,'old_pid':old['State']['Pid'],'ports':sorted({int(k.split('/')[0]) for k in old['HostConfig']['PortBindings']}),'timing':timing,'rehearsal_sha256':digest(directory/'rehearsal.json'),'controller_files':{p.name:digest(p) for p in source.glob('*.py') if not p.name.startswith('test_')}}
    for name in ('config.yaml','.env'):
        if digest(ROOT/name)!=original_hashes[name]:raise RuntimeError('production configuration changed during rehearsal; restage')
        manifest[name+'_sha256']=original_hashes[name]
    if digest(directory/'env.before')!=original_hashes['.env'] or digest(fixture/'config.yaml')!=original_hashes['config.yaml']:raise RuntimeError('fixture configuration snapshot drifted')
    candidate=create_stopped(old,'cli-proxy-api-idle-next-'+token,image_id)
    manifest['candidate_id']=candidate
    atomic(directory/'release.json',manifest)
    return directory/'release.json'


class FixtureBackend(Backend):
    def preflight(self,require_no_connections=True):
        if not self.m.get('fixture_repo') or self.repo==ROOT:raise RuntimeError('fixture worker cannot target production')
        if not inspect(self.m['old_id'])['State']['Running']:raise RuntimeError('fixture original not running')
        writers(self.state,True)
        if getattr(self,'evidence',None) is not None:
            run(['nsenter','-t',str(self.m['old_pid']),'-n','/usr/bin/python3',Path(__file__).parent/'main.py','probe','--commit',self.m['old_commit']],timeout=3)
            after=time.monotonic();deadline=after+3
            while self.evidence().get('timestamp',0)<after:
                if time.monotonic()>deadline:raise RuntimeError('fixture capture did not observe the readiness probes')
                time.sleep(.05)
            if not self.idle.ready(self.evidence()):raise RuntimeError('readiness probes invalidated the fixture quiet window')
        self.candidate_budget=self.m['fixture_candidate_budget']
        self.redemption=(self.state/'redemption.lock').open('rb')
        import fcntl
        fcntl.flock(self.redemption,fcntl.LOCK_EX|fcntl.LOCK_NB)
        self.snapshot();self.phase('preflight')
    def drained_and_quiet(self):
        from common import connections
        control=self.drain_connection
        if control is None:raise RuntimeError('fixture management socket is unavailable')
        try:
            if connections(self.m['old_pid'],self.m['ports'],exclude=control.exclude):raise RuntimeError('fixture connection still active')
            if control.activity()!=0:raise RuntimeError('fixture request lease still active')
        finally:
            control.close();self.drain_connection=None
        deadline=self.started+3
        while time.monotonic()<deadline:
            if not connections(self.m['old_pid'],self.m['ports']):return
            time.sleep(.05)
        raise RuntimeError('fixture management socket did not drain')


def fixture_worker(path):
    with (path.parent/'worker.lock').open('a+') as worker:
        import fcntl
        fcntl.flock(worker,fcntl.LOCK_EX|fcntl.LOCK_NB)
        from main import Capture
        manifest=json.loads(path.read_text());capture=Capture(manifest['old_pid'])
        try:
            idle,evidence=fixture_observation(capture)
            if manifest.get('retire_idle_clients',False):
                from maintenance import MaintenanceBackend
                class FixtureMaintenanceBackend(MaintenanceBackend,FixtureBackend):
                    pass
                b=FixtureMaintenanceBackend(path,evidence,idle)
            else:
                b=FixtureBackend(path,evidence,idle)
            # A forced start must be recovered by the independent watchdog.
            if b.m.get('force_watchdog'):
                def never_ready():
                    while True:time.sleep(1)
                b.candidate_ready=never_ready
            Cutover(b).run()
        finally:capture.close()


def fixture_observation(capture,now=time.monotonic):
    """Seed only the known isolated fixture's initial quiet interval.

    All subsequent traffic, health, framing, and loss evidence comes from the
    actual packet observer. New business traffic is never mapped to the baseline.
    """
    deadline=now()+3
    while True:
        first=capture.read(.1)
        if first.get('healthy') is True and first.get('pending')==0 and first.get('capture_drops')==0:break
        if now()>deadline:raise RuntimeError('fixture capture did not become healthy')
    baseline=first['last_business'];idle=IdleWindow(now=now);idle.started-=301
    def evidence():
        raw=capture.read()
        result=dict(raw)
        if result.get('last_business')==baseline:result['last_business']=idle.started
        return result
    return idle,evidence


def full_cycles(directory,fixture,token,image,commit,old_image,old_commit,retire_idle_clients=False):
    created=[];reports=[];clients=[]
    # Reserve an otherwise unused host port. Host gate code is exercised exactly,
    # but no production listener port is ever included in the host fixture rules.
    reservation=socket.socket();reservation.bind(('127.0.0.1',0));host_port=reservation.getsockname()[1]
    assert host_port not in (1455,8085,8317,11451,51121,54545)
    try:
        for forced in (False,True):
            label=token+('f' if forced else 's');cycle=directory/('forced-cycle' if forced else 'success-cycle');cycle.mkdir(mode=0o700)
            service='cliproxy-idle-fixture-'+label+'-old'
            old=fixture_container(fixture,service,old_image);created.append(old);startup(old,old_commit)
            next_id=fixture_container(fixture,'cliproxy-idle-fixture-'+label+'-next',image,['/bin/sleep','60'] if forced else None);created.append(next_id)
            info=inspect(old)
            client=idle_fixture_client(info['State']['Pid']) if retire_idle_clients else None
            if client is not None:clients.append(client)
            shutil.copy2(directory/'env.before',fixture/'.env');shutil.copy2(directory/'env.before',cycle/'env.before')
            unit='cliproxy-idle-fixture-worker-'+label
            manifest={'token':label,'retire_idle_clients':retire_idle_clients,'fixture_repo':str(fixture),'service_name':service,'watcher_unit':unit,'old_id':old,'candidate_id':next_id,'old_pid':info['State']['Pid'],'old_commit':old_commit,'candidate_commit':commit,'candidate_image_id':image,'ports':[8317],'host_ports':[host_port],'fixture_candidate_budget':5 if forced else 15,'force_watchdog':forced,'timing':{'rollback':8},'config.yaml_sha256':digest(fixture/'config.yaml'),'.env_sha256':digest(fixture/'.env')}
            atomic(cycle/'release.json',manifest)
            run(['systemd-run','--quiet','--collect','--unit',unit,'/usr/bin/python3',Path(__file__).parent/'main.py','fixture-worker','--release',cycle/'release.json'],timeout=4)
            deadline=time.monotonic()+35
            while time.monotonic()<deadline:
                journal=cycle/'transaction.json'
                if journal.exists():
                    data=json.loads(journal.read_text())
                    if data.get('phase') in ('committed','rolled_back') and data.get('ungated_at'):break
                time.sleep(.1)
            else:raise RuntimeError('complete fixture cutover/watchdog cycle exceeded budget')
            if data['phase']!=('rolled_back' if forced else 'committed'):raise RuntimeError('fixture cycle took an unexpected recovery path')
            if data['interruption_seconds']>=30:raise RuntimeError('fixture cycle left insufficient 40-second margin')
            from gates import Gates
            gate=Gates(label,[8317],info['State']['Pid'],[host_port])
            # Verify cleanup in both host families, rather than merely trusting
            # a successful return from the removal routine.
            for exe in ('iptables','ip6tables'):
                if gate.tag.encode() in run([exe,'-S']):raise RuntimeError('fixture admission rule survived cleanup')
            if client is not None:
                output,_=client.communicate(timeout=3)
                if client.returncode or output.strip()!='closed':raise RuntimeError('fixture idle socket was not retired')
            recovery=data['ungated_at']-data.get('rescue_started',data['started'])
            reports.append({'forced_watchdog':forced,'interruption_seconds':data['interruption_seconds'],'recovery_seconds':round(recovery,3),'phase':data['phase'],'idle_clients_retired':retire_idle_clients})
            active=old if forced else next_id;stop(active,fixture/'account-policy-state')
            run(['systemctl','stop',unit],check=False)
    finally:
        reservation.close()
        for child in clients:
            if child.poll() is None:child.kill();child.wait()
        for c in created:run(['docker','rm','-f',c],check=False)
    return {'cycles':reports,'recovery_seconds':reports[1]['recovery_seconds'],'all_host_gate_families':True,'independent_watchdog':True}


def idle_fixture_client(pid):
    """An existing idle HTTP socket must not prevent the fixture cutover."""
    import select
    script="""import http.client
c=http.client.HTTPConnection('127.0.0.1',8317,timeout=30)
c.request('GET','/healthz');r=c.getresponse();r.read();assert r.status==200
assert c.sock is not None
print('ready',flush=True)
try:
 data=c.sock.recv(1)
except (ConnectionResetError,ConnectionAbortedError):
 data=b''
assert not data
c.close();print('closed',flush=True)
"""
    child=subprocess.Popen(['nsenter','-t',str(pid),'-n','/usr/bin/python3','-c',script],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
    if not select.select([child.stdout],[],[],3)[0] or child.stdout.readline().strip()!='ready':
        child.kill();child.wait();raise RuntimeError('fixture idle client did not become ready')
    return child
