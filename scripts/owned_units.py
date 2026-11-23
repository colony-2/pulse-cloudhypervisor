"""Observe and release only this native test's independently supervised units."""
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import threading

from verification import require

BOOT_ID = Path('/proc/sys/kernel/random/boot_id')


def key(text):
    return hashlib.sha256(text.encode()).hexdigest()[:24]


def owned_spec(path, state, owner, boot):
    rel=path.relative_to(state)
    require(len(rel.parts)==3 and rel.name=='record.json')
    mode=path.lstat()
    require(stat.S_ISREG(mode.st_mode) and mode.st_uid==os.getuid() and mode.st_mode&0o777==0o600 and mode.st_nlink==1 and mode.st_size<65536)
    data=path.read_bytes()
    spec=json.loads(data)
    ident=spec['identity']
    launch=spec['launch_id']
    require(spec['version']==1 and spec['owner']==owner and spec['boot_id']==boot and re.fullmatch('[0-9a-f]{32}',ident))
    if rel.parts[0]=='jobs':
        require(re.fullmatch(r'[A-Za-z0-9_.-]{1,128}',launch) and rel.parts[1]==key(launch))
        unit='pulse-ch-'+key(str(state))+'-'+key(launch)+'.service'
    else:
        require(rel.parts[0]=='sources' and re.fullmatch(r'build-[0-9a-f]{8}',rel.parts[1]) and launch=='prepare')
        unit='pulse-ch-build-'+key(str(path.parent))+'.service'
    require(spec['unit']==unit)
    return unit,ident,str(path.parent),hashlib.sha256(data.rstrip(b'\n')).hexdigest()


class Units:
    def __init__(self, config, ledger):
        self.state=Path(config['state_dir'])
        self.owner=config['owner']
        self.boot=BOOT_ID.read_text().strip()
        self.runner=config['runner']
        self.ledger=ledger
        self.seen={}
        self.error=False
        self.stop_event=threading.Event()
        self.thread=threading.Thread(target=self.watch)

    def scan(self):
        for pattern in ('jobs/*/record.json','sources/build-*/record.json'):
            for path in self.state.glob(pattern):
                try:
                    entry=owned_spec(path,self.state,self.owner,self.boot)
                except FileNotFoundError:
                    continue
                previous=self.seen.get(entry[0])
                require(previous is None or previous==entry)
                if previous is None:
                    self.seen[entry[0]]=entry
                    with self.ledger.open('a') as out:
                        out.write(json.dumps(entry)+'\n')
                        out.flush()
                        os.fsync(out.fileno())

    def watch(self):
        try:
            while not self.stop_event.wait(.1):
                self.scan()
        except (OSError,ValueError,KeyError,TypeError):
            self.error=True

    def close(self):
        self.stop_event.set()
        self.thread.join()
        self.scan()
        require(not self.error and BOOT_ID.read_text().strip()==self.boot)
        for unit,ident,directory,sha in self.seen.values():
            self.release(unit,ident,directory,sha)
        print('PASS: independently supervised test units reconciled',flush=True)

    def show(self, unit):
        p=subprocess.run(['systemctl','--user','--no-ask-password','show',unit,
                          '--property=LoadState,Description,InvocationID,ActiveState,ControlGroup,MainPID,ExecStart'],
                         capture_output=True,text=True,timeout=4)
        fields=dict(line.split('=',1) for line in p.stdout.splitlines() if '=' in line)
        if fields.get('LoadState')=='not-found':
            return None
        require(p.returncode==0 and fields.get('LoadState')=='loaded')
        return fields

    def release(self, unit, ident, directory, sha):
        fields=self.show(unit)
        if fields is None:
            # Positive manager absence only. An absent unit with a recorded
            # live PID or nonempty unit cgroup would be an ambiguous target.
            return
        require(fields['Description']=='pulse:'+ident and BOOT_ID.read_text().strip()==self.boot)
        role='prepare-runner' if unit.startswith('pulse-ch-build-') else 'runner'
        command=self.runner+' '+role+' --dir '+directory+' --digest '+sha
        require('argv[]='+command+' ;' in fields['ExecStart'])
        group=fields['ControlGroup']
        if group:
            require(group.startswith('/') and Path(group).name==unit and '..' not in Path(group).parts)
        pid=int(fields['MainPID'])
        if pid:
            require(pid>1 and group)
            proc=Path('/proc',str(pid))
            require(proc.stat().st_uid==os.getuid() and (proc/'cmdline').read_bytes().split(b'\0')[:-1]==[s.encode() for s in command.split()])
            require((proc/'cgroup').read_text().splitlines()==['0::'+group])
            start=(proc/'stat').read_text().rsplit(')',1)[1].split()[19]
            require(start.isdigit() and int(start)>0)
        require(self.show(unit)==fields)
        subprocess.run(['systemctl','--user','--no-ask-password','stop',unit],check=True,timeout=12,
                       stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        after=self.show(unit)
        require(after is None or after['Description']=='pulse:'+ident and after['ActiveState']=='inactive' and after['MainPID']=='0')
        if group:
            events=Path('/sys/fs/cgroup'+group,'cgroup.events')
            require(not events.exists() or 'populated 0' in events.read_text().splitlines())
