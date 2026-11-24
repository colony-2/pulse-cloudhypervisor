# Pulse Cloud Hypervisor

This Linux Go module runs supplied c2j assignments in disposable Cloud Hypervisor guests through the `pulse-provider-api` interface and baseline-3 plugin SDK. The native ARM64/KVM suite boots Ubuntu 24.04 guests and runs supplied assignments through JobDB. [Native verification](docs/native-verification.md)

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

Build `./cmd/pulse-cloudhypervisor` and `./cmd/pulse-cloudhypervisor-guest` for Linux. The provider command accepts `setup`, `upgrade`, and `server`. All three use strict version-1 JSON settings through `--config`, defaulting to `~/.pulse/cloudhypervisor/config.json`. `--owner` and `--state-dir` override saved settings. Owner state defaults to `~/.pulse/cloudhypervisor/owners/<owner>/`; explicit absolute paths work without HOME. Saved relative paths resolve against the settings directory. Setup and upgrade save successful resolved settings privately and atomically. Server never saves settings.

`server` uses SDK process hosting. For authenticated outbound hosting, supply `--outbound host:port --name ID --ca FILE --cert FILE --key FILE --server-name DNS`. The key must be an owned 0600 file. Outbound TLS verifies both endpoints and does not reconnect or resubmit automatically. The SDK factory accepts strict JSON in `ConfigYAML` and rejects executor environment defaults.

[Configuration and guest inputs](docs/operations.md) describe required pins and host authority. [Retained acceptance expectations](docs/cloudhypervisor-tests.md) cover 66 expectations and 22 derived cases. `bash scripts/verify-local.sh` runs all local checks without VM inputs and explicitly reports that VM execution was not tested. `bash scripts/verify.sh` runs the full gate, including race, vet, the isolated [lease fixture tests](tests/fixture/README.md), both ARM64 command builds, external consumers, native KVM/VMM host tests, ARM64 guest acceptance and strict candidate-bound aggregation. [Native verification inputs](docs/native-verification.md) include a fixture-independent host test command and explain the guest fixture references and evidence binding.
