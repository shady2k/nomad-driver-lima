# box ACL policies

Two Nomad ACL policies, the commands that apply them, and a test that proves
them against a real Nomad agent.

| file | for whom |
| --- | --- |
| `box-operator.policy.hcl` | the `box` CLI on the coordinator: jobs, host volumes, `alloc exec`, and the executors variable |
| `box-workload.policy.hcl` | one box job's own workloads: read `nomad/jobs/box-executors` |
| `apply.sh` | applies both, creates the operator token, and probes a cluster |
| `test.sh` | starts a throwaway agent with ACLs and checks both policies |

Nomad's own ACL engine reads these files; `internal/cli/aclpolicy_test.go` parses
them with `github.com/hashicorp/nomad/acl` and asserts every capability the
command needs and every capability it must not have, so a policy that drifts
from the code fails `go test`, not `box create`.

## The operator

```bash
# once per cluster, with a management token (NOMAD_ADDR and NOMAD_TOKEN set)
deploy/acl/apply.sh operator

# that prints the new token once; keep it out of git
install -m 0600 /dev/null ~/.config/box/env
printf 'NOMAD_TOKEN=%s\n' '<the token>' >> ~/.config/box/env

# and check what the token may and may not do
deploy/acl/apply.sh check --token <the token>
```

The same thing by hand:

```bash
nomad acl policy apply -description "box: the coordinator CLI" \
  box-operator deploy/acl/box-operator.policy.hcl
nomad acl token create -name box-operator -policy box-operator
```

What it grants, and the command that needs it:

| capability | for |
| --- | --- |
| `list-jobs` | `box ls` |
| `read-job` | `box status`, every wait of `create`/`start`/`resize`, the allocation lists |
| `submit-job` | `box create`, `start`, `stop`, `rm`, `resize` |
| `alloc-exec` | `box run`, `ssh`, `cp` and every guest command (`boxctl`) |
| `read-logs` | `box logs` |
| `host-volume-write` | `box create` makes the disk `box-<id>`, `box rm` deletes it, `box resize` raises it |
| `host_volume "box-*"` `mount-readwrite` | `box create`, `start`, `resize`: a job that mounts its disk `box-<id>` is refused without it, whatever the namespace grants |
| `node` `read` | `box capacity` |
| `variables` `read`/`write`/`destroy` on `nomad/jobs/box-executors` | `box executors set`, `show`, `delete` |

Nothing else: no agent, operator or ACL access, no node write, no other
namespace, no other variable path. A token with `box-operator` alone can run
every `box` command and nothing more.

## The box workloads

A box renders the executors variable into `secrets/` with one template per file.
The template reads `nomad/jobs/box-executors` with the allocation's **workload
identity** token, at render time.

The default workload identity scoping does **not** cover that path. Nomad gives
a workload identity `read` and `list` on exactly these variable paths
(`acl.matchingVariablesCapabilitySet`, `workloadVariablesCapabilitySet`):

```
nomad/jobs
nomad/jobs/<job>
nomad/jobs/<job>/<group>
nomad/jobs/<job>/<group>/<task>
```

`nomad/jobs/box-executors` is the *sibling* of `nomad/jobs/box-dev1`, not below
it, so a box job's own identity cannot read it: the templates would render empty
files, the box would start, and every executor would report "not configured".
`internal/cli/aclpolicy_test.go` pins that behaviour.

The fix is a job-attached policy, one per box job (Nomad 2.0 supports `-job`,
`-group` and `-task` on `nomad acl policy apply`):

```bash
# one box, exactly
nomad acl policy apply -namespace default -job box-dev1 \
  -description "box dev1: read the executors variable" \
  box-workload-dev1 deploy/acl/box-workload.policy.hcl

# or every box on the cluster (`box-*` jobs, one policy each)
deploy/acl/apply.sh workloads --all
deploy/acl/apply.sh workloads dev1 dev2
```

A box created after that needs its own attachment: `box create` does not touch
ACLs (the CLI token has no ACL write on purpose), so run `apply.sh workloads
<id>` right after creating a box, or run `--all` after a batch of creates.
Without the attachment the box still starts and runs; only the executors have no
credentials.

Attaching the policy to the namespace instead — `nomad acl policy apply
-namespace default box-workload deploy/acl/box-workload.policy.hcl`, without
`-job` — gives the same read to **every** workload identity of the namespace,
boxes or not. Use it only when every job in the namespace may read the executor
credentials; a namespace-wide attachment is the one thing here that widens the
grant.

The policy is read-only: a box can never write the variable that holds its own
credentials, so one compromised box cannot hand them to another. `-group box`
and `-task box` narrow the attachment to the box task, which is the same set for
a box job (one group, one task).

## Testing the policies

```bash
deploy/acl/test.sh          # throwaway agent on 24666-24668, needs nomad + curl + python3
go test ./internal/cli/ -run 'ACL|Workload'   # the policies against Nomad's ACL engine
```

`test.sh` bootstraps ACLs on its own agent, applies both policies, and checks
that the operator token can list jobs and nodes and host volumes, read the
executors variable, write it (and clean up after itself), submit and read a job,
and deregister it — while writing an ACL policy, writing another variable, listing
another namespace and writing a node are all denied. It also checks that the
job-attached policy lands on the job (`JobACL` of `box-workload-test`).

## When ACLs are off

A cluster without ACLs needs none of this: `box` works, and the templates read
the variable through the local server's unrestricted access. For a cluster with
ACLs, apply `box-operator` before the first `box` command, and attach
`box-workload` to each box job before its executor credentials matter.
