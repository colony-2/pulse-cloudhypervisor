# Cloud Hypervisor provider acceptance expectations

These are proposed test files in `pulse-cloudhypervisor`, not existing tests or recorded passes. They implement the [execution and recovery design](cloudhypervisor-design.md) within the [accepted direction](provider-implementation.md#problem-description). Each expectation is at most 30 words. P0 blocks acceptance; P1 covers supported operational behavior and must also pass before release.

The human cancellation of root coordination job `M3sYVRGIvNVYwiJI2QEeT1Pm0sg` on 21 November 2026 supersedes the original two-architecture approval: qualify ARM64 with KVM only; remove AMD64 requirements. All 67 retained expectations, 22 derived case IDs, exclusions and other safety gates remain required. Publication remains deferred.

Dependency keys: **F** = deterministic fakes with observable native calls; **C** = crash/storage fault harness; **G** = guest protocol/c2j process fixture; **S** = compatible published API/SDK host fixture; **K** = qualified native KVM host with independent systemd runner authority, networking and pinned images; **J** = disposable, authorized JobDB/lease fixture; **A** = candidate build and dependency resolution. Each K test runs on native ARM64 with KVM. Negative-path fixtures use disposable resources and synthetic credentials.

## Public contract and admission

| ID | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- |
| API-01 | P0 | unit | `runtime_test.go` | A | The runtime must compile as the latest pinned API Provider, including SubmitOptions and LogSubscription. |
| API-02 | P0 | unit | `config_test.go` | F | Construction must copy mutable settings, lock its owner scope and create no VMs. |
| API-03 | P0 | unit | `config_test.go` | F | Unsafe paths, unknown configuration versions and conflicting owner locks must fail without launching compute. |
| API-04 | P0 | unit | `capabilities_test.go` | F | Discovery must preserve ordered qualified bases while busy or warming, return independent slices and perform no native mutations. |
| API-05 | P0 | unit | `admission_test.go` | F | Empty, oversized or duplicate-ID batches must fail structural validation before any dispatch. |
| API-06 | P0 | unit | `admission_test.go` | F | Invalid identities, credential-bearing endpoints, empty leases and out-of-range quantities must cause no native side effects. |
| API-07 | P0 | unit | `matching_test.go` | F | Base matching must honor exact case and API stars, preserve selectors and reject aliases or implicit version matches. |
| API-08 | P0 | unit | `matching_test.go` | F | Capability matching must use exact all-of sets and decline missing capabilities without broadening the request. |
| API-09 | P0 | unit | `admission_test.go` | F | Positive scratch requests and any StartBefore must decline before dispatch; malformed runtime caps must be rejected. |
| API-10 | P0 | unit | `capacity_test.go` | F | Concurrent admissions and builders must never exceed configured slot, CPU, memory or disk budgets. |
| API-11 | P0 | unit | `capacity_test.go` | F | Warming, busy or zero-count pools must decline without retaining a lease or queuing a later worker. |
| API-12 | P0 | unit | `admission_test.go` | F | Mixed batches must correlate explicit outcomes by LaunchID and retain valid results alongside method errors. |
| API-13 | P0 | unit | `admission_test.go` | F, C | Cancellation before dispatch must not launch later; cancellation after possible dispatch must retain uncertainty and reservation. |
| API-14 | P0 | unit | `admission_test.go` | F | Accepted results must have empty reasons; definite declines must have safe reasons and guarantee no later start. |
| API-15 | P0 | unit | `capacity_test.go` | F | Resource facts must describe the selected concrete base and guaranteed usable profile, never the selector or unverified request. |

## Ownership, recovery and listing

| ID | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- |
| REC-01 | P0 | unit | `internal/state/store_test.go` | C | A failed intent commit must prevent dispatch and leave prior durable ownership readable. |
| REC-02 | P0 | unit | `internal/state/store_test.go` | C | Crash interruption at each persistence boundary must never free capacity while an owned start can still occur. |
| REC-03 | P0 | integration | `recovery_test.go` | F, C | Lost native start acknowledgement must return unknown, retain reservation and never repeat the native start on reopen. |
| REC-04 | P0 | integration | `recovery_test.go` | F, C | Repeated active or completed fenced LaunchIDs must never start another worker, including after controller restart. |
| REC-05 | P0 | integration | `recovery_test.go` | F, C | Reopen during a live runner must recover observation without a second delivery or native launch. |
| REC-06 | P0 | unit | `internal/state/store_test.go` | C | Corrupt state, stale PIDs and identity mismatches must quarantine resources without deletion or capacity release. |
| REC-07 | P0 | unit | `list_test.go` | F | List must expose only active owned assignments, retain JobRef and LaunchID, and exclude builders, images, leases and terminal fences. |
| REC-08 | P0 | unit | `list_test.go` | F | Pagination must honor exact launch filters and reject malformed, foreign-owner or filter-mismatched tokens without mutations. |
| REC-09 | P0 | unit | `list_test.go` | F | Native observation failure must return an error rather than a successful empty inventory or proof of non-acceptance. |
| REC-10 | P0 | unit | `cleanup_test.go` | F | Cleanup must require confirmed native termination and exact ownership, preserving foreign or mismatched resources. |
| REC-11 | P0 | unit | `cleanup_test.go` | F | Guest unreachability or worker age alone must never delete compute or release an uncertain reservation. |
| REC-12 | P1 | unit | `capacity_test.go` | F | Lowered limits must drain idle capacity while preserving running jobs and unresolved reservations. |

## Guest, image and runner boundaries

| ID | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- |
| RUN-01 | P0 | integration | `internal/runner/runner_test.go` | G | The runner must forward the exact opaque lease through protected stdin and preserve JobRef without claiming replacement work. |
| RUN-02 | P0 | integration | `internal/runner/security_test.go` | G | Sentinel credentials must be absent from argv, environment, unit properties, cloud-init, state, errors, listings and automatic provider logs. |
| RUN-03 | P0 | integration | `internal/guest/agent_test.go` | G, C | Guest intent must fence c2j startup across duplicate messages, disconnects and agent restarts. |
| RUN-04 | P0 | integration | `internal/runner/runner_test.go` | G, C | Runner loss before lease delivery must not trigger disk credential recovery, duplicate delivery or automatic replay. |
| RUN-05 | P0 | unit | `internal/runner/lifetime_test.go` | F | Positive caps must start at native dispatch; zero must impose no execution runtime limit. |
| RUN-06 | P0 | unit | `internal/runner/lifetime_test.go` | F | Boot grace must not terminate an assignment whose worker is running or may already have started. |
| IMG-01 | P0 | unit | `internal/images/prepare_test.go` | F | Digest mismatch or preparation failure must prevent publication and preserve the previous usable source. |
| IMG-02 | P0 | integration | `internal/images/clone_test.go` | G | Reflink and copy paths must isolate writable disks and never publish sources containing leases or worker intent. |
| IMG-03 | P0 | unit | `internal/images/prepare_test.go` | F | Job startup must use prepared pinned tools without installation, mutable release lookup or executed-image reuse. |
| IMG-04 | P0 | unit | `internal/network/pool_test.go` | F | Concurrent reservations must allocate distinct MACs, addresses, TAPs and private control paths within the owner's configured pool. |
| IMG-05 | P1 | unit | `internal/images/prepare_test.go` | F | Changed preparation fingerprints must select new immutable sources without reinterpreting existing jobs or mutating running disks. |

## Listener lifecycle

| ID | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- |
| LOG-01 | P0 | unit | `logs_test.go` | F | A nil listener must return a nil subscription; requested listening must return a handle even when observation fails. |
| LOG-02 | P0 | unit | `logs_test.go` | F | Invalid listener context, callback or duration must terminate observation explicitly without altering native admission outcomes. |
| LOG-03 | P0 | unit | `logs_test.go` | F | Binary records must retain bytes, timestamps and source identity, remain within 4096 bytes, and sequence consecutively per launch. |
| LOG-04 | P0 | unit | `logs_test.go` | F | Coverage must disclose unavailable/interrupted sources and known closure; LastSequence must count callback dispatch rather than successful processing. |
| LOG-05 | P0 | unit | `logs_test.go` | F | Stop, context cancellation and positive duration expiry must end listening without cancelling admission or accepted execution. |
| LOG-06 | P0 | unit | `logs_test.go` | F | Zero listener duration must remain active until another terminal condition, independent of admission context expiry. |
| LOG-07 | P0 | unit | `logs_test.go` | F | Wait-context expiry must affect only that wait; repeated Wait and Coverage must return stable results with independent slices. |
| LOG-08 | P0 | unit | `logs_test.go` | F | Callbacks must remain serial; explicit stop requests, callback errors and panics must terminate observation with safe typed outcomes. |
| LOG-09 | P0 | unit | `logs_test.go` | F | Overflow or blocked callbacks must close Done promptly and bound retained queues, subscriptions and abandoned callback goroutines. |
| LOG-10 | P0 | integration | `internal/runner/output_test.go` | G | After listener loss, the runner must keep draining output so high-volume c2j writes still complete. |
| LOG-11 | P0 | integration | `plugin_test.go` | S, G | SDK transport loss must interrupt coverage without resubmission, duplicate execution or cancellation of accepted work. |
| LOG-12 | P0 | unit | `logs_test.go` | F | No-start launches must disclose their source coverage without treating source closure as workload success. |

## CLI, SDK and native acceptance

| ID | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- |
| CLI-01 | P1 | unit | `cmd/pulse-cloudhypervisor/config_test.go` | F | Shared JSON settings must honor explicit flags, resolve paths consistently and reject unknown fields or versions. |
| CLI-02 | P1 | unit | `cmd/pulse-cloudhypervisor/config_test.go` | C | Failed setup/upgrade must preserve prior settings; successful saves must be atomic/private; serving must not rewrite them. |
| CLI-03 | P0 | integration | `maintenance_test.go` | F, G | Upgrade must drain accepted work, preserve sources on preparation failure and never cancel jobs on maintenance timeout. |
| SDK-01 | P0 | integration | `plugin_test.go` | S, A | A separate latest-SDK host must exercise discovery, submission, logs and listing through local process and authenticated outbound modes. |
| SDK-02 | P0 | integration | `plugin_test.go` | S | Incompatible SDK baseline and unauthorized identity must fail before dispatch, without protocol downgrade. |
| SDK-03 | P0 | integration | `dependency_test.go` | A | An external consumer must resolve published pinned API/SDK modules and build the candidate without local dependency replacements. |
| LIVE-01 | P0 | integration | `tests/live/preflight_test.go` | K | Missing KVM, supervisor authority, host budget, networking or qualified boot assets must fail acceptance rather than skip required checks. |
| LIVE-02 | P0 | integration | `tests/live/images_test.go` | K | Preparation must verify Ubuntu identity, architecture, c2j revision, pinned tools and Docker behavior before advertising corresponding capabilities. |
| LIVE-03 | P0 | integration | `tests/live/execution_test.go` | K, J | A real supplied-lease job must execute, renew beyond its initial lease lifetime and report concrete verified allocation facts. |
| LIVE-04 | P0 | integration | `tests/live/execution_test.go` | K, J | Expired, mismatched and completed leases must fail execution without reacquisition or fallback to another job. |
| LIVE-05 | P0 | integration | `tests/live/isolation_test.go` | K, J | Concurrent jobs must have distinct identities and private writable disks; writes must not reach another job or prepared source. |
| LIVE-06 | P0 | integration | `tests/live/isolation_test.go` | K, J | Guest-local Docker and any advertised workspace capability must work inside the guest without exposing host workspace mounts. |
| LIVE-07 | P0 | integration | `tests/live/lifetime_test.go` | K, J, S | Closing or killing the provider/SDK service must leave accepted work running; reopening must recover ownership without duplicate startup. |
| LIVE-08 | P0 | integration | `tests/live/lifetime_test.go` | K, J | Positive runtime caps must terminate owned VMs after controller loss and runner failure; uncapped jobs must finish without age-based termination. |
| LIVE-09 | P0 | integration | `tests/live/recovery_test.go` | K, J, C | Rebooted-host state must never replay old leases; recovery must distinguish terminated resources from uncertain or foreign resources. |
| LIVE-10 | P0 | integration | `tests/live/cleanup_test.go` | K, J | Completion and cap expiry must remove owned job resources and release reservations while preserving foreign-resource canaries. |
| LIVE-11 | P0 | integration | `tests/live/logs_test.go` | K, J, S | Real launch/stdout/stderr observation must survive high output and listener cancellation without blocking or killing the job. |

## Assertions for the approved storage and verification revisions

These subcases make the accepted revisions executable; they preserve all 67 rows above. Parent IDs identify the retained expectations they refine. **V** denotes the repository verification-runner fixture with observable child commands/results. All subcases are mandatory alongside their parents; P1 also remains required. Files are proposed, not existing implementations.

| Case | Parent | Priority | Kind | Proposed filename | Dependencies | Expectation |
| --- | --- | --- | --- | --- | --- | --- |
| PATH-01 | API-02 | P0 | unit | `config_test.go` | F | Omitted library state paths must resolve beneath the process home's `.pulse/cloudhypervisor/owners/<owner>/` without creating VMs. |
| PATH-02 | CLI-01 | P1 | unit | `cmd/pulse-cloudhypervisor/config_test.go` | F | Setup, upgrade and server must share the library's owner-data default and default settings to `~/.pulse/cloudhypervisor/config.json`. |
| PATH-03 | CLI-01 | P1 | unit | `cmd/pulse-cloudhypervisor/config_test.go` | F | Explicit flags must override saved paths, which must override defaults, without silently relocating an explicitly configured state directory. |
| PATH-04 | CLI-01 | P1 | unit | `cmd/pulse-cloudhypervisor/config_test.go` | F | Saved relative paths must resolve against the settings file directory, independently of the invocation directory. |
| PATH-05 | API-03 | P0 | unit | `config_test.go` | F | Missing or nonabsolute home resolution must fail when a default is needed, without fallback to working or temporary directories. |
| PATH-06 | API-03, CLI-01 | P0 | unit | `config_test.go`, `cmd/pulse-cloudhypervisor/config_test.go` | F | Complete explicit absolute settings and state paths must work without HOME when no home-based default is needed. |
| PATH-07 | API-03, CLI-02 | P0 | unit | `internal/state/store_test.go` | C | Newly created provider directories and files must retain 0700 and 0600 permissions under a permissive umask. |
| PATH-08 | API-03, REC-06 | P0 | unit | `config_test.go` | F, C | Symlinked, foreign-owned or unsafe provider paths and traversal-bearing owner names must fail before dispatch, preserving unrelated `.pulse` entries. |
| PATH-09 | API-02 | P0 | unit | `config_test.go` | C | Concurrent constructors for the same owner directory must conflict; distinct validated owner defaults must remain isolated. |
| PATH-10 | REC-04, REC-10 | P0 | integration | `recovery_test.go` | F, C | Choosing a new home or directory must not scan, migrate, clean or replay records from another location. |
| PATH-11 | REC-05, RUN-01 | P0 | integration | `internal/runner/runner_test.go` | G, C | Accepted runners must retain their resolved absolute owner path despite subsequent HOME changes or provider closure. |
| PATH-12 | API-03 | P0 | unit | `config_test.go` | F | Overlong control-socket paths must fail before dispatch; an explicit safe shorter state directory must remain supported. |
| PATH-13 | RUN-02 | P0 | integration | `internal/runner/security_test.go` | G, C | Default-root files, including interrupted-write temporaries, must never contain supplied lease bytes or automatically serialized assignments. |
| LOG-09.a | LOG-09 | P0 | unit | `logs_test.go` | F | Blocked callbacks must retain process-wide budget slots across provider closure and reopening until they return, without blocking admission. |
| VERIFY-01 | LIVE-01 | P0 | unit | `internal/verify/config_test.go` | V, A | Repository execution settings must retain `ubuntu*-arm64` and `kvm` without universal CPU, memory or scratch reservations. |
| VERIFY-02 | API-01, SDK-01, LIVE-01 | P0 | integration | `internal/verify/hook_test.go` | V, A | The recognized verify hook must invoke every required check and propagate any child failure as failed verification. |
| VERIFY-03 | API-01, LIVE-01 | P0 | unit | `internal/verify/runner_test.go` | V | Complete passing results for every required assertion and check must pass verification when candidate, pins and native ARM64 execution match. |
| VERIFY-04 | API-01, LIVE-01 | P0 | unit | `internal/verify/runner_test.go` | V | Missing expectation mappings, omitted required assertions or empty suites must fail verification despite successful child exit codes. |
| VERIFY-05 | LIVE-01 | P0 | unit | `internal/verify/runner_test.go` | V | Skipped or unexecuted mandatory assertions must fail full verification, including otherwise successful native suites. |
| VERIFY-06 | LIVE-01 | P0 | integration | `internal/verify/native_test.go` | V, A | Explicit native acceptance must exit unsuccessfully for missing prerequisites and failed assertions, without treating cross-builds as native execution. |
| VERIFY-07 | LIVE-01 | P0 | unit | `internal/verify/runner_test.go` | V | Missing architecture results, wrong architectures, stale candidates or mismatched asset/tool pins must fail aggregate verification. |
| VERIFY-08 | RUN-02 | P0 | unit | `internal/verify/runner_test.go` | V | Exported verification summaries must omit sentinel leases, raw assignment payloads, configuration secrets and environment dumps. |

## Candidate verification and interpretation

The first implementation commit added `.c2/config.yaml` requesting base `ubuntu*` and capability `kvm`; the current request refines that base to `ubuntu*-arm64`. Add `.c2/recipes/hooks/build/verify.yaml` and `scripts/verify.sh` early, before substantive runtime code. The hook runs the full plan in [candidate checks](cloudhypervisor-design.md#candidate-checks-and-remaining-qualification), including these subcases. Missing implementations must fail rather than produce an empty passing suite.

Run focused checks, `go test ./...`, `go test -race ./...` and `go vet ./...`. Build both commands for `GOOS=linux GOARCH=arm64` into scratch. Test an external consumer against the exact unpublished candidate, resolving published API/SDK pins without dependency replacements. Exercise process and authenticated outbound hosting, logs, listing, identity rejection and baseline mismatch.

A live build tag may exclude K tests from the default deterministic suite; the full hook must enable them through explicit native acceptance. Every K expectation runs natively on ARM64 KVM hosts with authorized K/J fixtures and fixed pins. Missing prerequisites, skipped/unexecuted mandatory assertions and failures fail acceptance. The ARM64 result must match the exact candidate, source and pins. Faults target fixture-owned resources only; real lease renewal and native termination cannot be replaced by mocks.

Record bounded redacted pre-merge evidence: candidate, tool/dependency/asset pins, architecture/kernel, fixture identity, executed assertions and failures. Never include leases or raw assignments. Publication and deployment remain deferred; no provider release is needed for consumer checks. These are planned tests, not recorded product passes.

### Local development verification

`bash scripts/verify-local.sh` validates the 78 non-LIVE requirement IDs and runs
all shared local checks. It reports that VM execution was not tested. The full
`bash scripts/verify.sh` command and recognized hook still require all eleven
LIVE mappings, actual ARM64/KVM assertions and matching results. Local success
cannot satisfy those requirements. Both commands reject child failures, missing
local mappings, failed/skipped/unexecuted assertions and source changes.

The verifier regression subcases execute through `TestVerificationGate` in
`internal/verify/verify_test.go`; `tests/expectations.json` maps their individual
Go subtest names to existing VERIFY IDs. Fixture capacity and authentication
boundary tests are deterministic local assertions under API-10 and RUN-02. They
do not fill LIVE mappings or demonstrate a VM execution.
