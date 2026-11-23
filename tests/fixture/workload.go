package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var dockerImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type workload struct {
	command        string
	stdout, stderr string
}

func selectWorkload(profile, name, image string, duration time.Duration) (workload, error) {
	if !caseName.MatchString(name) || duration < time.Second || duration > 10*time.Minute {
		return workload{}, errFixture
	}
	seconds := 1 + int64((duration+time.Second-1)/time.Second)
	switch profile {
	case "facts":
		// This profile is tied to the qualified one-vCPU Noble fixture. Read
		// the protected c2j allocation in the command process itself.
		command := `set -eu
/usr/bin/python3 - <<'PY'
import json, os, platform, re
from decimal import Decimal
a = json.loads(os.environ['C2J_EXECUTION_ALLOCATION'])
assert a['base'] == 'ubuntu-24.04-arm64' and a['platform'] == 'linux/arm64'
assert a['capabilities'] == ['docker'] and a.get('packages', []) == []
assert os.getuid() == os.getgid() == 1000 and platform.machine() == 'aarch64'
with open('/etc/os-release') as f:
    release = dict(line.rstrip().split('=', 1) for line in f if '=' in line)
assert release['ID'] == 'ubuntu' and release['VERSION_ID'].strip('"') == '24.04'
resources = a['resources']
cpu = resources['cpu']
millis = Decimal(cpu[:-1]) if cpu.endswith('m') else Decimal(cpu) * 1000
assert millis == 1000 and os.cpu_count() * 1000 >= millis
memory = re.fullmatch(r'([0-9.]+)(B|KiB?|MiB?|GiB?)?', resources['memory'])
scale = 1024 ** ('KMG'.index(memory[2][0]) + 1) if memory[2] and memory[2] != 'B' else 1
usable = Decimal(memory[1]) * scale
assert usable == 536870912 and os.sysconf('SC_PHYS_PAGES') * os.sysconf('SC_PAGE_SIZE') >= usable + 536870912
PY
`
		return workload{command: command + fmt.Sprintf("sleep %d && printf cloudhypervisor-facts-ok", seconds), stdout: "cloudhypervisor-facts-ok"}, nil
	case "renewal":
		return workload{command: fmt.Sprintf("sleep %d && printf cloudhypervisor-fixture-ok", seconds), stdout: "cloudhypervisor-fixture-ok"}, nil
	case "peer-isolation":
		peer := "isolate-a"
		if name == "isolate-a" {
			peer = "isolate-b"
		} else if name != "isolate-b" {
			return workload{}, errFixture
		}
		command := `set -eu
umask 077
cd "$HOME"
test "$(id -u):$(id -g)" = 1000:1000
mkdir .pulse-marker
printf %s "$1" > ".pulse-marker/$1"
sleep 25
test ! -e ".pulse-marker/$2"
test "$(cat ".pulse-marker/$1")" = "$1"
/usr/bin/python3 - <<'PY'
import fcntl, ipaddress, json, pathlib, struct, subprocess
facts = pathlib.Path('/proc/cmdline').read_text().split()
values = dict(v.split('=', 1) for v in facts if '=' in v)
link = json.loads(subprocess.check_output(['/usr/sbin/ip', '-j', 'addr', 'show', 'eth0']))[0]
address, = [a['local'] for a in link['addr_info'] if a['family'] == 'inet']
assert ipaddress.ip_address(address) and values['ip'].split(':')[0] == address
with open('/dev/vsock', 'rb', buffering=0) as fd:
    cid, = struct.unpack('I', fcntl.ioctl(fd, 0x7b9, bytes(4)))
print(json.dumps(dict(machine=pathlib.Path('/etc/machine-id').read_text().strip(), launch=values['pulse.launch'],
                      mac=link['address'], address=address, cid=cid)))
PY`
		return workload{command: "set -- " + name + " " + peer + "\n" + command}, nil
	case "isolation":
		command := `set -eu
umask 077
directory="$HOME/.pulse-ch-isolation"
mkdir "$directory"
printf '%s' "$1" > "$directory/$1"
cat "$directory/$1"`
		return workload{command: "set -- " + name + "\n" + command, stdout: name}, nil
	case "docker":
		if !dockerImageID.MatchString(image) {
			return workload{}, errFixture
		}
		command := "test \"$(id -u)\" = 1000 && ! grep -Eq ' - (virtiofs|9p) ' /proc/self/mountinfo && docker run --pull=never --rm --network=none --user=1000:1000 --entrypoint=/bin/sh " + image + ` -ec 'test "$(id -u):$(id -g)" = 1000:1000 && printf cloudhypervisor-docker-ok'`
		return workload{command: command, stdout: "cloudhypervisor-docker-ok"}, nil
	case "lifetime":
		return workload{command: fmt.Sprintf("sleep %d && printf cloudhypervisor-lifetime-ok", seconds+15), stdout: "cloudhypervisor-lifetime-ok"}, nil
	case "output":
		const out = "pulse-ch-fixture-stdout-record"
		const err = "pulse-ch-fixture-stderr-record"
		return workload{
			command: "set -eu; n=0; while [ \"$n\" -lt 128 ]; do printf '%s\\n' " + out + "; printf '%s\\n' " + err + " >&2; n=$((n+1)); done",
			stdout:  strings.TrimSuffix(strings.Repeat(out+"\n", 128), "\n"),
			stderr:  strings.TrimSuffix(strings.Repeat(err+"\n", 128), "\n"),
		}, nil
	default:
		return workload{}, errFixture
	}
}
