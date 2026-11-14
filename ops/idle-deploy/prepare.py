"""Offline candidate/rollback rehearsals; fixture mounts never alias production."""
import hashlib
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

from common import ROOT,LOCKS,RUNNER_SUFFIXES,atomic,compose_leftovers,digest,inspect,key,lock_available,owners,preserve_owners,probe,run,single_flight_held,write_in_place,writers
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
    installed=[]
    def remove():
        # Every installed family is removed independently.
        for exe in list(installed):
            try:run(['nsenter','-t',str(pid),'-n',exe,'-D','INPUT']+rule,check=False)
            except (OSError,subprocess.TimeoutExpired):continue
            installed.remove(exe)
    try:
        import select
        # Unbuffered reads with an explicit line buffer: lines printed together
        # must not hide in a text buffer that select() cannot see.
        fd=child.stdout.fileno();pending=[b'']
        def line(wait):
            deadline=time.monotonic()+wait
            while b'\n' not in pending[0]:
                remaining=deadline-time.monotonic()
                if remaining<=0 or not select.select([fd],[],[],remaining)[0]:return ''
                chunk=os.read(fd,4096)
                if not chunk:return ''
                pending[0]+=chunk
            text,_,pending[0]=pending[0].partition(b'\n')
            return text.decode(errors='replace').strip()
        if line(3)!='ready':raise RuntimeError('fixture keepalive did not become ready')
        for exe in ('iptables','ip6tables'):
            run(['nsenter','-t',str(pid),'-n',exe,'-I','INPUT','1']+rule);installed.append(exe)
        child.stdin.write('go\n');child.stdin.flush()
        if line(4)!='guarded':raise RuntimeError('admission gate did not preserve established HTTP')
        if line(2)!='holding':raise RuntimeError('held-connection probe did not start')
        # A connection opened while admission is closed must be served once it
        # reopens, after a kernel SYN retransmission rather than a reset.
        time.sleep(1.5)
        remove()
        if line(12)!='held' or child.wait(timeout=2):raise RuntimeError('connection opened during admission was not served after reopening')
    finally:
        try:remove()
        finally:
            if child.poll() is None:child.kill();child.wait()


def prepare(image,commit,retire_idle_clients=False,outage_budget=40,config=None):
    if os.geteuid()!=0:raise RuntimeError('preparation needs local root for isolated namespace probes')
    interrupt_budget=outage_budget>40
    if interrupt_budget and not 40<outage_budget<=INTERRUPT_MAX_BUDGET:raise RuntimeError('unsupported outage budget')
    # Larger budgets are only usable by the interrupting cutover; idle modes reject them.
    limit=45 if interrupt_budget else 16
    os.umask(0o077)
    if single_flight_held():raise RuntimeError('another deployment controller is active; refusing to prepare')
    old=inspect('cli-proxy-api')
    if not old['State']['Running']:raise RuntimeError('production is not running')
    removed=remove_stale_runner_containers(old)
    _,hdr,_=__import__('common').api('/v8/management/config',management=True)
    old_commit=next((v for k,v in hdr.items() if k.lower()=='x-cpa-commit'),None)
    if not old_commit or len(old_commit)!=40:raise RuntimeError('cannot establish current serving revision')
    probe(old_commit)
    original_hashes={n:digest(ROOT/n) for n in ('config.yaml','.env')}
    original_config=(ROOT/'config.yaml').read_bytes();staged=staged_config(config,original_config)
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
    (directory/'config.before').write_bytes(original_config)
    if staged is not None:(directory/'config.after').write_bytes(staged)
    atomic(directory/'old-container.json',old)
    before=owners(fixture/'account-policy-state');created=[];results=[];max_stop=.1
    try:
        for kind,img,sha in [('candidate',image_id,commit),('rollback',old['Image'],old_commit)]:
            # The candidate starts on the staged configuration, the original on its own.
            write_in_place(fixture/'config.yaml',staged if kind=='candidate' and staged is not None else original_config)
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
    cycles=full_cycles(directory,fixture,token,image_id,commit,old['Image'],old_commit,retire_idle_clients=retire_idle_clients,staged=staged,original=original_config)
    timing['recovery_total']=cycles['recovery_seconds']
    check(**timing)
    compose=compose_path(directory,token,old['Image'],image_id,original_config,staged)
    atomic(directory/'rehearsal.json',{'results':results,'timing':timing,'complete_cycles':cycles,'compose_path':compose,'fixture_only':True,'candidate_image_id':image_id,'old_image_id':old['Image'],'removed_stale_runner_containers':removed})
    old_again=inspect('cli-proxy-api')
    if old_again['Id']!=old['Id'] or old_again['State']['StartedAt']!=old['State']['StartedAt']:raise RuntimeError('production changed during rehearsal')
    source=Path(__file__).parent
    manifest={'schema':1,'retire_idle_clients':retire_idle_clients,'token':token,'prepared_at':time.time(),'candidate_commit':commit,'candidate_image_id':image_id,'candidate_image':image,'old_id':old['Id'],'old_image_id':old['Image'],'old_commit':old_commit,'old_pid':old['State']['Pid'],'ports':sorted({int(k.split('/')[0]) for k in old['HostConfig']['PortBindings']}),'timing':timing,'rehearsal_sha256':digest(directory/'rehearsal.json'),'controller_files':{p.name:digest(p) for p in source.glob('*.py') if not p.name.startswith('test_')}}
    for name in ('config.yaml','.env'):
        if digest(ROOT/name)!=original_hashes[name]:raise RuntimeError('production configuration changed during rehearsal; restage')
        manifest[name+'_sha256']=original_hashes[name]
    if staged is not None:manifest['config_after_sha256']=hashlib.sha256(staged).hexdigest()
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


