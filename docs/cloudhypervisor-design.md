# Disposable Cloud Hypervisor workers: execution and recovery design

The human cancellation of root coordination job `M3sYVRGIvNVYwiJI2QEeT1Pm0sg` on 21 November 2026 supersedes the original two-architecture approval: qualify ARM64 with KVM only; remove AMD64 requirements. All 67 retained expectations, 22 derived case IDs, exclusions and other safety gates remain required. Publication remains deferred.

I will implement the [accepted feature](provider-implementation.md) with one independently supervised runner per assignment, preserving its execution/recovery boundaries and exclusions. Root coordinates; this cell owns implementation. The approved [Ubuntu/KVM first commit](source-checks.md#ubuntu-kvm-worker-request-and-early-verification), [storage default](source-checks.md#provider-data-location-revision) and publication deferral remain in force. [Test statements](cloudhypervisor-tests.md) retain all 67 expectations and add concrete subcases without changing scope.

## Public surface and configuration

Expose `New(Config)`, `Run`, `Reconcile`, `Close` and the exact current `api.Provider`. Construction copies/validates settings and acquires the owner lock without VMs. `Run` drives explicit maintenance; embedders own retry policy. `Close` stops new work, observation and maintenance while accepted execution continues. Discovery and listing never reconcile or provision.

Typed settings cover owner/state scope, hypervisor, systemd, networking, host budgets, bases and slot profiles. Default native capacity is one 4-vCPU/8-GiB slot; explicit zero drains it. Counts are ceilings, not proof of resources. Reject unsafe paths, conflicting locks and unsupported versions.

Each concrete base binds architecture, disk/boot digests, preparation recipe and verified capabilities. Start with native `ubuntu-24.04-arm64` on KVM; reject unsupported base architectures. Fingerprints include fixed Nix/c2j/tool inputs, preventing changed preparation from reinterpreting existing jobs. Mutable main/latest is not a production asset pin.

The commands share versioned JSON settings and reject unknown fields/versions/options. Setup prepares missing capacity; upgrade drains work before replacing sources. Neither updates host executables. Certificate/key settings reference protected files. Path precedence and atomic saves follow below.

## Provider data defaults

Default provider data lives beneath `~/.pulse/cloudhypervisor/`. Library normalization and command loaders share one resolver; the library does not implicitly load a settings file. `*` describes descendants, not a literal glob.

| Data | Default | Explicit override |
| --- | --- | --- |
| Shared versioned settings | `<home>/.pulse/cloudhypervisor/config.json` | Command config-file path |
| Owner records, locks, fences, sources, disks and private control paths | `<home>/.pulse/cloudhypervisor/owners/<owner>/` | `Config.StateDir`, saved setting or command state-directory flag |

Resolve `os.UserHomeDir()` once only when a default is needed; require an absolute usable home and validate owner names. Give runners absolute paths. Missing home fails required defaults; complete explicit overrides work without HOME. Never use shell expansion or silently fall back to working/temporary directories.

Flags override saved settings, then defaults. Saved relative paths resolve against the settings directory; explicit library state paths are absolute. Another config-file path does not change the default data root. Successful setup/upgrade saves resolved paths atomically; server never rewrites them. Do not migrate or scan another home: changing directories cannot recover old reservations/fences. Services need a stable home or explicit paths.

Require owned 0700 directories, 0600 files and exclusive locks. Reject unsafe, symlinked or foreign-owned provider paths; preserve unrelated `.pulse` entries without recursive chmod/move/cleanup. Keep control sockets private with short names; overlong native paths fail before dispatch, allowing a shorter explicit state directory. External binaries/assets/protected credentials retain their configured references. Never persist leases, raw assignments or automatic secret-bearing logs; no log archive is added.

## Admission and durable execution

Perform whole-batch structural validation before creating resources. Require 1–100 distinct valid launch IDs, complete credential-free JobRefs, nonempty opaque leases and bounded quantities. Copy retained inputs. Listener validation and failures use the subscription contract without corrupting admission results.

Match selectors literally except for the API's `*` wildcard, preserve all requested constraints, and use exact all-of capability matching. Resolve a concrete qualified base and fitting CPU/memory profile. Check host CPU/RAM/disk budgets, including preparation builders and existing reservations. Report only conservative worker-usable allocation facts, not nominal VM RAM or requested capabilities. Initially decline positive scratch requirements and any `StartBefore` before dispatch. Reject malformed limits; support positive runtime caps and zero uncapped runtime. There is no queue holding a declined lease while capacity warms.

Keep a versioned, atomically written owner store with fsync/rename and an exclusive controller lock. Per-launch records contain correlation, selected configuration, resource identities, reservation and dispatch/delivery phase; never serialize an API Launch containing its lease. Persist intent before any ambiguous native operation. A runner obtains a separate per-launch lock and durable start fence, so controller restart cannot race it into a second execution.

| Boundary | Durable fact and behavior |
| --- | --- |
| Before intent/reservation commits | The provider can definitely decline without a later worker start. Storage failure causes no dispatch. |
| Intent committed, runner handoff pending | Reserve the slot. Cancellation alone cannot establish non-acceptance. Release only after proving no runner/native dispatch can still occur. |
| Runner begins native dispatch | Persist the start fence first and make one native start attempt. A lost response becomes `unknown`; do not repeat the operation. |
| Native dispatch acknowledged | Return `accepted` with an empty reason. The VM may still fail to boot or deliver the worker. |
| Guest worker intent committed | Start c2j once. Reconnecting may observe intent/status, never invoke a second worker. |
| Owned native termination confirmed | Remove owned job resources, release the reservation and retain a small duplicate fence. Exclude terminal records from `List`. |

Repeated fenced launch IDs are rejected without executing again, including after completion or reopening. Retain only terminal identity fences, not logs or leases. No automatic fence expiry is safe under this promise; deleting owner state ends duplicate protection and must not be documented as routine cleanup. Definite pre-dispatch declines need no execution fence once absence of any pending handoff is established.

## Runner, guest and credential boundary

I recommend one transient systemd service per runner, outside the provider service's cgroup. Use a supported system manager or lingering user manager with operator-provisioned, narrow launch authority. Each unit owns its runner and VMM descendants. A separate session alone cannot guarantee survival when a service manager removes the provider's cgroup. Hosts without a qualified independent supervisor fail preflight; adding another launcher is future work.

Pass the assignment to the runner through a private, same-user authenticated Unix connection. Keep lease bytes in memory and deliver them over the per-VM vsock endpoint into the guest agent, then to `c2j run with-lease --lease-file -` on stdin. Pass the original JobDB endpoint/job ID and verified concrete allocation facts through supported c2j flags. Never claim work, rewrite the lease or place it in unit properties, argv, environment, disk records or cloud-init.

The guest agent checks its launch identity, durably commits worker intent, starts c2j as the prepared unprivileged worker, drains output and shuts down after worker exit. Lease receipt is never treated as permission to execute twice. A runner failure before delivery cannot be repaired by recovering lease bytes from disk: preserve uncertainty, observe/terminate the exact owned failed instance when safe, and let the dispatcher decide a later authorized attempt. VM recovery means recovering observations and ownership, not replaying work.

Measure a positive runtime cap from native dispatch, including boot time. The runner uses a monotonic deadline, with the independently supervised unit's runtime limit as a backstop if the runner fails. Cap expiry terminates only that unit's VM resources. Zero adds no worker runtime cap; a separate bounded boot/delivery grace may clean up a VM proven never to have started c2j. A running or possibly running worker is never killed merely for age or unreachable networking. Host reboot interrupts work; reopen must not restart an old lease.

## Images, networking and cleanup

Prepare images only through setup/reconciliation. Verify cloud-disk and boot-asset digests before boot. Build the guest with pinned Nix/c2j/tools and guest-local Docker, verify the environment, then publish a stopped immutable source. Never include a lease or committed worker intent in a reusable source. Retain the old usable source if preparation fails.

Each assignment receives a private raw-disk clone, fresh guest machine identity, unique MAC, reserved address/TAP and private vsock/API paths. Reflink where supported; otherwise copy and check available host disk. Never share a writable source or executed disk with another assignment. Operator-provided networking supplies routes, DNS and guest access to JobDB/Git/caches. Vsock is a local control channel, not guest egress. Do not install bridges, firewall rules or persistent host daemons implicitly.

On reopen, correlate state with systemd unit identity, runner locks, boot identity and exact native resources. Never trust a stale PID or name prefix. Unreachable control endpoints yield observation failure/uncertainty, not an empty successful inventory. Quarantine mismatched identities or corrupt state rather than deleting or freeing possibly active capacity. Cleanup requires native termination confirmation, then removes only resources owned by that launch. Reduced counts drain idle capacity; active work and reservations remain accounted for.

`GetCapabilities` reads the configured qualified catalog, including busy/warming bases, without backend calls. Unsupported custom capability claims cannot become execution facts through configuration alone. Revalidate the prepared guest before allocation. `List` observes active owned instances, retains full JobRef/LaunchID, excludes images/builders/terminal fences, and returns opaque owner/filter-bound pagination. Inventory failure is an error. A changing paginated view never proves an uncertain launch was not accepted.

## Independent logging and SDK hosting

Pin the latest reviewed root API and compatible independently versioned SDK; the inspected root commit remains `2919f1ed5e42`. Verify published resolution without replacements and compile an interface assertion. Use a minimal external host fixture to test both local process and outbound collector paths, including baseline-3 rejection of incompatible peers. Existing Pulse/Tart migration remains excluded.

A nil listener returns a nil subscription. A requested listener always gets a handle, including invalid options or unavailable sources; report observation failure there while preserving admission outcomes and errors. Register before launch observation begins. Use independent listener context/duration, serial callbacks and stable typed terminal results.

Split binary output into records of at most 4096 bytes before assigning consecutive per-launch sequence numbers across sources. Preserve stdout/stderr separately and identify launch progress. Coverage distinguishes pending, active, closed, unavailable and interrupted sources; closure is not job success. Never automatically emit assignment/configuration/stdin/environment values.

Use finite per-subscription queues, bounded active subscriptions and a finite callback budget. Callback slots remain occupied until callbacks actually return, including after `Done`, `Close` or constructing another runtime. A process-wide ceiling prevents repeated reopening from creating unlimited abandoned callbacks; listener saturation must not alter admission. Overflow or an unresponsive callback ends observation with the appropriate typed reason, promptly closing `Done` without waiting for the callback. Do not create unlimited replacement callback goroutines. The runner continues draining/discarding output after consumers detach, so a full log pipe cannot block c2j. Provider or transport loss ends affected subscriptions; no historical replay or automatic resubmission follows.

## Test fixtures at execution boundaries

I will keep the state store, native supervisor, guest transport and clock behind narrow internal seams. Production implementations remain local code; fakes expose calls and durable facts for assertions, not alternative provider protocols. Explicit barriers control admission/close/dispatch races instead of sleeps. A fake clock drives caps and listener expiry; real systemd termination remains a native assertion.

The storage fixture will inject failure and subprocess termination around write, file fsync, rename, directory fsync, fence commit and runner handoff. Reopening the actual temporary store must preserve every possibly active reservation. The native-call oracle counts starts across controller restarts; tests must prove one attempt, not merely inspect an enum. Symlink/ownership and unrelated-resource canaries exercise destructive boundaries.

The guest fixture will exchange real framed messages and launch an observable child process with a private stdin pipe. It compares exact lease bytes, counts c2j starts, and exercises duplicate, partial, mismatched-identity and disconnected deliveries across guest-intent persistence. Sentinel checks cover state, temporaries, argv, environment and exported diagnostics. The fixture is not proof of real JobDB validation/renewal; those remain K/J tests. Output fixtures write beyond pipe and queue capacity and must complete after observers detach.

## Candidate checks and remaining qualification

The first implementation commit adds `.c2/config.yaml` requesting exactly base `ubuntu*` and capability `kvm`; the latest human direction refines the current base request to `ubuntu*-arm64`. Validate the current c2j schema before consuming that request. This authorizes placement only, not package installation, provisioning or broader privileges. Inspect the allocated native architecture, KVM, independent systemd authority, budgets and networking. Recheck Go after worker changes and request approval if unavailable.

Add `.c2/recipes/hooks/build/verify.yaml` and `scripts/verify.sh` early, before substantive runtime code. The recognized hook returns the normal typed result and bounded redacted diagnostics. Its full entry point must fail until implementations, prerequisites and assertions exist; focused development checks cannot substitute for full verification. Keep generated binaries and raw test streams in scratch, with only useful redacted results exported.

| Check | Required evidence before passing |
| --- | --- |
| Contract and deterministic suites | Exact Provider assertion; batch/matching/capacity, concurrency, storage/crash, guest protocol, logging, defaults and CLI assertions, including all derived subcases. |
| Go checks | Focused checks, `go test ./...`, `go test -race ./...` and `go vet ./...`; every required child succeeds. |
| Builds | Both Linux ARM64 command builds succeed; this provides compilation evidence only. |
| External consumer | Recheck API main and published pins; require both reviewed modules. A separate clean consumer exercises process and authenticated outbound hosting, including rejection and transport-loss cases. |
| Native execution | Every K assertion passes on authorized native ARM64 KVM hosts against the exact candidate, fixed pins and disposable JobDB/lease fixtures. |

Use `GOWORK=off`, clean scratch module state and a scratch file proxy for the unpublished provider candidate; API/SDK dependencies resolve publicly. Inspect selected modules for hidden replacement directives. The SDK host must actually discover, submit, observe logs and list, not only build. The prior dependency probe is insufficient for this row. Request API-owner work only for a demonstrated incompatibility.

The verifier will associate the 67 retained expectation IDs and every derived subcase with required test identities, then compare them with actual structured test events and child outcomes. Missing mappings, empty suites, skipped/unexecuted assertions, incomplete streams and failed commands fail the gate. Meta-tests inject each failure and a complete positive fixture; no test can establish native support by printing a pass label.

`scripts/acceptance.sh --arch arm64` is the explicit native entry point. They require the actual host architecture and K/J preflight, then run mandatory assertions without skip gating. The ARM64 result may be collected only from authorized fixture runs and must match candidate, architecture and hypervisor/image/boot/c2j/Nix/tool pins. Missing, stale, wrong-architecture or mismatched results fail aggregate verification. Cross-builds and mocks cannot satisfy this requirement.

The current worker is not qualified. Root must coordinate any remaining authorized hosts, systemd/KVM/TAP permissions, egress, disposable JobDB/leases and fixed assets. Complete locally available code/tests while identifying concrete gaps, then request required host/fixture delivery through consultation. Do not provision host services implicitly or recover/replay old leases. Before merge, report exact candidate/tool/dependency/asset identities, executed assertions and explicit failures. Publication, deployment and a publish hook remain deferred; setup's local promotion of stopped verified sources remains required.

Expected scope includes shared storage defaults and their verification, the worker request and early comprehensive hook. Publication is deferred; size categories remain unchanged:

| Cell | Core code | Tests | Docs |
| --- | --- | --- | --- |
| root | none | none | small |
| pulse-cloudhypervisor | large | large | medium |
