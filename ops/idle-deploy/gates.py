"""Admission exclusion for one pinned Docker release; existing sockets are retained."""
import subprocess
from pathlib import Path
from common import run


class Gates:
    def __init__(self,token,ports,pid,host_ports=None):
        self.tag='cliproxy-idle-'+token
        self.ports=','.join(str(p) for p in sorted(ports));self.pid=pid
        self.host_ports=','.join(str(p) for p in sorted(host_ports or ports))

    def freeze_idle_clients(self,exclude):
        server,client=exclude
        if server!=('127.0.0.1',8317) or client[0]!='127.0.0.1' or '8317' not in self.ports.split(','):raise RuntimeError('unexpected maintenance control tuple')
        chain='CPM'+self.tag.removeprefix('cliproxy-idle-')[:16]
        for exe in ('iptables','ip6tables'):
            prefix=['nsenter','-t',str(self.pid),'-n',exe,'-w','1']
            run(prefix+['-N',chain],timeout=2)
            if exe=='iptables':run(prefix+['-A',chain,'-p','tcp','-s',client[0],'-d',server[0],'--sport',str(client[1]),'--dport',str(server[1]),'-j','ACCEPT'],timeout=2)
            run(prefix+['-A',chain,'-p','tcp','-m','multiport','--dports',self.ports,'-j','DROP'],timeout=2)
            run(prefix+['-I','INPUT','1','-j',chain],timeout=2)

    def clear_idle_freeze(self):
        if not Path(f'/proc/{self.pid}').exists():return
        chain='CPM'+self.tag.removeprefix('cliproxy-idle-')[:16]
        for exe in ('iptables','ip6tables'):
            prefix=['nsenter','-t',str(self.pid),'-n',exe,'-w','1']
            if chain.encode() not in run(prefix+['-S'],timeout=2):continue
            check=subprocess.run(prefix+['-C','INPUT','-j',chain],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
            if check.returncode==0:run(prefix+['-D','INPUT','-j',chain],timeout=2)
            elif check.returncode!=1:raise RuntimeError('maintenance admission cleanup could not be verified')
            check=subprocess.run(prefix+['-S',chain],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
            if check.returncode==0:
                run(prefix+['-F',chain],timeout=2);run(prefix+['-X',chain],timeout=2)
            elif check.returncode!=1:raise RuntimeError('maintenance admission cleanup could not be verified')
            if chain.encode() in run(prefix+['-S'],timeout=2):raise RuntimeError('maintenance admission rule remains')

    def match(self,host=False):
        return ['-p','tcp','-m','multiport','--dports',self.host_ports if host else self.ports,'--syn','-m','comment','--comment',self.tag,'-j','REJECT','--reject-with','tcp-reset']

    def rules(self):
        for exe in ('iptables','ip6tables'):
            for chain in ('INPUT','OUTPUT','DOCKER-USER'):
                if chain=='DOCKER-USER':
                    check=subprocess.run([exe,'-w','1','-S',chain],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
                    if exe=='ip6tables' and check.returncode==1:continue
                    if check.returncode:raise RuntimeError('Docker admission chain could not be established')
                yield [exe],chain,self.match(True)
            yield ['nsenter','-t',str(self.pid),'-n',exe],'INPUT',self.match()

    def enable(self):
        for prefix,chain,match in self.rules():run(prefix+['-w','1','-I',chain,'1']+match,timeout=3)

    def disable(self):
        errors=[]
        try:self.clear_idle_freeze()
        except (RuntimeError,OSError,subprocess.TimeoutExpired):errors.append('maintenance admission cleanup incomplete')
        for exe in ('iptables','ip6tables'):
            pairs=[([exe],c) for c in ('INPUT','OUTPUT','DOCKER-USER')]
            pairs.append((['nsenter','-t',str(self.pid),'-n',exe],'INPUT'))
            for prefix,chain in pairs:
                actual=self.match(len(prefix)==1)
                for _ in range(4):
                    try:
                        check=subprocess.run(prefix+['-w','1','-C',chain]+actual,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
                        if check.returncode==1:break  # Rule absent or old namespace no longer exists.
                        if check.returncode:
                            errors.append('firewall check failed');break
                        deleted=subprocess.run(prefix+['-w','1','-D',chain]+actual,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
                        if deleted.returncode not in (0,1):errors.append('firewall deletion failed');break
                    except (OSError,subprocess.TimeoutExpired):errors.append('firewall cleanup timed out');break
            # An absent old namespace is expected, but host admission must be
            # positively verified open before declaring interruption finished.
            try:
                listing=run([exe,'-w','1','-S'],timeout=2)
                if self.tag.encode() in listing:errors.append('admission rule remains')
            except (RuntimeError,subprocess.TimeoutExpired):errors.append('host admission cleanup could not be verified')
        if errors:raise RuntimeError('admission cleanup incomplete; independent rescue remains active')