def full_cycles(directory,fixture,token,image,commit,old_image,old_commit,retire_idle_clients=False,staged=None,original=None):
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
            if original is not None:
                write_in_place(fixture/'config.yaml',original);(cycle/'config.before').write_bytes(original)
            if staged is not None:(cycle/'config.after').write_bytes(staged)
            unit='cliproxy-idle-fixture-worker-'+label
            manifest={'token':label,'retire_idle_clients':retire_idle_clients,'fixture_repo':str(fixture),'service_name':service,'watcher_unit':unit,'old_id':old,'candidate_id':next_id,'old_pid':info['State']['Pid'],'old_commit':old_commit,'candidate_commit':commit,'candidate_image_id':image,'ports':[8317],'host_ports':[host_port],'fixture_candidate_budget':5 if forced else 15,'force_watchdog':forced,'timing':{'rollback':8},'config.yaml_sha256':digest(fixture/'config.yaml'),'.env_sha256':digest(fixture/'.env')}
            if staged is not None:manifest['config_after_sha256']=hashlib.sha256(staged).hexdigest()
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
            # A committed cycle serves the staged config; a rollback restored the original bytes.
            if staged is not None and (fixture/'config.yaml').read_bytes()!=(original if forced else staged):raise RuntimeError('fixture cycle did not stage or restore configuration exactly')
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
        if original is not None:write_in_place(fixture/'config.yaml',original)
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


def remove_stale_runner_containers(old):
    """Stopped runner containers with the production compose labels break `docker
    compose up` (it exits 0 with the router never started). Remove only those;
    any other labelled container refuses preparation."""
    removed=[]
    for c in compose_leftovers(old):
        if c['state']=='running' or not c['name'].startswith('cli-proxy-api') or not any(x in c['name'] for x in RUNNER_SUFFIXES):
            raise RuntimeError('unexpected container carries the production compose labels: '+c['name'])
        run(['docker','rm',c['id']],timeout=10);removed.append(c['name'])
    if compose_leftovers(old):raise RuntimeError('production compose labels are still shared after cleanup')
    return removed


def staged_config(config,original):
    if not config:return None
    p=Path(config).resolve()
    if not p.is_relative_to(ROOT) or not p.is_file():raise RuntimeError('staged configuration must be a file inside the project')
    raw=p.read_bytes()
    if raw==original:raise RuntimeError('staged configuration is identical to the live configuration')
    return raw


COMPOSE_FILES=('docker-compose.yml','docker-compose.override.yml')
PRODUCTION_PORTS=(1455,8085,8317,11451,51121,54545)


def render_compose(project,image):
    """The production compose files, rendered exactly as `docker compose up` would."""
    args=['docker','compose','--project-directory',str(ROOT)]
    for name in COMPOSE_FILES:
        if (ROOT/name).exists():args+=['-f',str(ROOT/name)]
    r=subprocess.run(args+['-p',project,'config','--format','json'],env=dict(os.environ,CLI_PROXY_IMAGE=image),stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=30)
    if r.returncode:raise RuntimeError('production compose configuration did not render')
    return json.loads(r.stdout)


def dryrun_compose(rendered,project,fixture,port,image):
    """Same service definition; only the project, container name, host port,
    restart policy and fixture mounts differ."""
    if list(rendered.get('services',{}))!=['cli-proxy-api']:raise RuntimeError('unexpected production compose services')
    svc=rendered['services']['cli-proxy-api']
    svc.pop('build',None);svc['image']=image;svc['container_name']=project;svc['restart']='no'
    svc['ports']=[{'mode':'ingress','target':8317,'published':str(port),'host_ip':'127.0.0.1','protocol':'tcp'}]
    volumes=[]
    for v in svc.get('volumes',[]):
        if v.get('type')!='bind':raise RuntimeError('unexpected production compose volume')
        target=fixture/Path(v['source']).resolve().relative_to(ROOT)
        if not target.exists() or not target.resolve().is_relative_to(fixture.resolve()):raise RuntimeError('compose dry run would mount production state')
        volumes.append(dict(v,source=str(target)))
    svc['volumes']=volumes;rendered['name']=project
    return rendered


