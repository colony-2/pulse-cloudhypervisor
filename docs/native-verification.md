# Verify one candidate on native ARM64 with KVM

The human cancellation of root coordination job `M3sYVRGIvNVYwiJI2QEeT1Pm0sg` on 21 November 2026 supersedes the original two-architecture approval: test ARM64 with KVM only; remove AMD64 requirements. All 67 retained expectations, 22 derived case IDs, exclusions and other safety gates remain required. Publication remains deferred.

`bash scripts/verify-local.sh` is the explicit development command. It needs
ARM64 placement and installed tools, but no KVM access, VM assets, networking,
lease service, supervisor authority or fixed large host reservation. It runs
and validates every shared local check and all 78 local mappings, then prints
`VM execution was not tested by this command` on success or failure. The local
path does not read VM fixture or pin inputs. Its temporary receipts and builds
are private and outside the checkout. Unsupported modes fail; environment flags
cannot downgrade the full wrapper or hook to local success.

The repository retains `ubuntu*-arm64`, `kvm` and the approved hypervisor package
without universal CPU, memory or scratch minima. Guest defaults and admission
budgets stay unchanged. VM preflight checks actual capacity against the selected
concurrent guest/builder workload when it runs. No special instance class or
separate certification process is required.

The provided ARM64/KVM worker can run host tests without a guest fixture. With
the installed Go toolchain on PATH, run:

```sh
GOWORK=off GOTOOLCHAIN=local go test -count=1 -tags=live -run='^TestNativeHost$' -timeout=30s -v ./tests/live
```

This test opens KVM, checks API version 12, creates and closes an empty KVM VM,
then starts the installed Cloud Hypervisor with a private Unix API socket,
checks its ping response and shuts down that exact process. It uses no guest
image, lease, network changes or systemd service. Cleanup is restricted to its
owned descriptors, process and temporary directory. Missing KVM or VMM access
fails the test. Passing it demonstrates these host operations; guest boot and
the eleven LIVE requirements still need their complete assertions and inputs.

Stage Ubuntu boot source assets without root or installing host packages:

```sh
python3 scripts/native_assets.py --output /absolute/new/private-assets
```

This downloads the fixed Ubuntu 24.04 ARM64 release from 17 October 2026 and
checks the recorded official SHA-256 values before using it. The UEC archive's
bare ext4 filesystem is wrapped in deterministic GPT partition 1 for `/dev/vda1`.
It writes a sparse raw disk, an uncompressed ARM64 Image kernel, the initramfs and `assets.json`
with measured paths/hashes. It refuses an existing destination and never extracts
archive paths, links or device nodes. These remain **unprepared source assets**:
the collection is not the verifier's complete pins, a configured fixture or proof
of boot. Add the candidate bootstrap agent/service, worker layout and fixed guest
tools/preparation before testing or advertising any capabilities. The staging
boundary regressions run as part of the shared Python verifier tests.
The runner explicitly supplies `image_type=raw` for builder and job disks;
the approved Cloud Hypervisor v55 rejects a disk argument that omits the type.

