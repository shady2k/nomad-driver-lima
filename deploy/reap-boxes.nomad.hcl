# deploy/reap-boxes.nomad.hcl -- the pass that deletes the disks of boxes whose term
# ran out.
#
# A box is for one task: an agent hands its results over through git and deletes
# the box itself with `box rm <id> --yes`. A stopped box's disk is deleted 24
# hours after it stopped, as the backstop for an agent that forgot, and this job is
# what runs that pass: every 15 minutes, on a node that runs the box driver.
#
# It is a task of the box driver, as a bake and a publish are, and not a daemon:
# this project has no daemon, the coordinator may be a laptop, and a Mac node may
# have neither exec nor docker. The task runs no VM.
#
# The task reaches the Nomad API through the task API: the client serves its own
# API on a unix socket (NOMAD_UNIX_ADDR) and hands the task its workload identity
# token (NOMAD_TOKEN), which is what `identity { env = true }` is for. Nothing
# here holds a secret or an address, and the pass needs the capabilities of
# deploy/acl/box-reap.policy.hcl, attached to *this* job: Nomad resolves a
# dispatched allocation's policies by the parent job id, so an attachment to
# reap-boxes covers every reap-boxes/periodic-<time> child it dispatches.
#
# Register it once per cluster, after the policy (deploy/acl/README.md):
#
#   deploy/acl/apply.sh reap          # with a management token
#   deploy/reap.sh                    # with the coordinator's token
#
# The job's name is not box-<id>: a box job is box-<id>, and a periodic job named
# box-reap would collide with a box of the id "reap" and be listed as one.
#
# `box reap` on the coordinator runs the same pass by hand, and
# `box reap --dry-run` lists what the next pass would delete and when.
job "reap-boxes" {
  type = "batch"

  # Every 15 minutes, and never two passes at once: a pass that overlaps another
  # would race it on the same box's clock for no gain. `crons`, not the
  # deprecated single `cron`, which Nomad 2.0 warns about on every registration.
  periodic {
    crons            = ["*/15 * * * *"]
    prohibit_overlap = true
  }

  group "reap" {
    count = 1

    # A pass that fails is a pass the next one repeats; there is nothing to gain
    # from a retry of the same minute.
    restart {
      attempts = 0
      mode     = "fail"
    }

    task "reap" {
      driver = "box"

      # The task API and the workload identity token, which are what the pass
      # talks to Nomad with.
      identity {
        env = true
      }

      config {
        # The block carries nothing: the term is one number for everyone, and
        # there is no extension, no `box keep` and no per-box setting.
        reap {}
      }

      resources {
        cpu    = 500
        memory = 128
      }
    }
  }
}
