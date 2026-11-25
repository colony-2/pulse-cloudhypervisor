"""Provision one native Cloud Hypervisor owner from discovered or supplied settings."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
sys.path[:0] = [str(HERE), str(HERE.parent)]
import assets
import native_assets
import network

MiB = 1 << 20
GiB = 1 << 30


def limits():
    cpus = len(os.sched_getaffinity(0))
    mem = (
        int(
            next(
                line.split()[1]
                for line in Path("/proc/meminfo").read_text().splitlines()
                if line.startswith("MemAvailable:")
            )
        )
        * 1024
    )
    # Account for a constrained service as well as the physical host.
    groups = Path("/proc/self/cgroup").read_text().splitlines()
    for entry in groups:
        if not entry.startswith("0::"):
            continue
        root = Path("/sys/fs/cgroup")
        relative = entry[3:].lstrip("/")
        paths = [root]
        if ".." not in Path(relative).parts:
            group = root / relative
            while group != root:
                paths.append(group)
                group = group.parent
        for path in paths:
            try:
                limit = (path / "memory.max").read_text().strip()
                if limit != "max":
                    mem = min(mem, max(0, int(limit) - int((path / "memory.current").read_text())))
                quota, period = (path / "cpu.max").read_text().split()
                if quota != "max":
                    cpus = min(cpus, max(1, int(quota) // int(period)))
            except FileNotFoundError:
                pass
    return cpus, mem


def size(c, present, cpu, mem, free):
    def setting(name, value):
        if name not in present and not c.get(name):
            c[name] = value
        return c[name]

    cpu_budget = setting("host_cpu_millis", cpu * 1000)
    memory_budget = setting(
        "host_memory_bytes", max(0, mem - max(512 * MiB, min(2 * GiB, mem // 8))) // (2 * MiB) * (2 * MiB)
    )
    disk_budget = setting("host_disk_bytes", max(0, free - min(2 * GiB, free // 8)))
    wanted_cpu = c.get("default_worker_cpu_millis", 0)
    wanted_ram = c.get("default_worker_memory_bytes", 0)
    vcpus = setting("vcpus", max(1, (wanted_cpu + 999) // 1000, min(4, cpu_budget // 1000)))
    reference_ram = max(4 * GiB, wanted_ram + max(768 * MiB, (wanted_ram + 2) // 3))
    reference_ram = (reference_ram + 2 * MiB - 1) // (2 * MiB) * (2 * MiB)
    ram = setting("memory_bytes", max(GiB, min(reference_ram, memory_budget) // (2 * MiB) * (2 * MiB)))
    disk = setting("disk_bytes", (8 if free >= 32 * GiB else 4 if free >= 12 * GiB else 3) * GiB)
    if c.get("count") == 0 or c.get("total_capacity") == 0:
        if c.get("count") is None:
            c["count"] = 1
        return 0
    if (
        vcpus < 1
        or vcpus > 1024
        or ram < 1 * GiB
        or disk < 1 * GiB
        or cpu_budget < vcpus * 1000
        or memory_budget < ram
        or disk_budget < disk
    ):
        raise ValueError("pool budgets cannot fit one reference VM")
    count = setting(
        "count",
        max(
            1, min(64, cpu_budget // (1000 * vcpus), memory_budget // ram, max(0, disk_budget // disk - 2))
        ),
    )
    slots = min(count, c["total_capacity"] if c.get("total_capacity") is not None else count)
    if not 1 <= slots <= 1024:
        raise ValueError("invalid active worker count")
    if count > 1024 or (c.get("total_capacity") or 0) > 1024:
        raise ValueError("worker count must be at most 1024")
    for name, wanted, maximum in [
        ("cpu_millis", wanted_cpu, 1000 * vcpus),
        ("memory_bytes", wanted_ram, ram),
    ]:
        ceiling = c.get("max_worker_" + name) or maximum
        if wanted > min(ceiling, maximum):
            raise ValueError(f"default_worker_{name} exceeds the worker maximum")
    return slots


def ensure_tools(paths):
    needed = sorted(
        {pkg for tool, packages in paths.items() if not shutil.which(tool) for pkg in packages.split()}
    )
    if needed:
        if not shutil.which("apt-get"):
            raise ValueError("missing host tools: " + ", ".join(needed))
        print("Installing host dependencies: " + " ".join(needed), flush=True)
        prefix = network.admin()
        subprocess.run([*prefix, "apt-get", "update", "-qq"], check=True)
        subprocess.run(
            [
                *prefix,
                "env",
                "DEBIAN_FRONTEND=noninteractive",
                "apt-get",
                "install",
                "-y",
                "--no-install-recommends",
                *needed,
            ],
            check=True,
        )


def prereqs(c, managed_network):
    paths = {"curl": "curl", "ip": "iproute2", "modprobe": "kmod"}
    borrowed = bool(network.allocation_network())
    if managed_network and not borrowed or not os.access("/dev/kvm", os.R_OK | os.W_OK):
        paths["setfacl"] = "acl"
    if managed_network and not borrowed:
        paths["iptables"] = "iptables"
    for name in ["systemctl", "systemd-run"]:
        if not shutil.which(name):
            raise ValueError("a booted Linux systemd host is required for supervised VMs")
    ensure_tools(paths)
    if not Path("/dev/kvm").exists():
        names = network.kvm_modules()
        print("Loading host KVM modules: " + " ".join(names), flush=True)
        for name in names:
            network.run(*network.admin(), "modprobe", name)
    if not Path("/dev/kvm").is_char_device():
        raise ValueError("host kernel or firmware did not expose /dev/kvm")
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        print(f"Granting pool user {os.getuid()} access to /dev/kvm", flush=True)
        network.run(*network.admin(), "setfacl", "-m", f"u:{os.getuid()}:rw", "/dev/kvm")
    if c["user_manager"] and not borrowed:
        # Make the transient runner outlive SSH logout on a bare-metal host.
        state = subprocess.run(
            ["loginctl", "show-user", str(os.getuid()), "--property=Linger", "--value"],
            capture_output=True,
            text=True,
        )
        if state.stdout.strip() != "yes":
            print(f"Enabling the persistent systemd user manager for UID {os.getuid()}", flush=True)
            network.run(*network.admin(), "loginctl", "enable-linger", str(os.getuid()))
    network.run(
        c["systemctl"],
        *(["--user"] if c["user_manager"] else []),
        "show",
        "--property=Version",
        "--value",
        stdout=subprocess.DEVNULL,
    )


def provision(request):
    c = request["Config"]
    present = set(request["Present"])
    arch = native_assets.NATIVE_ARCH
    if not arch:
        raise ValueError("Linux ARM64 or AMD64 required")
    state = Path(c["state_dir"])
    root = state / "host"
    root.mkdir(mode=0o700, exist_ok=True)
    # Private state belongs to one provider. Refuse substitutions inside it.
    if root.is_symlink() or root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077:
        raise ValueError("unsafe host asset directory")
    cpu, mem = limits()
    free = shutil.disk_usage(state).free
    print(
        f"Detected linux/{arch}, {cpu} available CPU(s), {mem // MiB} MiB available memory, "
        f"{free // MiB} MiB free on {state}",
        flush=True,
    )
    slots = size(c, present, cpu, mem, free)
    manage = c.get("provision") or {"base": not c.get("bases"), "network": not c.get("network")}
    c["provision"] = manage
    c["systemctl"] = c.get("systemctl") or shutil.which("systemctl")
    c["systemd_run"] = c.get("systemd_run") or shutil.which("systemd-run")
    if "user_manager" not in present:
        c["user_manager"] = os.getuid() != 0
    print(
        f"Pool {c['owner']} on linux/{arch}: {slots} slot(s), {c['vcpus']} vCPU and {c['memory_bytes'] // MiB} MiB per reference VM; disk {c['disk_bytes'] // MiB} MiB. State {state}",
        flush=True,
    )
    print(
        f"Host budget: {c['host_cpu_millis']} millicores, {c['host_memory_bytes'] // MiB} MiB memory, {c['host_disk_bytes'] // MiB} MiB disk",
        flush=True,
    )
    if not slots:
        print("Pool is drained. Skipping network and image preparation.", flush=True)
        return c
    if manage["base"] and (c["disk_bytes"] < 3 * GiB or c["disk_bytes"] % 512):
        raise ValueError("automatic Ubuntu sources require a sector-aligned disk of at least 3 GiB")
    if manage["base"] and c.get("default_worker_memory_bytes", 0) > usable_memory(c):
        raise ValueError("default_worker_memory_bytes exceeds the usable reference VM memory")
    prereqs(c, manage["network"])
    exe = Path(request["Executable"])
    sha = native_assets.digest(exe)
    installed = root / ("provider-" + sha)
    if not c.get("runner") or (
        Path(c["runner"]).parent == root and Path(c["runner"]).name.startswith("provider-")
    ):
        if not installed.exists():
            with tempfile.NamedTemporaryFile(dir=root) as tmp:
                shutil.copyfile(exe, tmp.name)
                os.chmod(tmp.name, 0o700)
                os.fsync(tmp.fileno())
                os.link(tmp.name, installed)
        if native_assets.digest(installed) != sha:
            raise ValueError("installed runner digest mismatch")
        c["runner"] = str(installed)
    if manage["network"]:
        c["network"] = network.configure(state, slots)
    if not c.get("network"):
        raise ValueError("no network slots configured")
    cache = root / "downloads"
    cache.mkdir(mode=0o700, exist_ok=True)
    if manage["base"]:
        key = assets.source_key(ROOT, arch, c["disk_bytes"])
        final = root / ("ubuntu-" + key)
        manifest = final / "assets.json"
        if not manifest.exists():
            print(f"Building Ubuntu 24.04 {arch} assets in {final}", flush=True)
            if free < c["disk_bytes"] + 2 * GiB:
                raise ValueError(
                    f"need at least {c['disk_bytes'] // MiB + 2048} MiB free for the disk and setup inputs"
                )
            temp = Path(tempfile.mkdtemp(dir=root, prefix=".building-"))
            try:
                built = assets.build(ROOT, cache, temp, final, arch, c["disk_bytes"])
                (temp / "assets.json").write_text(json.dumps(built))
                os.rename(temp, final)
            finally:
                if temp.exists():
                    shutil.rmtree(temp)
        else:
            print(f"Reusing Ubuntu 24.04 {arch} assets in {final}", flush=True)
        inputs = json.loads(manifest.read_text())
        base = inputs["base"]
        base["usable_cpu_millis"] = 1000 * c["vcpus"]
        base["usable_memory_bytes"] = usable_memory(c)
        c["bases"] = [base]
        if not c.get("hypervisor", {}).get("path") or Path(c["hypervisor"]["path"]).parent.parent == root:
            c["hypervisor"] = inputs["hypervisor"]
    elif not c.get("hypervisor", {}).get("path"):
        vmm = assets.download(
            cache,
            "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v55.0/cloud-hypervisor-static"
            + ("-aarch64" if arch == "arm64" else ""),
            assets.PINS[arch]["vmm"],
        )
        vmm.chmod(0o700)
        c["hypervisor"] = {"path": str(vmm), "sha256": assets.PINS[arch]["vmm"]}
    print("Using runner " + c["runner"] + " and VMM " + c["hypervisor"]["path"], flush=True)
    return c


def usable_memory(c):
    return max(128 * MiB, c["memory_bytes"] - max(768 * MiB, c["memory_bytes"] // 4))


if __name__ == "__main__":
    try:
        os.umask(0o077)
        # Common system administration tools are outside a login user's PATH.
        os.environ["PATH"] = os.environ.get("PATH", "") + ":/usr/sbin:/sbin"
        if sys.argv[1] == "--qualification-tools":
            work = Path(sys.argv[2])
            packages = {"curl": "curl"}
            if sys.argv[3] == "supported":
                packages["cc"] = "gcc libc6-dev"
            ensure_tools(packages)
            go = assets.toolchain(native_assets.NATIVE_ARCH, work, work)
            (work / "go-path").write_text(str(Path(go).parent))
        else:
            result = provision(json.loads(Path(sys.argv[1]).read_text()))
            Path(sys.argv[2]).write_text(json.dumps(result))
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as e:
        sys.exit(f"Setup failed: {e}")
