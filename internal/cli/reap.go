package cli

// The pass that deletes the disks of boxes whose term ran out.
//
// A box is for one task. The agent hands its results over through git and deletes
// the box itself with `box rm <id> --yes`; the pass is the backstop for one that
// forgot. It looks at every box disk on the cluster and deletes the disk of a box
// that has not run for the cluster's stopped-box term, 24 hours unless the
// operator set another (nomad/jobs/box-reap, see reapterm.go). The term is read
// once, before any box is looked at, and used for every box in the pass.
//
// The pass is one function, run from two places: `box reap` on the coordinator,
// and the periodic Nomad job `reap-boxes` (deploy/reap-boxes.nomad.hcl), which is a
// task of the box driver. Both call this, so the two cannot drift.
//
// What it never does, and looks again on the next pass instead:
//
//   - delete anything when it cannot read the state of the box: an unreadable
//     state is never "stopped";
//   - delete a box with a live allocation;
//   - delete a box whose disk a bake or a publish job is working on;
//   - delete anything on a node that is down or unreachable.
//
// One box failing does not stop the pass over the others: the failure is one line
// in the log and one in the count.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// reapDeleteTimeout bounds the deletion of one box: the job is purged, the
// allocations are waited for, and the volume is deleted. It is the same figure
// `box rm` uses, because it is the same deletion.
const reapDeleteTimeout = 3 * time.Minute

// reapOutcome is what a pass decided about one box.
type reapOutcome string

const (
	// reapRunning is a box that uses its disk now. A clock it had is cleared.
	reapRunning reapOutcome = "running"
	// reapWaiting is a box that is not running, and whose term has not run out.
	reapWaiting reapOutcome = "waiting"
	// reapStarted is a box a pass saw not running for the first time: its term
	// starts at this pass.
	reapStarted reapOutcome = "started"
	// reapDue is a box whose term ran out, in a dry run: this pass would delete
	// its disk.
	reapDue reapOutcome = "due"
	// reapDeleted is a box whose disk this pass deleted.
	reapDeleted reapOutcome = "deleted"
	// reapKept is a box the pass could not decide about, or must not touch now.
	// It looks again on the next pass, and nothing was deleted.
	reapKept reapOutcome = "kept"
	// reapFailed is a box whose deletion was attempted and did not finish. The
	// pass goes on with the other boxes.
	reapFailed reapOutcome = "failed"
)

// reapedBox is one box a pass looked at.
type reapedBox struct {
	ID      string      `json:"id"`
	Outcome reapOutcome `json:"outcome"`
	// DiskBytes is the size Nomad records for the box's volume, and DiskGiB the
	// same size in GiB.
	DiskBytes int64 `json:"disk_bytes,omitempty"`
	DiskGiB   int   `json:"disk_gib,omitempty"`
	// StoppedAt is when a pass first saw the box not running, and RemovesAt when
	// its disk is due: StoppedAt plus the term.
	StoppedAt string `json:"stopped_at,omitempty"`
	RemovesAt string `json:"removes_at,omitempty"`
	// StoppedForSeconds is how long the box had been stopped when the pass
	// looked at it.
	StoppedForSeconds int64 `json:"stopped_for_seconds,omitempty"`
	// Note says why the pass kept the box, in the words a person reads, or what
	// the box was doing.
	Note string `json:"note,omitempty"`
}

// reapResult is what one pass did.
type reapResult struct {
	DryRun  bool        `json:"dry_run,omitempty"`
	Boxes   []reapedBox `json:"boxes"`
	Deleted int         `json:"deleted"`
	Failed  int         `json:"failed"`
}

