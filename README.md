# Pulse Cloud Hypervisor

This Linux Go module runs supplied c2j assignments in disposable Cloud Hypervisor guests through the `pulse-provider-api` interface and baseline-3 plugin SDK. Each Linux host serves its native Ubuntu 24.04 architecture, ARM64 or AMD64. Worker count, per-worker CPU and memory, pool budgets, and sizing defaults are configurable. [Native verification](docs/native-verification.md)

On a Linux KVM host with systemd and Python 3, run the installed command as the account that will serve the pool.

```sh
pulse-cloudhypervisor setup
```

From a checkout with Go, the equivalent command is `go run ./cmd/pulse-cloudhypervisor setup`. Setup prints the host architecture, VM sizes, pool budgets, network, image source, and saved YAML path. It prepares and boots the native Ubuntu base and verifies guest-local Docker. On a fresh Ubuntu host, setup uses sudo for the host dependencies and network. For hardware qualification from a checkout, run `./scripts/qualify-l0.sh` directly. It runs setup for you. [Setup and YAML overrides](docs/operations.md) · [L0 qualification](docs/manual-arm64-kvm.md#one-command-on-l0)

```go
p, err := cloudhypervisor.New(cloudhypervisor.Config{
    Version: cloudhypervisor.ConfigVersion,
    Owner: "worker-pool",
    // Configure pinned assets, host budgets, native bases and operator TAPs.
})
if err != nil { /* handle invalid settings or conflicting ownership */ }
defer p.Close()
```

Call `Setup` explicitly to prepare sources. `Run(ctx)` drives reconciliation; `Reconcile(ctx)` performs one pass. Discovery and listing perform no provisioning. `Close` stops admission and observation while independently supervised accepted execution continues. Admission acknowledges native dispatch, rather than successful boot or job completion. Do not retry `unknown` outcomes or infer non-acceptance from a listing.

The CLI provides `setup`, `upgrade` and `server`. They share `~/.pulse/cloudhypervisor/config.yaml`. To use a different settings file, pass `--config pool.yaml` to each command. Flags override YAML values and setup fills in missing values from the host. Successful maintenance saves resolved settings atomically with mode 0600. An existing installation using the earlier default `config.json` is read when YAML is absent. The next successful setup or upgrade writes YAML for the same owner.

`server` uses SDK process hosting. For authenticated outbound hosting, supply `--outbound host:port --name ID --ca FILE --cert FILE --key FILE --server-name DNS`. The key must be an owned 0600 file. Outbound TLS verifies both endpoints and does not reconnect or resubmit automatically. The SDK factory accepts versioned YAML in `ConfigYAML` and rejects executor environment defaults.

Guest KVM is discovered per host and prepared base. Setup probes a nested clone where ARM EL2 or x86 nested KVM is enabled. Ordinary and Docker-only assignments use ordinary CPUs on both types of host. [Guest KVM operation and qualification](docs/manual-arm64-kvm.md)

[Configuration and guest inputs](docs/operations.md) describe required pins and host authority. [Retained acceptance expectations](docs/cloudhypervisor-tests.md) cover 66 expectations and 22 derived cases. `bash scripts/verify-local.sh` runs all local checks without VM inputs and explicitly reports that VM execution was not tested. `bash scripts/verify.sh` runs the full gate, including race, vet, the isolated [lease fixture tests](tests/fixture/README.md), provider and guest builds for both architectures, external consumers, native KVM/VMM host tests, native guest acceptance and candidate-bound checks.

The first-release hook runs on ARM64. For AMD64 sign-off, check out that commit on a bare-metal AMD64 Linux host, run `./scripts/qualify-l0.sh`, and retain the `pulse-l0-amd64-<random>.log` it prints. [Native verification](docs/native-verification.md)

On a physical ARM64 or AMD64 Linux host, `./scripts/qualify-l0.sh` qualifies guest KVM using the local candidate pool and real c2j jobs. [L0 qualification commands and prerequisites](docs/manual-arm64-kvm.md#one-command-on-l0)
