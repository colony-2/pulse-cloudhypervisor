#!/usr/bin/env python3
"""Distribute the exact working candidate in scratch without Go replacements."""
import datetime, hashlib, json, pathlib, subprocess, sys, zipfile
root=pathlib.Path(__file__).resolve().parents[1]
module='github.com/colony-2/pulse-cloudhypervisor';version='v0.0.0-candidate'
proxy=pathlib.Path(sys.argv[1])/module/'@v';proxy.mkdir(parents=True)
(proxy/(version+'.mod')).write_bytes((root/'go.mod').read_bytes())
(proxy/(version+'.info')).write_text(json.dumps({'Version':version,'Time':'2026-11-21T00:00:00Z'}))
(proxy/'list').write_text(version+'\n')
files=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z'],cwd=root).split(b'\0')
# Go module archives exclude complete nested modules, including private tests.
nested=tuple(raw[:-len(b'go.mod')].decode() for raw in files if raw.endswith(b'/go.mod'))
with zipfile.ZipFile(proxy/(version+'.zip'),'w',zipfile.ZIP_DEFLATED) as z:
    for raw in sorted(set(files)):
        if not raw:continue
        name=raw.decode();path=root/name
        if not path.is_file() or name.startswith(('.git/',)+nested):continue
        z.writestr(module+'@'+version+'/'+name,path.read_bytes())
