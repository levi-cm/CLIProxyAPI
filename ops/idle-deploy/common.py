"""Protected local I/O and read-only router probes. Never log upstream bodies."""
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import sys
import urllib.request
import uuid

ROOT=Path('/DATA/AppData/CLIProxyAPI')


WATCHER_LOCK=ROOT/'.tmp/idle-deploy/watcher.lock'


def single_flight_held(lock=None):
    """True while a deployment controller holds the host-wide single-flight lock."""
    lock=Path(lock or WATCHER_LOCK);lock.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    with lock.open('a+') as f:
        try:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:return True
        fcntl.flock(f,fcntl.LOCK_UN);return False


def require_no_active_deployment(lock=None):
    # The lock stays authoritative; this only refuses a launch that would die on it.
    if single_flight_held(lock):raise RuntimeError('another deployment controller holds the single-flight lock; refusing a concurrent release')


def confirm_controller(unit,lock=None,timeout=10,clock=None,sleep=None,state=None):
    """Wait until the launched unit owns the single-flight lock, or fail loudly."""
    import time
    clock=clock or time.monotonic;sleep=sleep or time.sleep
    state=state or (lambda: run(['systemctl','is-active',unit],timeout=3,check=False).decode().strip())
    deadline=clock()+timeout
    while clock()<deadline:
        if single_flight_held(lock):return True
        if state() in ('failed','inactive'):raise RuntimeError('deployment controller exited before taking the single-flight lock')
        sleep(.2)
    raise RuntimeError('deployment controller did not take the single-flight lock')


def run(args,timeout=8,check=True):
    r=subprocess.run([str(a) for a in args],stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=timeout)
    if check and r.returncode: raise RuntimeError(str(args[0])+' failed with exit '+str(r.returncode))
    return r.stdout


def atomic(path,value):
    path=Path(path);path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    temp=path.with_name(path.name+'.'+str(os.getpid())+'.'+uuid.uuid4().hex+'.tmp')
    fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
    with os.fdopen(fd,'w') as f:
        json.dump(value,f,indent=2);f.write('\n');f.flush();os.fsync(f.fileno())
    os.replace(temp,path)


def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()

def inspect(name,optional=False):
    raw=run(['docker','inspect',name],check=not optional)
    return json.loads(raw)[0] if raw else None


def key(name):
    for line in (ROOT/'.env').read_text().splitlines():
        k,sep,value=line.partition('=')
        if sep and k.strip()==name:
            value=value.strip().strip('\"\'')
            if value and not any(c in value for c in '\r\n'):return value
    raise RuntimeError('required protected credential variable is unavailable')


def api(path,address='127.0.0.1',management=False,body=None,headers=None,timeout=1):
    hdr={'Authorization':'Bearer '+key('CLI_PROXY_MANAGEMENT_KEY' if management else 'CLI_PROXY_API_KEY'),'Content-Type':'application/json'}
    hdr.update(headers or {})
    req=urllib.request.Request('http://'+address+':8317'+path,headers=hdr,data=None if body is None else json.dumps(body).encode())
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=timeout) as response:
        return response.status,dict(response.headers),response.read()


def probe(commit,all_interfaces=True):
    addresses=('127.0.0.1','192.168.178.156','100.82.251.30') if all_interfaces else ('127.0.0.1',)
    for address in addresses:
        for path in ('/healthz','/v1/models'):
            if api(path,address)[0]!=200:raise RuntimeError('router readiness failed')
    status,headers,_=api('/v8/management/config',management=True)
    got=next((v for k,v in headers.items() if k.lower()=='x-cpa-commit'),None)
    if status!=200 or got!=commit:raise RuntimeError('serving source commit does not match the reviewed release')
    s=json.loads(api('/v8/management/account-policy/diagnostics',management=True)[2])['settings']
    if not (s['enabled'] and s['automation_required'] and s['automation']=='auto_expiring' and not s.get('read_only',False) and s['expiry_guard_seconds']>=600 and 'codex_rate_limits' in s['credit_types']):
        raise RuntimeError('mandatory refill/authority guard failed')
    if not json.loads(api('/v1/account-policy/usage-history/status')[2])['healthy']:raise RuntimeError('usage history is not ready')
    return {'commit':commit,'mandatory_refills':True,'history_healthy':True,'interfaces':len(addresses)}


