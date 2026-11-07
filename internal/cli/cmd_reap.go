package cli

// `box reap`: one pass over the cluster that deletes the disks of boxes whose
// term ran out. The periodic Nomad job reap-boxes runs the same pass on a node
// (deploy/reap-boxes.nomad.hcl); this command is for a person, and for a cluster
// whose job is not registered yet.

import (
	"context"
	"fmt"
	"time"
)

// cmdReap runs one pass, or shows what a pass would delete.
func (a *App) cmdReap(ctx context.Context, args []string) error {
	fs := newFlagSet("reap")
	dryRun := fs.Bool("dry-run", false, "list what a pass would delete and when, and delete nothing")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	return a.RunReap(ctx, *dryRun)
}

// RunReap runs one pass and writes what it did to ErrOut. It is what `box reap`
// does, and what the periodic job of the driver calls: the task log of that job
// is the record of every disk the pass deleted.
func (a *App) RunReap(ctx context.Context, dryRun bool) error {
	res, err := a.ReapOnce(ctx, dryRun)
	if err != nil {
		return err
	}
	if a.JSON {
		if err := a.printJSON(res); err != nil {
			return err
		}
	} else {
		a.printReap(res)
	}
	// A box that failed is a failure of the command, and of the task: the pass
	// went over the other boxes first.
	if res.Failed > 0 {
		return errorf("%d box(es) could not be deleted; the lines above say why, and the next pass tries again", res.Failed)
	}
	return nil
}

// printReap writes what a pass did: one line per box that was deleted, that
// failed, or that the pass had to leave alone, then the count.
func (a *App) printReap(res reapResult) {
	for _, box := range res.Boxes {
		switch box.Outcome {
		case reapDeleted:
			a.note("deleted box %s: %s, stopped %s ago", box.ID, reapSize(box), reapStoppedFor(box))
		case reapDue:
			a.note("box %s: %s, stopped %s ago: this pass would delete it", box.ID, reapSize(box), reapStoppedFor(box))
		case reapStarted:
			a.note("box %s: %s, not running; this pass starts its term, and its disk goes at %s",
				box.ID, reapSize(box), box.RemovesAt)
		case reapWaiting:
			a.note("box %s: %s, stopped %s ago: its disk goes at %s", box.ID, reapSize(box), reapStoppedFor(box), box.RemovesAt)
		case reapFailed:
			a.note("box %s: its disk could not be deleted: %s", box.ID, box.Note)
		case reapKept:
			a.note("box %s: kept: %s", box.ID, box.Note)
		}
	}
	if res.DryRun {
		a.note("nothing was deleted (--dry-run)")
		return
	}
	running, waiting, started := reapCounts(res)
	a.note("pass over %s: %d deleted, %d waiting for the term, %d not seen before, %d running or starting",
		describeCount(len(res.Boxes), "box disk"), res.Deleted, waiting, started, running)
}

// reapCounts counts the outcomes of a pass.
func reapCounts(res reapResult) (running, waiting, started int) {
	for _, box := range res.Boxes {
		switch box.Outcome {
		case reapRunning:
			running++
		case reapWaiting:
			waiting++
		case reapStarted:
			started++
		}
	}
	return running, waiting, started
}

// reapSize renders the size of a box's disk for a line of the log.
func reapSize(box reapedBox) string {
	switch {
	case box.DiskGiB > 0:
		return fmt.Sprintf("%d GiB of disk", box.DiskGiB)
	case box.DiskBytes > 0:
		return humanBytes(box.DiskBytes) + " of disk"
	}
	return "its disk"
}

// reapStoppedFor renders how long a box had been stopped, the short way.
func reapStoppedFor(box reapedBox) string {
	if box.StoppedForSeconds <= 0 {
		return "a moment"
	}
	return humanDuration(time.Duration(box.StoppedForSeconds) * time.Second)
}