// ReapOnce runs one pass over the cluster and returns what it did. With dryRun
// set it deletes nothing and reports what it would delete and when.
func (a *App) ReapOnce(ctx context.Context, dryRun bool) (reapResult, error) {
	// The term comes first, and a term that cannot be used stops the pass before
	// it looks at a box: the pass then deletes nothing and writes no clock. A
	// fallback to the default here would delete the disks of a cluster set to 7
	// days after 24 hours.
	term, err := a.readBoxStoppedTerm(ctx)
	if err != nil {
		return reapResult{}, wrapf(err, "the pass deleted nothing and wrote no clock")
	}
	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return reapResult{}, wrapf(err, "cannot list the host volumes, so a pass cannot find a box disk")
	}
	jobs, err := a.Nomad.JobList(ctx)
	if err != nil {
		return reapResult{}, wrapf(err, "cannot list the Nomad jobs, so a pass cannot tell a bake from a box")
	}
	allocs, err := a.Nomad.Allocations(ctx)
	if err != nil {
		return reapResult{}, wrapf(err, "cannot list the allocations, so a pass cannot tell a box that runs")
	}
	// A box is its disk: the pass starts from the volumes, so a box whose job
	// Nomad has collected is looked at like any other. A volume name that is not
	// a box disk belongs to something else and is left alone.
	byID := map[string]*api.HostVolumeStub{}
	ids := make([]string, 0, len(volumes))
	for _, stub := range volumes {
		if stub == nil || !strings.HasPrefix(stub.Name, boxVolumePrefix) {
			continue
		}
		id := strings.TrimPrefix(stub.Name, boxVolumePrefix)
		if id == "" {
			continue
		}
		if _, ok := byID[id]; ok {
			continue // a stale duplicate of one box's disk
		}
		byID[id] = stub
		ids = append(ids, id)
	}
	sort.Strings(ids)

	res := reapResult{DryRun: dryRun, Boxes: make([]reapedBox, 0, len(ids))}
	nodes := map[string]*api.Node{}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		box := a.reapBox(ctx, id, byID[id], jobs, allocs, nodes, dryRun, term)
		res.Boxes = append(res.Boxes, box)
		switch box.Outcome {
		case reapDeleted:
			res.Deleted++
		case reapFailed:
			res.Failed++
		}
	}
	return res, nil
}

// reapBox decides one box: it reads the state of the box, its clock, and deletes
// the disk when the term ran out and nothing says the box came back.
func (a *App) reapBox(ctx context.Context, id string, volume *api.HostVolumeStub, jobs []*api.JobListStub,
	allocs []*api.AllocationListStub, nodes map[string]*api.Node, dryRun bool, term time.Duration) reapedBox {
	out := reapedBox{ID: id}
	if volume.CapacityBytes > 0 {
		out.DiskBytes = volume.CapacityBytes
		out.DiskGiB = int(volume.CapacityBytes / (1 << 30))
	}
	if volume.State != "" && volume.State != api.HostVolumeStateReady {
		out.Outcome = reapKept
		out.Note = fmt.Sprintf("its volume %s is %s: the disk is being made or deleted", volume.Name, volume.State)
		return out
	}
	// The node that holds the disk is asked first: a node that is down makes the
	// box unreadable in every other way too, and a box on it is not deleted.
	if volume.NodeID == "" {
		out.Outcome = reapKept
		out.Note = fmt.Sprintf("Nomad has not placed its volume %s on a node, so there is no disk to delete yet", volume.Name)
		return out
	}
	node, err := a.reapNode(ctx, volume.NodeID, nodes)
	if err != nil {
		out.Outcome = reapKept
		out.Note = fmt.Sprintf("its node %s cannot be read: %s", volume.NodeID, oneLine(err.Error(), 200))
		return out
	}
	if node.Status != api.NodeStatusReady {
		out.Outcome = reapKept
		out.Note = fmt.Sprintf("its node %s is %s: a node that is away is never a reason to delete a disk", node.Name, node.Status)
		return out
	}

	facts, err := a.readReapFacts(ctx, id)
	if err != nil {
		out.Outcome = reapKept
		out.Note = oneLine(err.Error(), 240)
		return out
	}
	if uses, why, stopping := boxUsesDisk(facts); uses {
		if stopping {
			// The job is stopped and an allocation of it still runs: the box is
			// going away, and its clock keeps running. The disk is not deleted
			// under a live allocation, and the next pass looks again.
			out.Outcome = reapKept
			out.Note = "it is still stopping: " + why
			return out
		}
		// A box that came back is not on a term any more, and the pass is one of
		// the two places that say so (the other is `box start`).
		out.Outcome = reapRunning
		out.Note = why
		// A dry run only looks: the clock stays for the pass that really runs.
		if dryRun {
			return out
		}
		if _, err := a.clearBoxClock(ctx, id); err != nil {
			out.Note = joinNotes(why, "and its clock could not be cleared: "+oneLine(err.Error(), 200))
		}
		return out
	}
	if work := workOnBox(id, volume.NodeID, jobs, allocs); work != "" {
		out.Outcome = reapKept
		out.Note = work
		return out
	}

	clock, seen, err := a.readBoxClock(ctx, id)
	if err != nil {
		out.Outcome = reapKept
		out.Note = oneLine(err.Error(), 240)
		return out
	}
	now := a.now()
	if !seen {
		// A dry run writes nothing, not even the clock of a box it has not seen
		// before: the term shown is the one this pass would start.
		if dryRun {
			setReapClock(&out, now, now, term)
			out.Outcome = reapStarted
			return out
		}
		clock, err = a.startBoxClock(ctx, id, now)
		if err != nil {
			out.Outcome = reapKept
			out.Note = oneLine(err.Error(), 240)
			return out
		}
		setReapClock(&out, clock.at, now, term)
		out.Outcome = reapStarted
		return out
	}
	setReapClock(&out, clock.at, now, term)
	if now.Sub(clock.at) < term {
		out.Outcome = reapWaiting
		return out
	}
	if dryRun {
		out.Outcome = reapDue
		return out
	}

	// The state is read again immediately before the disk goes: the box may have
	// been started while this pass was looking at the others.
	facts, err = a.readReapFacts(ctx, id)
	if err != nil {
		out.Outcome = reapKept
		out.Note = oneLine(err.Error(), 240)
		return out
	}
	if uses, why, stopping := boxUsesDisk(facts); uses {
		out.Outcome = reapKept
		out.Note = joinNotes("it came back while this pass looked at the cluster", why)
		if !stopping {
			_, _ = a.clearBoxClock(ctx, id)
		}
		return out
	}
	// `box start` deletes the clock before it registers the job, so a clock that
	// is gone or rewritten is a box that came back. Claiming it is the one
	// check-and-set Nomad offers here.
	current, still, err := a.readBoxClock(ctx, id)
	if err != nil {
		out.Outcome = reapKept
		out.Note = oneLine(err.Error(), 240)
		return out
	}
	if !still || current.modifyIndex != clock.modifyIndex {
		out.Outcome = reapKept
		out.Note = "a box start cleared its clock while this pass looked at the cluster, so nothing was deleted"
		return out
	}
	claimedClock, claimed, err := a.claimBoxClock(ctx, id, clock)
	if err != nil {
		out.Outcome = reapFailed
		out.Note = oneLine(err.Error(), 240)
		return out
	}
	if !claimed {
		out.Outcome = reapKept
		out.Note = "a box start cleared its clock while this pass looked at the cluster, so nothing was deleted"
		return out
	}
	if err := a.deleteReapedBox(ctx, id, facts.allocID); err != nil {
		out.Outcome = reapFailed
		out.Note = oneLine(err.Error(), 300)
		// The claim marked the clock before the disk went. A disk that is still
		// there gets its time back, so the next pass tries again instead of
		// waiting another full term for a box that has already served its term.
		if a.volumeExists(ctx, id) {
			if clockErr := a.releaseBoxClock(ctx, id, claimedClock); clockErr != nil {
				out.Note = joinNotes(out.Note, "and its clock could not be restored: "+oneLine(clockErr.Error(), 200))
			}
		}
		return out
	}
	out.Outcome = reapDeleted
	return out
}

