package live

import (
	"path"
	"strings"
	"testing"
)

func unifiedGroup(data []byte) string {
	var group string
	for _, line := range strings.Split(string(data), "\n") {
		current, found := strings.CutPrefix(line, "0::")
		if !found {
			continue
		}
		if group != "" || !path.IsAbs(current) || current == "/" || path.Clean(current) != current || strings.ContainsAny(current, "\r\x00") {
			return ""
		}
		group = current
	}
	return group
}

func independentSupervisor(caller, service []byte, reported string) bool {
	callerGroup, serviceGroup := unifiedGroup(caller), unifiedGroup(service)
	return callerGroup != "" && serviceGroup != "" && serviceGroup == reported &&
		callerGroup != serviceGroup && !strings.HasPrefix(serviceGroup, callerGroup+"/") && !strings.HasPrefix(callerGroup, serviceGroup+"/")
}

func TestIndependentSupervisionEvidence(t *testing.T) {
	const caller = "0::/system.slice/provider.service\n"
	const service = "0::/user.slice/probe.service\n"
	const reported = "/user.slice/probe.service"
	tests := []struct {
		name, caller, service, reported string
		want                            bool
	}{
		{"separate", caller, service, reported, true},
		{"component boundary", caller, "0::/system.slice/provider.service-extra\n", "/system.slice/provider.service-extra", true},
		{"missing caller", "1:memory:/system.slice/provider.service\n", service, reported, false},
		{"caller is root", "0::/\n", service, reported, false},
		{"missing service", caller, "", reported, false},
		{"service is root", caller, "0::/\n", "/", false},
		{"service identity mismatch", caller, service, "/user.slice/other.service", false},
		{"same group", caller, caller, "/system.slice/provider.service", false},
		{"service under caller", caller, "0::/system.slice/provider.service/probe\n", "/system.slice/provider.service/probe", false},
		{"caller under service", "0::/user.slice/probe.service/provider\n", service, reported, false},
		{"duplicate caller", caller + caller, service, reported, false},
		{"unclean path", "0::/system.slice/../caller\n", service, reported, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := independentSupervisor([]byte(test.caller), []byte(test.service), test.reported); got != test.want {
				t.Fatalf("independent supervision = %v, want %v", got, test.want)
			}
		})
	}
}
