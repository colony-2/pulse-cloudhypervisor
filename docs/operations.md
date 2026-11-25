# Configure isolated Cloud Hypervisor workers

## First setup

Run `pulse-cloudhypervisor setup` on the Linux ARM64 or AMD64 host that will run the workers. Setup needs Python 3, a booted systemd manager, working host KVM and outbound access to fetch the pinned inputs. Ubuntu and Debian setups install missing host packages through apt. Run as the service account and allow sudo when it asks to configure the host. Root uses the system manager. A regular user gets a lingering user manager.

Setup discovers CPU affinity, memory available to the process and free space on the state volume. It selects the native Ubuntu 24.04 base, budgets and slot count, then prints its decisions. It downloads content-pinned Cloud Hypervisor and guest inputs, installs the matching guest agent, Nix, c2j and Docker in a disposable builder, verifies them and publishes a stopped source. It also probes guest KVM where the host supports another virtualization level. The source is reused by subsequent setups with matching inputs.

On bare metal, setup creates an owner-specific bridge on an unused private IPv4 subnet, TAPs owned by the service account, forwarding and NAT. A systemd oneshot restores these at boot. On a managed worker with an allocation bridge, setup uses its advertised addresses and creates the pool's TAPs there. The output names the bridge, TAP count and state path.

## YAML and sizing

The default settings file is `~/.pulse/cloudhypervisor/config.yaml`. Owner `default` stores its private state under `~/.pulse/cloudhypervisor/owners/default/`. If that path exceeds the Unix socket limit, setup tries a shorter path under `~/.pulse/ch/`, then a user-specific path under `/var/tmp`. It prints and saves the selected path. Use `state_dir` to select a particular volume. CLI flags override the file. Relative paths in a file resolve against its own directory.

To choose your own settings, create a YAML file with just the fields you want to control. For example, this pool allows two workers and defaults each worker to one usable CPU when the job omits its CPU request.

```yaml
owner: build
count: 2
default_worker_cpu_millis: 1000
```

Run `pulse-cloudhypervisor setup --config pool.yaml` and use the same path with `server` or `upgrade`. Setup writes the effective settings to that file after successful verification. Edit it and rerun setup to change the pool. Generated settings use `provision.base` and `provision.network` to mark the sections managed by setup. To supply your own pinned base or pre-existing network slots in a generated file, set the matching switch to `false` and supply the corresponding section. Stop an existing server for this owner before maintenance so setup can acquire its lock. A fresh host needs no prior pool.

## Supervision

Each pool uses an independent systemd manager. Its authority must allow starting transient services, inspecting exact units and setting their runtime properties. Each runner is a separate service with `KillMode=control-group`, no restart and discarded service stdout/stderr. For positive caps the runner uses a notify service with a bounded preparation phase. Its readiness notification starts the configured `RuntimeMaxSec` timer at dispatch. Before starting the VMM, the runner reads the exact microsecond cap with `busctl` alongside `systemctl` and checks the unit identity and main PID. With `ExitType=cgroup` the manager continues supervising the VMM if the runner dies. The runner also keeps a monotonic deadline. Uncapped execution has no worker runtime limit.

Automatic sizing uses up to four vCPUs and 4 GiB of RAM per reference VM by default. The default disk is 3, 4 or 8 GiB according to free space. The slot count reflects how many reference VMs fit the CPU, memory and disk budgets. Set capacity in YAML or with these options on `setup`, `upgrade` or `server`.

| Setting | CLI option | Meaning |
| --- | --- | --- |
| `count`, `total_capacity` | `--linux-capacity/-l`, `--total-capacity/-c` | Both count concurrent workers. The lower count applies. Omitting the total uses the Linux count. Explicit zero drains new admissions. |
| `host_cpu_millis`, `host_memory_bytes` | `--total-cpu-millis`, `--total-memory-bytes` | Pool-wide budgets for allocated vCPUs and VM RAM, including builders and unresolved launches. Zero budget allows no launches. |
| `max_worker_cpu_millis`, `max_worker_memory_bytes` | `--max-worker-cpu-millis`, `--max-worker-memory-bytes` | Ceilings on worker-usable resources. Each is also limited by its prepared base's usable profile. |
| `default_worker_cpu_millis`, `default_worker_memory_bytes` | `--default-worker-cpu-millis`, `--default-worker-memory-bytes` | Defaults for an omitted CPU or memory request. An unset or zero sizing default uses the effective worker maximum for that resource. |

CPU and memory requests are resolved independently. A request above its per-worker maximum returns `unsupported`. Temporary pool exhaustion returns `no_capacity`. Admission retains the difference between the base's nominal and worker-usable profile as guest headroom, rounds up to whole vCPUs and 2 MiB memory increments, and reserves at least 1 GiB of VM RAM. Allocation facts describe the resulting usable size up to the configured ceilings. The pool is charged for the actual VM size. Forecast the reference builder separately when choosing pool budgets. Existing accepted launches retain their recorded size across a restart or a change in settings.