For allocations supplied by pulse-macvz, follow its
[Linux/KVM instructions](https://github.com/colony-2/pulse-macvz/blob/fcfcf56a47830f573e8045e54ae36e29892351fd/docs/linux-kvm.md).
The documented route provides a network descriptor at `PULSE_KVM_NETWORK`, a
lingering user manager with explicit runtime/bus environment and the
`pulse-kvm-run` capability launcher for independent user services. Check that
these exist on this actual allocation before using them. Published source or an
older readiness receipt does not prove rollout here. Do not run preparation on
an accepted allocation or bypass its launch-receipt guard. A failed handoff needs
owner rollout/access correction, rather than another implementation of the same
published fix. No parent Mac or unrelated allocation is in scope.

`bash scripts/verify.sh` runs deterministic Go tests, verifier failure tests, recipe hook tests, race tests, vet, both Linux ARM64 command builds and the separate published-module consumer. The shared checks also run the private nested-module lease fixture tests with race detection and vet. It then runs the native host test even when guest fixture inputs are absent, runs native guest acceptance, and invokes `check-coverage.py` to aggregate that one ARM64 result with current local and consumer evidence. Every failed command fails the hook. Missing, skipped, failed or unexecuted mandatory assertions remain failures. `verify-local.sh` does not run the host test and still needs no KVM access.

Use the installed approved Go/GCC toolchains. The full hook and native entry point also discover existing Go 1.26.8/GCC 15.3.0 executables in the Nix store when absent from PATH; they do not realize store paths. No script installs tools or enables Nix builds. Command builds used for native evidence use `CGO_ENABLED=0`, `-trimpath` and `-buildvcs=false` on the ARM64 host. The complete pin collection includes the Go version; aggregation compares each candidate-built guest agent with the corresponding candidate-built executable.

Supply these nonsecret references through the environment:

| Variable | Contents |
| --- | --- |
| `CANDIDATE` | Managed candidate identity; defaults to Git HEAD for local runs. |
| `CLOUDHYPERVISOR_PINS` | Absolute path to the complete, fixed ARM64 pin collection. |
| `CLOUDHYPERVISOR_FIXTURE` | Absolute path to this host's private operator fixture descriptor. |

For an independent native run, use `bash scripts/acceptance.sh --arch arm64 --candidate ID --result /absolute/scratch/native.json` on ARM64. AMD64 is rejected and no counterpart is requested. `--result` is optional for a check without an exported receipt. The output file must be new and outside the checkout. Missing inputs or native prerequisites fail. A successful preflight alone cannot produce passing native acceptance.

The version-1 pin collection contains `common` and `architectures` objects. Common pins contain `provider_api`, `plugin_sdk`, integer `sdk_baseline`, a full `c2j_revision` and `go_version`. The single `arm64` entry contains SHA-256 values for `hypervisor`, `disk`, `kernel`, `preparation` and `bootstrap_agent`; `initramfs` is its SHA-256 or JSON null. `tools` contains separate `c2j`, `nix` and `docker` executable hashes; `docker_image` is a fixed `sha256:` image identity. Unknown fields and incomplete collections fail. The installed candidate guest agent is measured after building and recorded separately, avoiding a self-referential source pin.

The private version-1 fixture descriptor contains `identity`, absolute `config` and `control_socket` paths, a `bootstrap_agent` asset (`path`/`sha256`) and declared `faults`. It contains no lease or raw assignment. Common native preflight checks resources for concurrent guests/builders, KVM API access, asset hashes, reserved TAPs, a protected same-user fixture socket and controller/runner fault declarations. It does not require `host-reboot`. Its disposable supervisor probe checks independent launch, inspection and runtime-property changes, with same-user execution, noninteractive commands and cleanup restricted to its exact description. It creates no persistent services.

`TestNativeRebootPreflight` separately requires the reboot declaration. Full acceptance still runs both preflights and requires LIVE-09. The declaration check does not perform a reboot or prove recovery, external observation or exact-host authority. Those must be implemented and exercised by the separate reboot assertion. To develop the other tests, select their Go test names without selecting the reboot prerequisite. Neither test selection nor a successful guest preflight establishes full acceptance.

The descriptor and protected connection are implemented locally. The
[private lease helper](../tests/fixture/README.md) now supplies isolated
valid/expired/mismatched/completed assignments and authoritative lease-event
observations. Its installed-c2j integration tests execute on the host. The native
client implements bounded authenticated health, assignment and status requests;
preflight requires the health handshake. The helper is not a deployed guest
fixture. The latest human direction authorizes preparing the missing test assets and isolated fixture inside the supplied allocation: do not wait for an external operator to deliver a complete fixture. This cell owns preparation, helper deployment, independent observations, assertions and execution. Demonstrated allocation permission failures go to pulse-macvz for a narrowly scoped capability repair; they do not authorize a new allocation, package installation or changes to unrelated resources. Reboot recovery separately needs the exact disposable Linux host and an external observer; no Mac or other allocation is in scope.

The remaining guest/native assertions are unfinished. All eleven LIVE mappings intentionally remain empty until their complete real assertions exist. Missing boot assets, prepared tools and guest networking are implementation tasks within that scope. Full verification remains failed until real execution, including reboot recovery, is demonstrated.

Receipts bind candidate, source, mapping, complete pins, selected native assets, fixture identity, built guest-agent digest and test command outcome. Source binding includes sorted tracked/nonignored paths, file types, modes and contents, symlink targets and missing tracked files. Git metadata and ignored outputs do not affect it. A dirty source change invalidates evidence even when HEAD is unchanged; source is checked before and after execution.

The bounded event collector discards arbitrary test Output fields and subprocess stderr in memory. It exports mapped test/package identifiers and recognized actions only. Invalid/truncated JSON, failures or skips latch failure; successful later events cannot erase them. Required tests need run/pass events and completed packages. Unknown result versions, duplicate JSON keys, duplicate architectures, missing mappings, changed pins and stale source all fail aggregation. Synthetic events in verifier tests exercise the verifier only.

Lease bytes must arrive in memory through the authorized fixture connection and retain the production Unix/vsock-to-c2j-stdin path. Never supply them through these variables, configuration, files, argv, systemd properties or review evidence. c2j owns validation/renewal; these tools do not acquire a replacement lease or use the active task's lease.
