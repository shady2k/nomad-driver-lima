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
#   alloc-exec         box run, ssh, cp and every guest command (boxctl); the
#                      driver declares image isolation, so Nomad does not ask
#                      for alloc-node-exec (which would reach raw_exec tasks)
#   read-logs          box logs
#   host-volume-write  box create makes the disk box-<id>, box rm deletes it,
#                      box resize raises it; the capability implies
#                      host-volume-create, host-volume-register,
#                      host-volume-delete and host-volume-read
#   host_volume box-*  box create/start/resize register a job that mounts its
#   mount-readwrite    disk box-<id>: Nomad checks this per volume, apart from
#                      submit-job, and refuses the job without it
#   node read          box capacity: node list, node info and a node's
#                      allocations (the last one also needs read-job)
#   variables write    box executors set/show/delete, on nomad/jobs/box-executors;
#                      box store set/show on nomad/jobs/box-store; box reap term
#                      set/unset on nomad/jobs/box-reap; the description of every
#                      box on nomad/jobs/box-*/spec (all three read/write/destroy,
#                      and no other path)
#
# nomad/jobs/box-<id>/spec is a box's description: the rendered job as JSON,
# written by create/start/resize/upgrade and read by ls/status/start. A box is
# its volume and its disk, so the record of how it was made has to outlive the
# job Nomad deletes a few hours after a stop. It holds no secret: the job names
# the files and the Vault paths an executor reads, never a value.
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
    # The description of every box, where Nomad's job GC does not reach:
    # nomad/jobs/box-<id>/spec holds the rendered job as JSON. `box create`,
    # `start`, `resize` and `upgrade` write it; `box ls`, `status` and `start`
    # read it; `box rm` deletes it. It holds no secret -- a credential reaches a
    # box through a template, never through a value in a job.
    path "nomad/jobs/box-*" {
      capabilities = ["read", "write", "destroy"]
    }
    path "nomad/jobs/box-executors" {
      capabilities = ["read", "write", "destroy"]
    }
    # The operator's image store: `box store set` writes it, `box store show`
    # reads it. A publish job reads it with a workload policy of its own
    # (box-store.policy.hcl), never through this one.
    path "nomad/jobs/box-store" {
      capabilities = ["read", "write", "destroy"]
    }
    # The cluster's stopped-box term: `box reap term` reads it, `set` writes it,
    # `unset` deletes the variable (destroy). The reap pass reads it with its own
    # policy (box-reap.policy.hcl), and never writes it.
    path "nomad/jobs/box-reap" {
      capabilities = ["read", "write", "destroy"]
    }
  }
}

host_volume "box-*" {
  capabilities = ["mount-readwrite"]
}

node {
  policy = "read"
}
