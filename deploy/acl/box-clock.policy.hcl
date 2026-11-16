# deploy/acl/box-clock.policy.hcl -- the stopped-box clock of every box.
#
# This is one rule for a CLI token that is not given the whole box-operator
# policy. Such a token usually carries another narrow policy for the job reads a
# command needs (list-jobs, read-job); box-clock adds the one variable those
# policies do not cover: nomad/jobs/box-<id>/stopped, the clock that says when a
# pass first saw a box not running.
#
# Apply it with a management token, and give it to a token without dropping what
# that token already has:
#
#   deploy/acl/apply.sh clock --token <accessor id>
#
# or by hand:
#
#   nomad acl policy apply -description "box: the stopped-box clock of every box" \
#     box-clock deploy/acl/box-clock.policy.hcl
#   nomad acl token info <accessor id>          # its current policies
#   nomad acl token update -policy <each current policy> -policy box-clock <accessor id>
#
# What each capability is for (internal/cli/stopped.go):
#
#   read     box ls, box status and `box reap --dry-run` read the clock
#   write    a reap pass sets the clock when it first sees a box not running
#   destroy  box create, box start and box rm delete the clock
#
# It grants nothing else: not the description of a box
# (nomad/jobs/box-<id>/spec), not the executor credentials
# (nomad/jobs/box-executors), not the stopped-box term, and no job, node or
# volume capability. A token that runs every box command wants box-operator;
# box-clock is for the token that must not have it.

namespace "default" {
  variables {
    path "nomad/jobs/box-*/stopped" {
      capabilities = ["read", "write", "destroy"]
    }
  }
}
