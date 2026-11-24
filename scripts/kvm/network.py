#!/usr/bin/env python3
"""Exercise nested networking as the job account in a disposable Cloud Hypervisor L1."""
import argparse
import contextlib
import http.server
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlsplit

KIT = Path(__file__).resolve().parent


def run(*args):
    subprocess.run(args, check=True, timeout=120)


def read_network():
    cfg = json.loads(Path(os.environ["PULSE_KVM_NETWORK"]).read_text())
    boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    if cfg["version"] != 1 or cfg["boot_id"] != boot:
        raise ValueError("network descriptor does not identify this boot")
    subnet = ipaddress.IPv4Network(cfg["subnet"])
    first = ipaddress.IPv4Address(cfg["first_guest_address"])
    last = ipaddress.IPv4Address(cfg["last_guest_address"])
    gateway = ipaddress.IPv4Address(cfg["gateway"])
    if not (gateway in subnet and first in subnet and last in subnet and first < last and
            gateway not in (first, first + 1) and first > subnet.network_address and first + 1 < subnet.broadcast_address):
        raise ValueError("need a gateway and two usable nested guest addresses")
    return cfg, subnet.prefixlen, first, gateway


def check(url, docker_image):
    if os.getuid() != 1000 or " pulse.launch=" not in " " + Path("/proc/cmdline").read_text():
        raise ValueError("run as the c2j worker inside a disposable Cloud Hypervisor assignment")
    target = urlsplit(url)
    if target.scheme != "http" or not target.hostname or target.username or target.password or any(c.isspace() for c in url):
        raise ValueError("supply a plain HTTP test URL with a DNS name and no credentials")
    try:
        ipaddress.ip_address(target.hostname)
    except ValueError:
        pass
    else:
        raise ValueError("use a DNS name so the external HTTP request exercises DNS")
    cfg, prefix, first, gateway = read_network()
    token = secrets.token_hex(12)
    unit = "pulse-kvm-test-" + token
    taps = ["pq" + token[:9] + suffix for suffix in ("a", "b")]
    wrapper = shutil.which("pulse-kvm-run")
    if not wrapper:
        raise ValueError("the worker is missing the pulse-kvm-run user-service helper")
    env = ["--setenv=" + k + "=" + os.environ[k] for k in ("PULSE_KVM_NETWORK", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS")]
    svc = ["systemd-run", "--user", "--quiet", "--collect", *env]

    class Handler(http.server.BaseHTTPRequestHandler):
        timeout = 5

        def do_GET(self):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(token.encode())

        def log_message(self, *_):
            pass

    with tempfile.TemporaryDirectory() as tmp, contextlib.ExitStack() as cleanup:
        tmp = Path(tmp)
        for i, tap in enumerate(taps):
            # The direct job and a separate user service must both have TAP authority.
            command = ["ip", "tuntap", "add", "dev", tap, "mode", "tap", "vnet_hdr"]
            if i:
                run(*svc, "--wait", "--pipe", wrapper, *command)
            else:
                run(*command)
            cleanup.callback(run, "ip", "tuntap", "del", "dev", tap, "mode", "tap")
            run("ip", "link", "set", "dev", tap, "master", cfg["bridge"], "up")
        if docker_image:
            if not re.fullmatch(r"sha256:[a-f0-9]{64}", docker_image):
                raise ValueError("supply a preloaded immutable Docker image ID")
            run(*svc, "--wait", "--pipe", wrapper, "docker", "run", "--pull=never", "--rm",
                "--network=none", "--device=/dev/kvm", "--entrypoint=/bin/sh", docker_image,
                "-ec", "test -c /dev/kvm && exec 3<>/dev/kvm")
        server = http.server.HTTPServer((str(gateway), 0), Handler)
        cleanup.callback(server.server_close)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        cleanup.callback(thread.join)
        cleanup.callback(server.shutdown)

        def boot(i, role):
            mac = "02:" + bytes.fromhex(token[:8] + f"{i:02x}").hex(":")
            serial = tmp / role
            console = {"x86_64": "ttyS0", "aarch64": "ttyAMA0"}[os.uname().machine]
            args = ["console=" + console, "loglevel=4", "panic=1", "net.ifnames=0", "role=" + role,
                    f"addr={first + i}/{prefix}", f"gw={gateway}", f"dns={ipaddress.IPv4Address(cfg['dns'][0])}",
                    f"peer=http://{first}:8081/", f"outer=http://{gateway}:{server.server_port}/",
                    "egress=" + url, "token=" + token]
            return [KIT / "cloud-hypervisor", "--kernel", KIT / "Image", "--initramfs", KIT / "network.initrd",
                    "--cpus", "boot=1", "--memory", "size=512M", "--serial", f"file={serial}", "--console", "off",
                    "--net", f"tap={taps[i]},mac={mac}", "--cmdline", " ".join(args)]

        run(*svc, "--unit=" + unit, "--property=RuntimeMaxSec=180", wrapper, *boot(0, "peer"))
        cleanup.callback(run, "systemctl", "--user", "stop", unit + ".service")
        peer = tmp / "peer"
        until = time.monotonic() + 60
        while not (peer.exists() and "PULSE_CH_PEER_READY" in peer.read_text(errors="replace").splitlines()):
            if time.monotonic() >= until:
                raise TimeoutError("nested peer did not listen under the user service")
            time.sleep(.25)
        run(*boot(1, "client"))
        lines = (tmp / "client").read_text(errors="replace").splitlines()
        if "PULSE_CH_NESTED_NETWORK_PASS" not in lines:
            raise ValueError("nested client failed:\n" + "\n".join(lines[-20:]))
    print("PASS user-service peer, dynamic TAPs, peer and worker HTTP, DNS and outbound HTTP")


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--egress-url", required=True, help="controlled external HTTP endpoint returning 200")
    p.add_argument("--docker-image", help="also verify Docker device access from the user service")
    args = p.parse_args()
    try:
        check(args.egress_url, args.docker_image)
    except (KeyError, OSError, ValueError, subprocess.SubprocessError) as exc:
        sys.exit(f"FAIL: {exc}")
