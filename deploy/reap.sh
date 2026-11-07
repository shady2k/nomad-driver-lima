#!/usr/bin/env bash
#
# deploy/reap.sh - register the periodic reap job on a cluster.
#
# A box is for one task: its disk is deleted 24 hours after the box stopped, and
# the pass that deletes it is the periodic Nomad job reap-boxes, a task of the box
# driver (deploy/reap-boxes.nomad.hcl). Every 15 minutes it looks over the cluster
# and deletes the disks whose term ran out; `box reap` runs the same pass by hand.
#
# Two steps, in this order, once per cluster:
#
#   deploy/acl/apply.sh reap      the policy of the pass, attached to the job
#                                 reap-boxes: it needs a MANAGEMENT token, and it
#                                 must be there before the first pass runs
#
#   deploy/reap.sh                this job: it needs a token with submit-job in
#                                 the default namespace (the coordinator's
#                                 box-operator token has it)
#
# The job carries no secret and no address: its task gets the Nomad API over the
# task API with its own workload identity (`identity { env = true }` in the job),
# and the policy is what lets that identity delete a disk.
#
#   ./deploy/reap.sh --dry-run    # print what would be registered, change nothing
#
# Environment, the same as for `box` itself: NOMAD_ADDR, NOMAD_TOKEN,
# NOMAD_CACERT, NOMAD_NAMESPACE (deploy/README.md, or ~/.config/box/env).
set -euo pipefail

readonly PROG=${0##*/}
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
JOB=$DIR/reap-boxes.nomad.hcl
NOMAD=${NOMAD:-nomad}

die() { printf '%s: %s\n' "$PROG" "$*" >&2; exit 2; }
log() { printf '%s: %s\n' "$PROG" "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; }

dry_run=false
case ${1:-} in
	--dry-run) dry_run=true ;;
	-h | --help) usage; exit 0 ;;
	"") ;;
	*) die "unknown argument '$1' (--help shows the usage)" ;;
esac

need "$NOMAD"
[[ -f $JOB ]] || die "the job file $JOB is missing: this script ships with it"
[[ -n ${NOMAD_ADDR:-} ]] || die "NOMAD_ADDR is not set (deploy/README.md, or ~/.config/box/env)"

if $dry_run; then
	log "would register $JOB on $NOMAD_ADDR with the token in NOMAD_TOKEN:"
	"$NOMAD" job run -output "$JOB" | sed 's/^/  /'
	log "the policy must be applied first: deploy/acl/apply.sh reap (a management token)"
	exit 0
fi

log "registering the periodic reap job reap-boxes on $NOMAD_ADDR"
"$NOMAD" job run "$JOB"
log "the first pass runs at the next quarter of an hour; its log is the record:"
log "  nomad job status reap-boxes                    # the newest allocation's id"
log "  nomad alloc logs <the allocation id>"
log "if the task stops with 'Permission denied', the policy is not attached to the job:"
log "  deploy/acl/apply.sh reap   (with a management token), then nomad job run $JOB"
