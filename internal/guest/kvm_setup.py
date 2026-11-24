"""Configure nested guest networking once inside a fresh Cloud Hypervisor VM."""
import ipaddress
import json
import os
from pathlib import Path
import pwd
import shutil
import stat
import subprocess

BRIDGE = "pulsebr0"
DESCRIPTOR = Path("/run/pulse-kvm/network.json")


def run(*args):
    subprocess.run(args, check=True, timeout=20)


def subnet_for(routes, addresses):
    used = [ipaddress.IPv4Network(r["dst"], strict=False) for r in routes if r.get("dst", "default") != "default"]
    for link in addresses:
        used.extend(ipaddress.IPv4Interface(f"{a['local']}/{a['prefixlen']}").network
                    for a in link["addr_info"] if a["family"] == "inet")
    for octet in range(200, 255):
        candidate = ipaddress.IPv4Network(f"10.{octet}.0.0/16")
        if not any(candidate.overlaps(network) for network in used):
            return candidate
    raise RuntimeError("no free nested subnet")


def configure():
    if " pulse.nested_kvm=1 " not in " " + Path("/proc/cmdline").read_text().strip() + " ":
        raise RuntimeError("expected a nested-enabled worker")
    if not stat.S_ISCHR(os.stat("/dev/kvm").st_mode):
        raise RuntimeError("guest KVM device unavailable")
    if not Path("/dev/net/tun").exists():
        run("modprobe", "tun")
    os.umask(0o022)
    # Go starts c2j with exactly UID/GID 1000. This grant also applies to its
    # user manager and lasts only for this disposable guest boot.
    os.chown("/dev/kvm", 0, 1000)
    os.chmod("/dev/kvm", 0o660)
    routes = json.loads(subprocess.check_output(["ip", "-j", "-4", "route", "show", "table", "all"]))
    addresses = json.loads(subprocess.check_output(["ip", "-j", "-4", "addr", "show"]))
    subnet = subnet_for(routes, addresses)
    gateway = str(subnet.network_address + 1)
    run("ip", "link", "add", BRIDGE, "type", "bridge")
    run("ip", "link", "set", BRIDGE, "alias", "pulse-cloudhypervisor:kvm:v1")
    run("ip", "address", "add", gateway + "/" + str(subnet.prefixlen), "dev", BRIDGE)
    run("ip", "link", "set", BRIDGE, "up")
    Path("/proc/sys/net/ipv4/ip_forward").write_text("1\n")
    run("iptables", "-w", "5", "-I", "FORWARD", "-i", BRIDGE, "-o", "eth0", "-j", "ACCEPT")
    run("iptables", "-w", "5", "-I", "FORWARD", "-i", "eth0", "-o", BRIDGE,
        "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
    run("iptables", "-w", "5", "-I", "FORWARD", "-i", BRIDGE, "-o", BRIDGE, "-j", "ACCEPT")
    run("iptables", "-w", "5", "-t", "nat", "-I", "POSTROUTING", "-s", str(subnet), "-o", "eth0", "-j", "MASQUERADE")
    dropin = Path("/run/systemd/resolved.conf.d/90-pulse-kvm.conf")
    dropin.parent.mkdir(parents=True, exist_ok=True)
    dropin.write_text("[Resolve]\nDNSStubListenerExtra=" + gateway + "\n")
    run("systemctl", "restart", "systemd-resolved.service")
    user = pwd.getpwuid(1000).pw_name
    run("loginctl", "enable-linger", user)
    run("systemctl", "start", "user@1000.service")
    # A service started by the user manager needs to acquire NET_ADMIN again.
    # Restrict the file-capability executable to this worker's primary group.
    helper = "/usr/local/libexec/pulse-kvm-setpriv"
    run("install", "-D", "-m", "0750", "-o", "0", "-g", "1000", shutil.which("setpriv"), helper)
    run("setcap", "cap_net_admin=ep", helper)
    wrapper = Path("/usr/local/bin/pulse-kvm-run")
    wrapper.write_text('#!/bin/sh\nexec ' + helper + ' --inh-caps=+net_admin --ambient-caps=+net_admin -- "$@"\n')
    wrapper.chmod(0o755)
    DESCRIPTOR.parent.mkdir(mode=0o755)
    DESCRIPTOR.write_text(json.dumps(dict(version=1, boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                                          bridge=BRIDGE, subnet=str(subnet), prefix_length=subnet.prefixlen,
                                          netmask=str(subnet.netmask), gateway=gateway, dns=[gateway],
                                          first_guest_address=str(subnet.network_address + 2),
                                          last_guest_address=str(subnet.broadcast_address - 1))) + "\n")


if __name__ == "__main__":
    configure()
