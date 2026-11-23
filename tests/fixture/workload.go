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
	case "renewal":
		return workload{command: fmt.Sprintf("sleep %d && printf cloudhypervisor-fixture-ok", seconds), stdout: "cloudhypervisor-fixture-ok"}, nil
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