// setReapClock fills in when the box was first seen stopped and when its disk is
// due, from the term this pass read.
func setReapClock(out *reapedBox, at, now time.Time, term time.Duration) {
	out.StoppedAt = at.UTC().Format(time.RFC3339)
	out.RemovesAt = at.Add(term).UTC().Format(time.RFC3339)
	if d := now.Sub(at); d > 0 {
		out.StoppedForSeconds = int64(d.Seconds())
	}
}

// reapNode reads one node, once per pass.
func (a *App) reapNode(ctx context.Context, nodeID string, cache map[string]*api.Node) (*api.Node, error) {
	if node, ok := cache[nodeID]; ok && node != nil {
		return node, nil
	}
	node, err := a.Nomad.NodeInfo(ctx, nodeID)
	if err != nil {
		return nil, wrapf(err, "cannot read node %s", nodeID)
	}
	if node == nil {
		return nil, errorf("Nomad returned no node for %s", nodeID)
	}
	cache[nodeID] = node
	return node, nil
}

// reapFacts is what a pass reads about one box before it decides anything.
type reapFacts struct {
	// job is the box's job, nil when Nomad no longer has it.
	job *api.Job
	// live is the number of allocations that still use the box.
	live int
	// allocID is the newest allocation of the box, live or not, empty when it
	// never had one. It is what the deletion waits for.
	allocID string
}

