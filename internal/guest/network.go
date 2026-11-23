package guest

import (
	"context"
	"net"
	"os/exec"
	"strings"
)

// Network configures only the disposable guest's sole virtio interface. Host
// TAPs, bridges, routes, firewall and egress remain operator-provided.
func Network(ctx context.Context, cmdline string) error {
	return configureNetwork(cmdline, func(args []string) error { return exec.CommandContext(ctx, args[0], args[1:]...).Run() })
}
func configureNetwork(cmdline string, run func([]string) error) error {
	var encoded string
	for _, word := range strings.Fields(cmdline) {
		if strings.HasPrefix(word, "ip=") {
			if encoded != "" {
				return ErrAgent
			}
			encoded = strings.TrimPrefix(word, "ip=")
		}
	}
	parts := strings.Split(encoded, ":")
	if len(parts) != 8 || parts[5] != "eth0" || parts[6] != "off" {
		return ErrAgent
	}
	address, gateway, mask, dns := net.ParseIP(parts[0]).To4(), net.ParseIP(parts[2]).To4(), net.ParseIP(parts[3]).To4(), net.ParseIP(parts[7]).To4()
	if address == nil || gateway == nil || mask == nil || dns == nil {
		return ErrAgent
	}
	prefix, bits := net.IPMask(mask).Size()
	if bits != 32 {
		return ErrAgent
	}
	cidr := (&net.IPNet{IP: address, Mask: net.CIDRMask(prefix, 32)}).String()
	for _, args := range [][]string{{"/usr/sbin/ip", "link", "set", "dev", "eth0", "up"}, {"/usr/sbin/ip", "address", "replace", cidr, "dev", "eth0"}, {"/usr/sbin/ip", "route", "replace", "default", "via", gateway.String(), "dev", "eth0"}, {"/usr/bin/resolvectl", "dns", "eth0", dns.String()}, {"/usr/bin/resolvectl", "domain", "eth0", "~."}} {
		if run(args) != nil {
			return ErrAgent
		}
	}
	return nil
}
