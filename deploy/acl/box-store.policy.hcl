# deploy/acl/box-store.policy.hcl -- what a publish job may read.
#
# A publish is a batch job of the box driver, box-publish-<version>: it puts an
# image that was baked on one node into the operator's store, and it reads that
# store's url and token from the variable nomad/jobs/box-store, rendered into its
# own secrets by templates (never into its task config).
#
# The default workload scoping does NOT cover that path. Nomad gives a workload
# identity read and list on nomad/jobs, nomad/jobs/<job>, nomad/jobs/<job>/<group>
# and nomad/jobs/<job>/<group>/<task> (acl.matchingVariablesCapabilitySet), and
# nomad/jobs/box-store is none of those for a job called box-publish-project-1 --
# it is the sibling of nomad/jobs/box-publish-project-1. Without this policy the
# template renders an empty file and the publish fails with "this node has no
# image store configured".
#
# Attach it to one publish job, after that job exists:
#
#   nomad acl policy apply -namespace default -job box-publish-project-1 \
#     -description "publish project-1: read the image store" \
#     box-store-publish-project-1 deploy/acl/box-store.policy.hcl
#
# A publish job is short-lived and is purged when it ends, so a policy attached to
# it stays behind: delete it (`nomad acl policy delete box-store-publish-...`) when
# that version is published for good.
#
# Do NOT attach it to a box job or to the namespace: a box never needs the store,
# and a namespace-wide attachment would hand the store's write token to every
# workload in it -- including the boxes, whose only interest in a credential is a
# provider key.
#
# It is read only: a publish reads the store, it never writes the variable that
# holds its own credential.

namespace "default" {
  variables {
    path "nomad/jobs/box-store" {
      capabilities = ["read"]
    }
  }
}
