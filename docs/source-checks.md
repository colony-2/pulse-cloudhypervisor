# Current repository and published API checks

I inspected this checkout and the retained collection on 21 November 2026. The dependency probe below is retained evidence from 20 November, not a new implementation check. This supports the [approved proposal](provider-implementation.md).

## Accepted direction

I inspected every page of the current job's task inventory and downloaded the exact `/response` fields from its three recorded human-review task occurrences. After the two revisions below, the latest response in job `PUUYAWfpUOHruoOJvhpfbmgNMlU` is:

```json
{"attachments":[],"direction":"accept","feedback":"","settings":{}}
```

The original root direction was accepted in job `3MOLBzpXPU382mwbmM8zXiOasNo`. The current approval includes both revisions. I keep its one-pager unchanged and make the revised behavior concrete through derived assertions; no product scope or exclusion changes are proposed.

## Ubuntu KVM worker request and early verification

Exact human wording:

```text
you'll need to request a base of ubuntu* and capability of kvm so you can use kvm for testing. best make that your first commit to .c2/config.yaml

don't worry about publishing for now. but get a verify hook in early and make sure it tests everything.
```

This authorizes the exact worker placement request in the first implementation commit. It does not authorize extra worker packages, implicit host provisioning or broader privileges. The recognized hook path is `.c2/recipes/hooks/build/verify.yaml`; the prior inspection checked the build-hook guide at this job's recipe pin `eeca2e9598e13a2ef8456c32f4975fe66062050e`. A script alone is not discovered. No foreign hook or package declaration was copied. Publication remains deferred.

## Provider data location revision

Exact human wording:

```text
if we need to store data for the provider, default to ~/.pulse/cloudhypervisor/*
```

The accepted defaults are command settings at `~/.pulse/cloudhypervisor/config.json` and owner-isolated state beneath `~/.pulse/cloudhypervisor/owners/<owner>/`. Explicit paths win. Home resolution, private ownership/permissions, safe failure and the prohibition on disk leases remain required; no migration or log archive is added.

## Repository and available prerequisites

The checkout is `9cc01fb798feea8e625f56af03cd346db363db7f`. `git ls-files` contains only `PURPOSE.md`; repository status is clean. I read the purpose and four supplied review documents. There is no repository `AGENTS.md`, separate feedback/error/hooks/children input, or implementation to recover. Cell initialization established ownership only.

The prior worker inspection ran installed Go 1.26.10 from `/nix/store/k2vfr5rajk1gjpsdzgqdfz1gkadrc7v7-go-1.26.10/bin/go`, reporting `go version go1.26.10 linux/arm64`. No toolchain was installed. It found no `/dev/kvm`, `cloud-hypervisor` or `systemctl`. Recheck tools and actual native prerequisites after Ubuntu/KVM allocation; request approval if a required toolchain is unavailable. That request cannot qualify either architecture by itself.

No authorized AMD64/ARM64 hosts, disposable JobDB/lease fixtures, networking pool or complete fixed hypervisor/image/boot/guest-tool pins were supplied. Root coordinates remaining host/fixture work. The prior `c2j cells` call failed with `JobDB runtime URL is required`, so it supplied no owner inventory. Broker job inspection works; current `c2j self --json` confirms this cell/job. No owner was inferred from the failed inventory.

## API and published module resolution

On 20 November, `git ls-remote` returned `2919f1ed5e42893f759af0fdd930c1a66824887b` for API HEAD and main. Source inspection found the required three Provider methods and SDK baseline 3. The public proxy resolved both `@latest` modules to:

| Module | Published version |
| --- | --- |
| `github.com/colony-2/pulse-provider-api` | `v0.0.0-20261120061856-2919f1ed5e42` |
| `github.com/colony-2/pulse-provider-api/plugin` | `v0.0.0-20261120061856-2919f1ed5e42` |

Both declare Go 1.22. The SDK requires older root `v0.0.0-20261105011626-401f11e595a8`; explicitly requiring the reviewed root alongside it selected the desired contract. A clean scratch consumer with public proxy resolution, `GOTOOLCHAIN=local` and no replacements/workspace overrides compiled an inert exact-interface implementation and `(*plugin.Process)(nil)` as `api.Provider`.

`go run -mod=mod .` exited 0 and printed `API/SDK contract compiled; SDK baseline=3`. The initial module query failed for missing go.sum in readonly mode; explicit module-update mode in scratch generated sums and succeeded. This was not an API incompatibility.

That probe neither imported a provider candidate nor exercised SDK transports, VMs or SDK-01 through SDK-03. Recheck API main and published resolution at implementation time. The actual candidate must pass the external-consumer tests without replacements; a scratch file proxy can distribute the unpublished candidate while dependencies resolve publicly. Only a demonstrated incompatibility warrants API-owner work.

## Supplied-lease command inspection

During this preparation I read `c2j run with-lease --help` from installed `v0.0.0-20261120054404-3722ba2038af`. It supports `--lease-file -`, `--jobdb`, `--job-id`, concrete execution base/platform and CPU/memory/scratch/capability flags, without claiming work. The guest fixture must verify those argv facts and exact stdin delivery against the selected pinned guest binary. Complete JobRef correlation remains in provider records; do not invent unsupported identity flags. Help inspection is not execution or native qualification.

## Validation limits

The prepared collection preserves the approved one-pager, all 67 original expectation rows and both exact revision texts. Document checks cover local links, statement word limits, test metadata, scope and collection size. Repeated descriptions were shortened to accommodate explicit derived test statements without dropping decisions or evidence. No provider implementation, native qualification, release or deployment has been performed here.

## Human correction after native-fixture coordination

Root coordination job `M3sYVRGIvNVYwiJI2QEeT1Pm0sg` was cancelled with this human completion detail:

```text
limit your testing to arm with kvm. we do not support and today so remove that requirement.

you can add that requirement to your base .c2/config so that jobs run in that context. if you need a package on top, please identify what it is and go through the package request process.
```

This supersedes the original approved AMD64 build/native/counterpart requirements and the exact broad base selector in VERIFY-01. The current selector is `ubuntu*-arm64`, capability `kvm`; c2j's published base matcher supports full-name star matching and documents ARM64 concrete names. Operative design/test wording and PURPOSE now reflect ARM64 only. All 89 IDs remain. The cancelled child delivered read-only assessment, no fixture implementation or reservations; it is not restarted.

On this continuation, published API and plugin `@latest` still resolve to `v0.0.0-20261120061856-2919f1ed5e42`. The allocated Ubuntu ARM64 host permits KVM API 12 access as UID 55050 and runs systemd. Cloud Hypervisor is absent. These observations do not qualify native execution. Package approval and native fixture delivery remain separate from local verification. Historical observations above are retained as history.
