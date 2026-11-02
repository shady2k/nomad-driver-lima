# deploy/acl/box-workload.policy.hcl -- what a box's own workloads may read.
#
# A box task renders the variable nomad/jobs/box-executors into the files under
# secrets/ (one template per file). The template reads the variable with the
# allocation's workload identity token at render time.
#
# The default scoping does NOT cover that path. Nomad gives a workload identity
# read and list on these variable paths only (acl.matchingVariablesCapabilitySet,
# workloadVariablesCapabilitySet):
#
#   nomad/jobs
#   nomad/jobs/<job>
#   nomad/jobs/<job>/<group>
#   nomad/jobs/<job>/<group>/<task>
#
# nomad/jobs/box-executors is none of those for a job called box-dev1 -- it is
# the sibling of nomad/jobs/box-dev1, not below it. Without this policy the
# templates render empty files: the box starts, and every executor reports "not
# configured".
#
# Attach it to one box job (exact; a policy name per job):
#
#   nomad acl policy apply -namespace default -job box-dev1 \
#     -description "box dev1: read the executors variable" \
#     box-workload-dev1 deploy/acl/box-workload.policy.hcl
#
# or to every box of the cluster:
#
#   deploy/acl/apply.sh workloads --all
#
# Attaching it to the namespace instead (`-namespace default` without `-job`)
# gives every workload identity of that namespace the same read, box or not:
# use that only when every job in the namespace may read the executor
# credentials. Attaching it to a group or task narrows it further (`-group box`,
# `-task box`), which is the same set for a box: one group and one task.
#
# It is read only. A box can never write the variable that holds its own
# credentials, so a compromised box cannot hand credentials to another one.

namespace "default" {
  variables {
    path "nomad/jobs/box-executors" {
      capabilities = ["read"]
    }
  }
}