def activity():
    a=json.loads(api('/v8/management/account-policy/dashboard?range=1h',management=True)[2])['activity']
    if not a['available']:raise RuntimeError('request accounting is unavailable')
    return sum(x['active_requests'] for x in a['accounts'])


LOCKS=('owner.lock','history/writer.lock','redemption.lock')

def lock_available(path):
    with Path(path).open('rb') as f:
        try:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:return False
        fcntl.flock(f,fcntl.LOCK_UN);return True


def writers(state,held):
    for name in LOCKS[:2]:
        if lock_available(Path(state)/name)==held:raise RuntimeError('authoritative writer exclusion failed')


def _tcp_identity(endpoint):
    if not isinstance(endpoint,tuple) or len(endpoint)!=2:
        raise ValueError('TCP endpoint must be an (IP, port) tuple')
    address,port=endpoint
    if not isinstance(address,str) or '%' in address or type(port) is not int or not 1<=port<=65535:
        raise ValueError('TCP endpoint requires an unscoped IP and valid port')
    address=ipaddress.ip_address(address)
    if isinstance(address,ipaddress.IPv6Address) and address.ipv4_mapped is not None:
        address=address.ipv4_mapped
    return address,port


def _proc_tcp_endpoint(value,family):
    address,port=value.rsplit(':',1)
    if len(address)!=(8 if family=='tcp' else 32):
        raise ValueError('unexpected /proc TCP address width')
    # Linux renders each native-endian u32 separately, including for IPv6.
    packed=b''.join(int(address[i:i+8],16).to_bytes(4,sys.byteorder) for i in range(0,len(address),8))
    return _tcp_identity((str(ipaddress.ip_address(packed)),int(port,16)))


def connections(pid,ports,exclude=None):
    """Count active server sockets, optionally proving one control socket exists.

    exclude is ((server_ip, server_port), (client_ip, client_port)). IPv6
    sockaddr flow/scope fields must be removed by the caller; scoped IPs fail
    closed because /proc TCP rows cannot prove their interface identity.
    """
    expected=None
    if exclude is not None:
        try:
            if not isinstance(exclude,tuple) or len(exclude)!=2:
                raise ValueError('TCP exclusion must contain two endpoint tuples')
            expected=tuple(_tcp_identity(endpoint) for endpoint in exclude)
        except (TypeError,ValueError) as err:
            raise RuntimeError('malformed control TCP tuple') from err
    count=0;matched=0
    for family in ('tcp','tcp6'):
        for line in Path(f'/proc/{pid}/net/{family}').read_text().splitlines()[1:]:
            try:
                fields=line.split()
                if int(fields[1].rsplit(':',1)[1],16) not in ports or fields[3] in ('06','07','0A'):
                    continue
                if expected is not None and fields[3]=='01' and (_proc_tcp_endpoint(fields[1],family),_proc_tcp_endpoint(fields[2],family))==expected:
                    matched+=1
                else:
                    count+=1
            except (IndexError,TypeError,ValueError) as err:
                raise RuntimeError('malformed /proc TCP socket row') from err
    if expected is not None and matched!=1:
        raise RuntimeError('control TCP tuple must match exactly one established socket')
    return count


def owners(state):
    data=json.loads((Path(state)/'conversation-bindings.json').read_text())
    return {json.dumps(r['key'],sort_keys=True):r['owner'] for r in data['records']}


def preserve_owners(before,state):
    after=owners(state)
    if any(after.get(k)!=v for k,v in before.items()):raise RuntimeError('durable conversation ownership changed')
    return len(before)

def prefix_digest(path,size):
    sha=hashlib.sha256()
    with Path(path).open('rb') as f:
        remaining=size
        while remaining:
            chunk=f.read(min(1<<20,remaining))
            if not chunk:raise RuntimeError('retained history was truncated')
            sha.update(chunk);remaining-=len(chunk)
    return sha.hexdigest()