def http_get(port,path,auth=False,timeout=2):
    import urllib.request
    headers={'Authorization':'Bearer '+key('CLI_PROXY_API_KEY')} if auth else {}
    req=urllib.request.Request('http://127.0.0.1:'+str(port)+path,headers=headers)
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=timeout) as response:
        return response.status,response.read()


def compose_up(project,file,fixture,port,image,config,limit=60):
    """`docker compose up -d` must exit 0 AND leave the container running and serving."""
    write_in_place(fixture/'config.yaml',config)
    atomic(file,dryrun_compose(render_compose(project,image),project,fixture,port,image))
    start=time.monotonic()
    r=subprocess.run(['docker','compose','-p',project,'-f',str(file),'up','-d','--no-deps','cli-proxy-api'],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=120)
    if r.returncode:raise RuntimeError('compose up exited '+str(r.returncode))
    last='not checked'
    while True:
        info=inspect(project,True);state=info['State']['Status'] if info else 'missing'
        if info and info['Image']!=image:raise RuntimeError('compose dry run serves an unexpected image')
        if state=='running':
            try:
                checks={'healthz':http_get(port,'/healthz')[0],'models':http_get(port,'/v1/models',auth=True)[0]}
                status,text=http_get(port,'/v1/account-policy/subagent-advice?format=text',timeout=15);checks['advice_text']=status
                status,raw=http_get(port,'/v1/account-policy/subagent-advice',timeout=15);checks['advice_json']=status;json.loads(raw)
                if set(checks.values())=={200} and text.strip():
                    return {'state':'running','seconds':round(time.monotonic()-start,3),**checks,'advice_first_line':text.decode(errors='replace').splitlines()[0][:200]}
                last='http '+json.dumps(checks)
            except Exception as error:last='probe failed: '+type(error).__name__
        elif state in ('exited','dead','missing') or (state=='created' and time.monotonic()-start>3):
            # "created" after a successful compose up is the 2026-11-14 failure: never started.
            raise RuntimeError('compose left the container in State '+state)
        if time.monotonic()-start>limit:raise RuntimeError('compose dry run not serving within '+str(limit)+'s: '+state+', '+last)
        time.sleep(.5)


def compose_path(directory,token,old_image,image,original,staged):
    """Rehearse the production compose definition under a separate project on a
    spare loopback port: original -> candidate -> original, each running and serving.
    Credential-free fixture: empty auths and policy state, copied plugins/static."""
    project='cliproxy-dryrun-'+token;fixture=Path(directory)/'compose-fixture';fixture.mkdir(mode=0o700)
    for name in ('auths','account-policy-state','logs'):(fixture/name).mkdir(mode=0o700)
    for name in ('plugins','static'):
        (fixture/name).mkdir(mode=0o700)
        if (ROOT/name).is_dir():run(['rsync','-a',str(ROOT/name)+'/',str(fixture/name)+'/'],timeout=120)
    (fixture/'config.yaml').write_bytes(original)
    reservation=socket.socket();reservation.bind(('127.0.0.1',0));port=reservation.getsockname()[1];reservation.close()
    if port in PRODUCTION_PORTS:raise RuntimeError('spare port collides with production')
    file=Path(directory)/'compose-dryrun.json';steps=[]
    try:
        for step,img,config in (('original',old_image,original),('candidate',image,staged if staged is not None else original),('rollback',old_image,original)):
            steps.append(dict(step=step,**compose_up(project,file,fixture,port,img,config)))
    finally:
        subprocess.run(['docker','compose','-p',project,'-f',str(file),'down','--remove-orphans','--timeout','5'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120)
        if run(['docker','ps','-aq','--filter','label=com.docker.compose.project='+project],timeout=5).strip():raise RuntimeError('compose dry run containers remain')
    return {'project':project,'port':port,'steps':steps,'candidate_running':True,'rollback_running':True,'staged_config':staged is not None}


def compose_dryrun(image,config=None):
    """Standalone compose-path dry run (no release, no production change)."""
    if os.geteuid()!=0:raise RuntimeError('the dry run needs root for its private fixture')
    os.umask(0o077)
    old=inspect('cli-proxy-api');image_id=run(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip()
    original=(ROOT/'config.yaml').read_bytes();staged=staged_config(config,original)
    token='dry'+uuid.uuid4().hex[:8];directory=ROOT/'.tmp/idle-deploy'/('dryrun-'+token);directory.mkdir(parents=True,mode=0o700)
    result=compose_path(directory,token,old['Image'],image_id,original,staged)
    atomic(directory/'compose-dryrun-result.json',result)
    return dict(result,directory=str(directory))