For example, with a 4-vCPU, 8-GiB reference VM qualified for 4,000 millicores and 7 GiB usable, a 1,000-millicore default and a memory-only request for 2 GiB start a 1-vCPU, 3-GiB VM. Admission also checks the disk budget and available TAPs. Each resolved network entry contains a distinct `tap` and guest `address`, plus `gateway`, contiguous IPv4 `netmask` and `dns`. The guest configures its own `eth0` from fixed boot parameters. For operator-supplied networks, provide host routes, DNS and egress to JobDB, Git and caches.

A configuration may contain both `ubuntu-24.04-arm64` and `ubuntu-24.04-amd64`. Each host prepares, advertises and runs only its native base. A foreign-architecture request returns `unsupported` before dispatch. Disk, kernel, optional initramfs and preparation script reference absolute paths and SHA-256 digests. `recipe` is the script digest. `tools` pins the actual installed executable bytes for `c2j`, `nix`, `docker` and `guest-agent`. `docker_fixture_digest` selects a fixed guest-local Docker image by `sha256:` identity; verification never pulls a mutable tag. Docker is verified during ordinary preparation. Guest KVM is detected automatically. An optional `kvm` entry in the base capability list cannot grant it. Set usable CPU and memory below measured guest capacity; preparation reserves guest OS memory and rejects stronger claims.

On ARM64, setup checks the current host’s KVM EL2 extension. On AMD64 it checks the Intel or AMD KVM module’s nested-virtualization setting. When present, it boots a disposable clone of the prepared image using nested virtualization and creates a VM, exercises a native vCPU and attaches a TAP as the job user. It repeats those checks from a transient user service. Only a successful probe adds `kvm` to discovery. Evidence is bound to the host boot, base, prepared disk, VMM digest, CPU/memory profile and GIC settings. Run setup again after a host reboot or profile change to refresh it. An older prepared agent can continue serving ordinary jobs. Update its pin and boot assets and run setup to qualify KVM. A host that can run ordinary KVM guests but cannot expose another level continues to serve ordinary and Docker work.

Only assignments requesting `kvm` launch with nested CPUs and receive the guest network descriptor, TAP privileges and user systemd environment. The agent checks those facilities before starting c2j. On ARM64, set `kvm_gic_traps: true` to apply the three macvz GICv3 kernel settings to the probe and KVM workers for comparison. The default uses the guest kernel’s own settings. [Manual candidate qualification](manual-arm64-kvm.md)

The stopped Ubuntu bootstrap disk must already contain the guest command and [guest service](../guest/pulse-cloudhypervisor-guest.service), UID/GID 1000 with `/home/worker`, systemd-resolved, `/usr/sbin/ip`, and a working boot layout with root on `/dev/vda1`. The pinned preparation script runs inside a disposable builder. It installs fixed Nix/c2j/tools and configures guest-local Docker access for UID/GID 1000. Verification runs tool and Docker checks as that unprivileged user. It measures executable hashes, Ubuntu identity, architecture and usable resources, then clears guest machine identity. A source containing worker intent is rejected. Publication happens only after the exact builder's native termination is confirmed; failure preserves earlier sources.

Every accepted assignment gets a stopped-source clone, fresh machine identity/MAC and private API/vsock endpoints. The supplied lease goes through authenticated same-user Unix handoff, the private per-VM vsock channel and c2j stdin. It is never recovered from disk, placed in argv/environment/cloud-init or retained in provider records. c2j validates and renews the lease; the provider never claims replacement work.

Keep a stable service home or explicit state path. Owner directories are private and exclusively locked. Reopening observes existing identities and reserves uncertain work without replay. A stale PID, missing endpoint, old age or changing listing does not authorize deletion. Corrupt or foreign resources block cleanup. Reconciliation requires exact native termination evidence before deleting known owned resources. Small duplicate fences are retained indefinitely; deleting owner state ends duplicate protection and is not routine cleanup. No lease archive or historical log replay exists.

For development, use an installed approved Go toolchain on PATH with `GOWORK=off GOTOOLCHAIN=local`. Run `go test ./...`, `go test -race ./...`, `go vet ./...`, `python3 scripts/verify_test.py`, and `bash scripts/external-consumer.sh`. The local verifier cross-builds the provider and guest agent for both `linux/arm64` and `linux/amd64`. The external script serves the exact working candidate through a scratch file proxy, resolves API/SDK pins publicly with a clean module cache, checks for replacements, and exercises process and certificate-authenticated outbound hosts.

Run the full verifier on separate native Ubuntu ARM64 and AMD64 workers with KVM. Acceptance requires the same candidate to pass all 78 local and ten live requirements on both architectures, with matching source and architecture-specific pins. The native suite exercises supplied leases, controller loss, guest execution, and cleanup of disposable resources. [Native verification](native-verification.md)
