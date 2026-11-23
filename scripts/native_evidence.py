"""Nonsecret native-test diagnostics, independent of the strict release receipt."""
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
from verification import MAX_EVENTS, decode, digest, read_json, require, write_json


def arm_el2():
    try:
        raw = subprocess.check_output([sys.executable, str(Path(__file__).resolve()), '--probe'],
                                      stderr=subprocess.DEVNULL, timeout=5)
        api, el2 = json.loads(raw)
        require(type(api) is int and type(el2) is int and api==12 and el2>=0)
        verdict = 'supported' if el2>0 else 'unsupported'
        return {'api':api,'arm_el2':el2,'verdict':verdict}
    except (OSError, subprocess.SubprocessError, ValueError, TypeError):
        return {'verdict':'unproven'}


class StreamAudit:
    """Audit all test names in memory. Only mapped, locally known names leave it."""
    def __init__(self, mapping):
        self.mapping = mapping
        self.count = 0
        self.damaged = False
        self.packages = {}
        self.tests = {}

    def feed(self, line):
        self.count += 1
        if self.damaged:
            return
        try:
            require(self.count <= MAX_EVENTS and len(line)<=1024*1024)
            event=decode(line)
            action, package, test = event['Action'], event['Package'], event.get('Test')
            require(isinstance(package,str) and 0<len(package)<=500)
            require(test is None or isinstance(test,str) and 0<len(test)<=500)
            state=self.packages.get(package)
            if action=='start' and test is None:
                require(state is None)
                self.packages[package]='run'
                return
            require(state=='run')
            if action=='output':
                return
            if test is None:
                require(action in ('pass','fail','skip'))
                results=[v for (p,_),v in self.tests.items() if p==package]
                require(all(v in ('pass','fail','skip') for v in results))
                require('fail' not in results or action=='fail')
                self.packages[package]=action
                return
            key=(package,test)
            prior=self.tests.get(key)
            require((action=='run' and prior is None) or (action=='pause' and prior in ('run','cont'))
                    or (action=='cont' and prior=='pause') or (action in ('pass','fail','skip') and prior in ('run','cont')))
            self.tests[key]=action
        except (KeyError, ValueError, TypeError, UnicodeError):
            self.damaged=True

    def result(self, code, timed_out=False):
        packages={p for pairs in self.mapping.values() for p,_ in pairs}
        consistent=(not self.damaged and not timed_out and code in (0,1)
                    and bool(self.packages) and packages.issubset(self.packages)
                    and 'run' not in self.packages.values() and (code==1)==('fail' in self.packages.values()))
        if not consistent:
            return {'integrity':'damaged'}
        results={}
        for ident,pairs in self.mapping.items():
            states=[self.tests.get(tuple(pair),'not_run') for pair in pairs]
            results[ident]=next((s for s in ('fail','skip','not_run') if s in states), 'pass' if states else 'not_run')
        return {'integrity':'complete','outcomes':results}


def report(path, native, binding, probe, mapping):
    if path is None:
        return
    summary={'version':1,'binding':binding,'nesting':probe,'integrity':'not_run'}
    if native.exists():
        summary['integrity']='untrusted'
        try:
            side=read_json(native.with_suffix('.audit'))
            require(side['binding']==binding and side['sha256']==digest(native))
            data=side['audit']
            if data=={'integrity':'damaged'}:
                summary['integrity']='damaged'
            else:
                require(set(data)=={'integrity','outcomes'} and data['integrity']=='complete' and set(data['outcomes'])==set(mapping))
                require(all(v in ('pass','fail','skip','not_run') for v in data['outcomes'].values()))
                summary.update(data)
        except (ValueError,KeyError,TypeError,OSError):
            pass
    write_json(path,summary)


if __name__=='__main__' and sys.argv[1:]==['--probe']:
    with open('/dev/kvm','rb',buffering=0) as kvm:
        print(json.dumps([fcntl.ioctl(kvm,0xae00,0),fcntl.ioctl(kvm,0xae03,240)]))
