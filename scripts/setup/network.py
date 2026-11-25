"""Host network for an explicitly provisioned pool. Also used by its boot unit."""

import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile


def run(*args, **kw):
    return subprocess.run(list(map(str, args)), check=True, timeout=60, **kw)


def ips(*args):
    return json.loads(run("ip", "-j", *args, stdout=subprocess.PIPE).stdout)


def choose_subnet(routes, links, key, count, bridge):
    used = [
        ipaddress.ip_network(r["dst"], strict=False)
        for r in routes
        if r.get("dst", "default") != "default" and r.get("dev") != bridge
    ]
    used += [
        ipaddress.ip_interface(f"{a['local']}/{a['prefixlen']}").network
        for l in links
        if l["ifname"] != bridge
        for a in l.get("addr_info", [])
        if a.get("family") == "inet"
    ]
    prefix = min(24, 32 - (count + 2).bit_length())
    space = ipaddress.IPv4Network("10.64.0.0/10")
    n = 1 << (prefix - space.prefixlen)
    for i in range(n):
        addr = int(space.network_address) + ((int(key, 16) + i) % n) * (1 << (32 - prefix))
        subnet = ipaddress.IPv4Network((addr, prefix))
        if not any(subnet.overlaps(x) for x in used):
            return subnet
    raise ValueError("no available private subnet; supply a network in a YAML config")


def upstream_dns():
    for path in ("/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"):
        try:
            lines = Path(path).read_text().splitlines()
        except FileNotFoundError:
            continue
        for line in lines:
            parts = line.split()
            if len(parts) >= 2 and parts[0] == "nameserver":
                try:
                    ip = ipaddress.IPv4Address(parts[1])
                except ValueError:
                    continue
                if not ip.is_loopback and not ip.is_unspecified:
                    return str(ip)
    return "1.1.1.1"


def link(name):
    try:
        socket.if_nametoindex(name)
    except OSError:
        return None
    return Path("/sys/class/net", name)


def owned_link(name, kind, uid, tag):
    path = link(name)
    if path:
        if (path / "ifalias").read_text().strip() != tag:
            raise ValueError(f"interface {name} already belongs to another configuration")
        if kind == "bridge" and not (path / "bridge").is_dir():
            raise ValueError(f"{name} is not the managed bridge")
        if kind == "tap" and (
            not (path / "tun_flags").exists() or (path / "owner").read_text().strip() != str(uid)
        ):
            raise ValueError(f"{name} is not the managed TAP")
        return
    if kind == "bridge":
        run("ip", "link", "add", "name", name, "type", "bridge")
    else:
        run("ip", "tuntap", "add", "dev", name, "mode", "tap", "vnet_hdr", "user", uid)
    try:
        run("ip", "link", "set", "dev", name, "alias", tag)
    except BaseException:
        run("ip", "link", "del", "dev", name)
        raise


def ensure_taps(bridge, key, uid, count):
    for i in range(count):
        tap = "pt" + key + format(i, "03x")
        owned_link(tap, "tap", uid, "pulse-cloudhypervisor:" + key)
        run("ip", "link", "set", "dev", tap, "master", bridge, "up")


def kvm_modules():
    modules = ["kvm"]
    flags = Path("/proc/cpuinfo").read_text().split()
    if "vmx" in flags:
        modules.append("kvm_intel")
    elif "svm" in flags:
        modules.append("kvm_amd")
    return modules


def rule(table, chain, args):
    base = ["iptables", "-w", "5", "-t", table]
    check = subprocess.run(base + ["-C", chain] + args, capture_output=True, timeout=10)
    if check.returncode == 1:
        run(*base, "-I", chain, "1", *args)
    else:
        check.check_returncode()


def apply(key, subnet, uid, count):
    # The installed boot service uses only system tools and validated literals.
    os.environ["PATH"] = "/usr/sbin:/usr/bin:/sbin:/bin"
    if not re.fullmatch("[0-9a-f]{8}", key) or not 0 <= uid < 2**32 or not 1 <= count <= 1024:
        raise ValueError("invalid managed network parameters")
    net = ipaddress.IPv4Network(subnet)
    if not net.is_private or net.num_addresses < count + 3:
        raise ValueError("invalid pool subnet")
    bridge = "pb" + key
    tag = "pulse-cloudhypervisor:" + key
    for route in ips("-4", "route", "show", "table", "all"):
        if (
            route.get("dst", "default") != "default"
            and route.get("dev") != bridge
            and net.overlaps(ipaddress.ip_network(route["dst"], strict=False))
        ):
            raise ValueError(f"{subnet} overlaps an existing route")
    if not Path("/dev/kvm").exists():
        for module in kvm_modules():
            run("modprobe", module)
    if not Path("/dev/net/tun").exists():
        run("modprobe", "tun")
    if uid and shutil.which("setfacl"):
        run("setfacl", "-m", f"u:{uid}:rw", "/dev/kvm")
    owned_link(bridge, "bridge", uid, tag)
    gateway = f"{net.network_address + 1}/{net.prefixlen}"
    current = ips("-4", "addr", "show", "dev", bridge)[0]["addr_info"]
    if any(f"{a['local']}/{a['prefixlen']}" != gateway for a in current):
        raise ValueError(f"{bridge} already has a different address")
    if not any(f"{a['local']}/{a['prefixlen']}" == gateway for a in current):
        run("ip", "addr", "add", gateway, "dev", bridge)
    run("ip", "link", "set", "dev", bridge, "up")
    ensure_taps(bridge, key, uid, count)
    forwarding = Path("/proc/sys/net/ipv4/ip_forward")
    if forwarding.read_text().strip() != "1":
        forwarding.write_text("1\n")
    comment = ["-m", "comment", "--comment", tag]
    rule("nat", "POSTROUTING", ["-s", subnet, "!", "-o", bridge, *comment, "-j", "MASQUERADE"])
    rule("filter", "FORWARD", ["-i", bridge, "-s", subnet, *comment, "-j", "ACCEPT"])
    rule(
        "filter",
        "FORWARD",
        [
            "-o",
            bridge,
            "-d",
            subnet,
            "-m",
            "conntrack",
            "--ctstate",
            "RELATED,ESTABLISHED",
            *comment,
            "-j",
            "ACCEPT",
        ],
    )


