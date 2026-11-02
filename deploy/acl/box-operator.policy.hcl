# deploy/acl/box-operator.policy.hcl -- the `box` CLI on the coordinator.
#
# Apply it once, with a management token:
#
#   nomad acl policy apply -description "box: the coordinator CLI" box-operator \
#     deploy/acl/box-operator.policy.hcl
#   nomad acl token create -name box-operator -policy box-operator \
#     -description "box CLI"
#
# or run `deploy/acl/apply.sh operator`, which does both and prints the new
# token once. Put that token into the coordinator environment as NOMAD_TOKEN
# (~/.config/box/env, mode 0600).
#
# What each capability is for (internal/cli):
#
#   list-jobs          box ls
#   read-job           box status; every wait of create/start/resize; the
#                      allocations `box ls` and `box status` read
#   submit-job         box create/start/stop/rm/resize register and deregister
#                      the box job
#   alloc-exec         box run, ssh, cp and every guest command (boxctl)
#   read-logs          box logs
#   host-volume-write  box create makes the disk box-<id>, box rm deletes it,
#                      box resize raises it; the capability implies
#                      host-volume-create, host-volume-register,
#                      host-volume-delete and host-volume-read
#   node read          box capacity: node list, node info and a node's
#                      allocations (the last one also needs read-job)
#   variables write    box executors set/show/delete, on the one path
#                      nomad/jobs/box-executors (read/write/destroy)
#
# Nothing else is granted: no agent, operator or ACL access, no other namespace,
# no other variable path, no node write. The box workloads have their own
# policy (box-workload.policy.hcl); this one is for the person or agent that
# runs the CLI.

namespace "default" {
  capabilities = [
    "list-jobs",
    "read-job",
    "submit-job",
    "alloc-exec",
    "read-logs",
    "host-volume-write",
  ]

  variables {
    path "nomad/jobs/box-executors" {
      capabilities = ["read", "write", "destroy"]
    }
  }
}

node {
  policy = "read"
}