// readReapFacts reads the job and the allocations of one box. Every failure is
// an error: a box whose state cannot be read is never "stopped".
func (a *App) readReapFacts(ctx context.Context, id string) (reapFacts, error) {
	facts := reapFacts{}
	job, err := a.Nomad.Job(ctx, BoxJobName(id))
	switch {
	case err == nil:
		if !IsBoxJob(job) {
			return facts, errorf("the job %s is not a box (it has no meta.box = \"true\"), so its disk is left alone",
				BoxJobName(id))
		}
		facts.job = job
	case isNotFoundErr(err):
		// Nomad's job GC deletes a dead job: the box is its disk and its clock.
	default:
		return facts, wrapf(err, "cannot read the job of box %s", id)
	}
	allocs, err := a.Nomad.JobAllocations(ctx, BoxJobName(id))
	if err != nil && !isNotFoundErr(err) {
		return facts, wrapf(err, "cannot read the allocations of box %s", id)
	}
	newest := uint64(0)
	for _, alloc := range allocs {
		if alloc == nil {
			continue
		}
		if !isTerminalAlloc(alloc.ClientStatus) {
			facts.live++
		}
		if alloc.ID != "" && (facts.allocID == "" || alloc.CreateIndex >= newest) {
			facts.allocID, newest = alloc.ID, alloc.CreateIndex
		}
	}
	return facts, nil
}

// boxUsesDisk reports whether the box uses its disk now, in words why when it
// does, and whether it is a box that is on its way out (its job is stopped and an
// allocation of it has not finished yet).
//
// A live allocation is what makes a disk untouchable: one that is starting counts,
// and so does one that is stopping, because the disk is still open. A box Nomad
// has given up on -- its job is dead, or stopped, or gone with its allocations --
// is a broken box, and gets the same term as one that was stopped on purpose.
func boxUsesDisk(facts reapFacts) (uses bool, why string, stopping bool) {
	if facts.live > 0 {
		return true, fmt.Sprintf("%d allocation(s) of it are live", facts.live),
			facts.job == nil || jobStopped(facts.job)
	}
	if facts.job == nil {
		return false, "", false
	}
	if jobStopped(facts.job) {
		return false, "", false
	}
	switch status := derefStr(facts.job.Status, ""); status {
	case "dead":
		return false, "", false
	case "pending":
		return true, "its job is pending: the box is starting, or waiting for room", false
	default:
		return true, fmt.Sprintf("its job is %s and Nomad is still trying to run it", dash(status)), false
	}
}

// workOnBox names a bake or a publish job that is alive on this box's disk, or
// "". A bake stops the box and can take hours; a publish reads an image from the
// node the disk is on. Either one is a reason to look again next pass.
func workOnBox(id, nodeID string, jobs []*api.JobListStub, allocs []*api.AllocationListStub) string {
	live := map[string]int{}
	for _, alloc := range allocs {
		if alloc == nil || isTerminalAlloc(alloc.ClientStatus) {
			continue
		}
		live[alloc.JobID]++
	}
	byID := make(map[string]*api.JobListStub, len(jobs))
	for _, stub := range jobs {
		if stub != nil {
			byID[stub.ID] = stub
		}
	}
	for _, stub := range jobs {
		if stub == nil || live[stub.ID] == 0 {
			continue
		}
		if stub.ID == BoxBakeJobName(id) || (stub.Meta[boxWorkMarker] == "bake" && stub.Meta["box_id"] == id) {
			return fmt.Sprintf("a bake of it is running (job %s)", stub.ID)
		}
	}
	for _, alloc := range allocs {
		if alloc == nil || isTerminalAlloc(alloc.ClientStatus) || alloc.NodeID != nodeID {
			continue
		}
		if stub := byID[alloc.JobID]; stub != nil && stub.Meta[boxWorkMarker] == "publish" {
			return fmt.Sprintf("a publish on its node is running (job %s)", alloc.JobID)
		}
	}
	return ""
}

// deleteReapedBox deletes a box whose term ran out: what `box rm` does, minus the
// question, and the two records of the box -- its description and its clock --
// with the disk.
func (a *App) deleteReapedBox(ctx context.Context, id, allocID string) error {
	box := BoxRecord{ID: id, Volume: BoxVolumeName(id), AllocationID: allocID}
	if _, err := a.deleteBoxVolumes(ctx, box, reapDeleteTimeout); err != nil {
		return err
	}
	// The description goes with the disk: a description without a disk is a box
	// `box start` cannot run.
	if _, err := a.forgetBoxSpec(ctx, id); err != nil {
		return partialf("the disk of box %s was deleted, but its description was not: %s", id, errOr(err))
	}
	// The clock is the pass's own claim; it was deleted before the disk went, and
	// deleting it again is how a clock left by an interrupted pass goes.
	if _, err := a.clearBoxClock(ctx, id); err != nil {
		return partialf("the disk of box %s was deleted, but its clock was not: %s", id, errOr(err))
	}
	return nil
}
