# Private JobDB lease fixture

This Linux-only test command supplies disposable assignments for the provider's
native tests. It is not a deployed operator service, a guest image, a networking
setup tool or a substitute for the ten LIVE assertions.

The nested Go module pins the recovered c2j and JobDB source references separately
from the public provider module. Its tests use the installed `c2j`, Git and Go/GCC:

```sh
GOWORK=off GOTOOLCHAIN=local go -C tests/fixture test -race -count=1 -timeout=90s ./...
GOWORK=off GOTOOLCHAIN=local go -C tests/fixture vet ./...
```

Both local and full verification require these checks. Tests create their own
SQLite database, Git source, jobs and leases. The installed c2j executes a
three-second command with the exact fixture-owned capability on redirected stdin,
in a private HOME with no inherited task credentials. Its raw output is discarded.
Tests also check actual c2j rejection of expired, mismatched and completed
assignments without new chapters or replacement acquisition. This executes on
the host, not in a Cloud Hypervisor guest.

For the authorized isolated guest test network, build into an existing private scratch
directory and run as the same user as the native test:

```sh
GOWORK=off GOTOOLCHAIN=local go -C tests/fixture build -o /absolute/private/ch-lease-fixture .
/absolute/private/ch-lease-fixture \
  --control-socket /absolute/private/control.sock \
  --identity cloudhypervisor-fixture \
  --lease-duration 1m \
  --listen OPERATOR_RESERVED_IP:18880 \
  --endpoint http://GUEST_REACHABLE_IP:18880
```

The initial lease duration defaults to one minute and accepts 1 second through
10 minutes. Select a duration that covers the fixture's guest startup; the helper
does not renew it while booting. The command runs for longer than that duration,
so a real execution can demonstrate renewal after the original expiry. Local
tests use a two-second lease and a three-second command.

The control socket's parent must already exist, be owned by this user and have
mode 0700. The helper creates a 0600 socket and checks Linux peer credentials.
The descriptor's `identity` must match `--identity`; its `control_socket` points
to this socket. The existing descriptor version and provider API are unchanged.
The endpoint must be an HTTP origin without credentials, query or path. Defaults
bind loopback for local tests only. Listening does not configure networking.
The operator must supply routing and access confined to the disposable fixture.
No host services or privilege changes are made.

Every control request is an HTTP GET over that socket with the
`X-Pulse-Fixture` identity header. Responses use `Cache-Control: no-store`.

| Request | Result |
| --- | --- |
| `/health` | `{"ready":true}`; native preflight requires this handshake. |
| `/launch?name=CASE&case=valid&profile=PROFILE` | Complete `job` reference and opaque `lease` string. Omit `profile` to use `renewal`. |
| `/launch?name=CASE&case=expired` | Capability whose original lease has expired. |
| `/launch?name=CASE&case=mismatched` | Reference to a different isolated job from the held capability. |
| `/launch?name=CASE&case=completed` | Capability for an already completed isolated job. |
| `/status?name=CASE` | Nonsecret job state, completion outcome, exact output check and authoritative lease-event observations. |

Names match `[a-z][a-z0-9-]{0,63}`. A name reserves one attempt before side effects;
retrying the same name, variant and profile returns the same assignment bytes,
including after expiry. It never acquires another lease. A changed variant or profile, or ambiguous failed
attempt is rejected. At most 64 names are retained. Use a fresh name for an
independent assertion, never as an automatic retry of uncertain provider work.
The helper stops after 30 minutes or SIGINT/SIGTERM and removes only its own
private temporary directory and sockets. SIGKILL cannot perform cleanup.

Valid jobs select `renewal`, `isolation`, `docker`, `lifetime`, or `output`.
These are fixed commands; no request can provide shell text. Isolation writes a
case marker in the disposable worker home, and output emits 128 records on
each stream. Docker requires `--docker-image sha256:...` at helper startup and
uses a preloaded image containing `/bin/sh`, with network disabled and pulls
forbidden. Invalid-lease variants use only the default profile. Status verifies
both command streams as well as a successful, nontruncated terminal result.

Assignments stay in helper/client memory and the private socket-to-stdin path.
Never print, save or put the response into shell substitution, arguments,
environment variables, configuration or review evidence. `tests/live/fixture.go`
provides the authenticated, bounded client for health, assignment and status;
assignment formatting redacts the lease. Errors contain fixed messages only.
SQLite owns its private test records. The helper never opens the active task's
JobDB or imports its lease.

The guest-facing endpoint offers read access to this disposable database's
metadata and read-only synthetic Git source. It grants no cell mutation or
acquisition authority. Execution writes require JobDB's validated capability.
Replacement claim routes are denied and counted. The helper stops renewing at
handoff. Status counts successful expiry-extending renewal events from JobDB,
not requests; `renewed_beyond_initial` requires a renewal recorded after the
original expiry. `job` in status identifies the actually leased job, which differs
from the supplied assignment in the mismatched case. `output_ok` requires a
successful terminal job outcome with the exact command stdout, exit status and
nontruncated result; the marker in a recipe alone cannot satisfy it.

The managed verifier stages the helper after promoting a prepared guest source.
It binds the JobDB/Git endpoint to the private bridge, restricts guest traffic
to the assigned addresses and keeps host control on the protected Unix socket.
The `facts` and `peer-isolation` profiles check usable allocation and guest
identity from inside the workload VM. Results are filtered before leaving the
fixture.
