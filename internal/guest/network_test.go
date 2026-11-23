package guest

import (
	"errors"
	"strings"
	"testing"
)

func TestGuestNetworkIsExplicitAndDoesNotTouchHost(t *testing.T) {
	commands := []string{}
	run := func(args []string) error { commands = append(commands, strings.Join(args, " ")); return nil }
	if configureNetwork("root=/dev/vda1 ip=192.0.2.2::192.0.2.1:255.255.255.0:worker:eth0:off:192.0.2.1", run) != nil {
		t.Fatal("network settings")
	}
	if len(commands) != 5 || commands[1] != "/usr/sbin/ip address replace 192.0.2.2/24 dev eth0" {
		t.Fatal("network arguments changed")
	}
	for _, line := range []string{"", "ip=bad", "ip=192.0.2.2::192.0.2.1:255.0.255.0:worker:eth0:off:192.0.2.1", "ip=192.0.2.2::192.0.2.1:255.255.255.0:worker:eth9:off:192.0.2.1"} {
		commands = nil
		if configureNetwork(line, run) == nil || len(commands) != 0 {
			t.Fatal("invalid guest networking had side effects")
		}
	}
	if configureNetwork("ip=192.0.2.2::192.0.2.1:255.255.255.0:worker:eth0:off:192.0.2.1", func([]string) error { return errors.New("fixture failure") }) == nil {
		t.Fatal("network failure ignored")
	}
}
