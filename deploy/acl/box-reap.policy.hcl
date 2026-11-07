# deploy/acl/box-reap.policy.hcl -- what the box reap task may do.
#
# `box reap` deletes the disk of a box that has not run for 24 hours. It runs as
# a periodic Nomad job of the box driver (deploy/reap-boxes.nomad.hcl), and the task
# authenticates with its own workload identity: the token is a file and an
# environment variable of the task, never a secret of the cluster.
#
# Attach it to the periodic job, not to the namespace:
#
#   nomad acl policy apply -namespace default -job reap-boxes \
#     -description "box reap: delete the disk of a box that stopped 24 hours ago" \
#     box-reap deploy/acl/box-reap.policy.hcl
#
# or run `deploy/acl/apply.sh reap`, which does that.
#
# Nomad resolves the policies of a dispatched allocation by the *parent* job id
# (structs.Job.GetIDforWorkloadIdentity), so an attachment to the periodic job
# reap-boxes covers the child job reap-boxes/periodic-<time> it dispatches. A policy
# attached to one child would be lost at the next dispatch.
#
# What each capability is for (internal/cli/reap.go):
#
#   list-jobs           the boxes, the bakes and the publishes of the cluster
#   read-job            one box's job, and the allocations of its job
#   submit-job          purge the job of a box whose disk is deleted
#   host-volume-read    list the box disks
#   host-volume-delete  delete the disk of a box whose term ran out
#   node read           whether the node that holds a disk is up: a node that is
#                       down is never a reason to delete anything
#   variables read      the clock of a stopped box (nomad/jobs/box-<id>/stopped)
#   variables write     and claim it while the disk is deleted
#   variables destroy   delete the clock, and the description of the box
#                       (nomad/jobs/box-<id>/spec) with the disk
#
# It is deliberately not the operator policy: no alloc-exec, no job submission
# beyond purging a box that is already over, no other variable path, no node
# write. A box that runs is never touched, and a pass that cannot read something
# deletes nothing.

namespace "default" {
  capabilities = [
    "list-jobs",
    "read-job",
    "submit-job",
    "host-volume-read",
    "host-volume-delete",
  ]

  variables {
    # nomad/jobs/box-<id>/stopped is when a pass first saw the box not running,
    # and nomad/jobs/box-<id>/spec is the description of the box: both go when
    # the disk goes. The two paths are named one by one, not as
    # nomad/jobs/box-*: a glob there would also match nomad/jobs/box-executors,
    # which holds the executor credentials and is none of the pass's business.
    path "nomad/jobs/box-*/stopped" {
      capabilities = ["read", "write", "destroy"]
    }
    path "nomad/jobs/box-*/spec" {
      capabilities = ["destroy"]
    }
  }
}

node {
  policy = "read"
}