def admin():
    return [] if os.geteuid() == 0 else ["sudo", *([] if sys.stdin.isatty() else ["-n"]), "--"]


def persistent(key, subnet, count):
    name = "pulse-ch-network-" + key
    helper = "/usr/local/libexec/" + name + ".py"
    unit_path = "/etc/systemd/system/" + name + ".service"
    python = "/usr/bin/python3"
    command = f"{python} -I {helper} {key} {subnet} {os.getuid()} {count}"
    unit = f"""# Managed by pulse-cloudhypervisor setup
[Unit]
Description=Pulse Cloud Hypervisor network {key}
After=network-online.target
Wants=network-online.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart={command}
[Install]
WantedBy=multi-user.target
"""
    print(
        f"Ensuring boot service {name}, bridge pb{key}, {count} TAP(s), IPv4 forwarding and NAT for {subnet}",
        flush=True,
    )
    prefix = admin()
    # Install atomically. A boot-time root service never executes owner-writable files.
    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / "unit"
        source.write_text(unit)
        changed = False
        for src, dst in [(Path(__file__), helper), (source, unit_path)]:
            path = Path(dst)
            if path.is_symlink():
                raise ValueError(f"unexpected symlink at {dst}")
            if path.exists():
                old = path.read_bytes()
                if old == src.read_bytes():
                    continue
                if (
                    dst == unit_path
                    and not old.startswith(b"# Managed by pulse-cloudhypervisor setup")
                    or dst == helper
                    and not old.startswith(b'"""Host network for an explicitly provisioned pool.')
                ):
                    raise ValueError(f"foreign file at {dst}")
            if not path.parent.exists():
                run(*prefix, "install", "-d", "-m", "0755", str(path.parent))
            staged = dst + ".new-" + os.urandom(6).hex()
            run(*prefix, "install", "-o", "root", "-g", "root", "-m", "0644", src, staged)
            run(*prefix, "mv", "-T", staged, dst)
            changed = True
        if changed:
            run(*prefix, "systemctl", "daemon-reload")
        run(*prefix, "systemctl", "enable", name + ".service")
        run(*prefix, "systemctl", "restart", name + ".service")


def allocation_network():
    supplied = os.environ.get("PULSE_KVM_NETWORK")
    fallback = Path("/etc/pulse-macvz/network.json")
    return supplied or (str(fallback) if fallback.is_file() else None)


def configure(state, count):
    key = hashlib.sha256(os.fsencode(state)).hexdigest()[:8]
    bridge = "pb" + key
    descriptor = allocation_network()
    if descriptor:
        cfg = json.loads(Path(descriptor).read_text())
        bridge = cfg["bridge"]
        net = ipaddress.IPv4Network(cfg["subnet"])
        gw = ipaddress.IPv4Address(cfg["gateway"])
        first = ipaddress.IPv4Address(cfg["first_guest_address"])
        last = ipaddress.IPv4Address(cfg["last_guest_address"])
        if (
            cfg.get("version") != 1
            or cfg["boot_id"] != Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            or bridge != "pulsebr0"
            or (Path("/sys/class/net") / bridge / "ifalias").read_text().strip()
            not in ("pulse-macvz:kvm:v1", "pulse-cloudhypervisor:kvm:v1")
            or gw not in net
            or first not in net
            or last not in net
            or first + count - 1 > last
            or first <= gw <= first + count - 1
        ):
            raise ValueError("allocation network cannot provide the requested slots")
        dns = str(ipaddress.IPv4Address(cfg["dns"][0]))
        print(f"Using allocation bridge {bridge} and {count} owned TAP(s)", flush=True)
        ensure_taps(bridge, key, os.getuid(), count)
    else:
        routes = ips("-4", "route", "show", "table", "all")
        addresses = ips("-4", "addr", "show")
        net = None
        for iface in addresses:
            if iface["ifname"] == bridge:
                for a in iface["addr_info"]:
                    if a.get("family") == "inet":
                        net = ipaddress.IPv4Interface(f"{a['local']}/{a['prefixlen']}").network
        if net is None:
            net = choose_subnet(routes, addresses, key, count, bridge)
        elif net.num_addresses < count + 3:
            raise ValueError(f"existing pool subnet {net} cannot fit {count} TAPs")
        gw, first, dns = net.network_address + 1, net.network_address + 2, upstream_dns()
        persistent(key, str(net), count)
    return [
        dict(
            tap="pt" + key + format(i, "03x"),
            address=str(first + i),
            gateway=str(gw),
            netmask=str(net.netmask),
            dns=dns,
        )
        for i in range(count)
    ]


if __name__ == "__main__":
    apply(sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]))
