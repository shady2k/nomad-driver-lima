package cli

// The clock of a box that is not running.
//
// A box is for one task: the agent hands its results over through git and deletes
// the box itself with `box rm <id> --yes`. Stopping a box is a short pause that
// frees memory, never a way to keep a disk for later. The backstop for an agent
// that forgot is the pass `box reap` runs: a box that is not running has its disk
// deleted after the cluster's stopped-box term, which is 24 hours unless the
// operator set another (nomad/jobs/box-reap, see reapterm.go).
//
// The clock is a Nomad Variable, nomad/jobs/box-<id>/stopped, because Nomad's job
// garbage collection deletes a dead job and its allocations: the time has to
// outlive them, and it must be visible to every command without a daemon. Nomad
// Variables have no TTL, so nothing here expires by itself; the term is the
// comparison `now - stopped_at >= the cluster's term`, and a pass that cannot
// read the clock deletes nothing.
//
// Every way a box stops is covered because the clock is written by looking at
// Nomad, not by `box stop`: `box stop`, `nomad job stop`, a crash and the stop a
// bake makes all leave a job that is stopped, dead or gone. The disk can
// therefore only ever go late, never early.
//
// `box start` deletes the clock before it registers the job again, and so does a
// pass that sees the box running: a box that comes back is not on a term any
// more.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

const (
	// boxStoppedItem is the item of the clock variable that holds the time.
	boxStoppedItem = "stopped_at"
	// boxStoppedStateItem is the item that says whether a pass owns the box: a
	// pass that is deleting the disk marks the clock "deleting" before it does,
	// and a `box start` refuses such a box instead of registering it under the
	// deletion. That check-and-set is what closes the race as far as Nomad
	// allows, because Nomad has no transaction across a variable and a volume.
	boxStoppedStateItem = "state"
	// boxStoppedStateStopped is a clock no pass is acting on.
	boxStoppedStateStopped = "stopped"
	// boxStoppedStateDeleting is a clock a pass has claimed: the disk is going.
	boxStoppedStateDeleting = "deleting"
)

// BoxStoppedVariable returns the Nomad Variable path that holds the time a box
// was first seen not running.
func BoxStoppedVariable(id string) string {
	return boxSpecVariablePrefix + BoxJobName(id) + "/stopped"
}

// boxClock is what the clock of one box says.
type boxClock struct {
	// at is when a pass first saw the box not running.
	at time.Time
	// modifyIndex is the index the variable had when it was read, which is what
	// a checked write or delete has to match.
	modifyIndex uint64
	// deleting is true when a pass has claimed the clock and is deleting the
	// disk now: `box start` refuses such a box.
	deleting bool
}

// readBoxClock reads the clock of a box. A box no pass has seen not running has
// none, which is (boxClock{}, false, nil): a missing clock is a state, not a
// failure.
//
// A clock that cannot be read as a time is an error and not a missing clock: a
// pass that took it for missing would start a new term, and a pass that took it
// for zero would delete the disk at once.
func (a *App) readBoxClock(ctx context.Context, id string) (boxClock, bool, error) {
	path := BoxStoppedVariable(id)
	v, err := a.Nomad.VariableRead(ctx, path)
	if err != nil {
		if isConfirmedNotFound(err) {
			return boxClock{}, false, nil
		}
		return boxClock{}, false, wrapf(err, "cannot read %s, the clock of box %s%s", path, id, variableDenied(err, path))
	}
	if v == nil {
		return boxClock{}, false, nil
	}
	state := strings.TrimSpace(v.Items[boxStoppedStateItem])
	if state != "" && state != boxStoppedStateStopped && state != boxStoppedStateDeleting {
		return boxClock{}, false, errorf("%s holds the state %q, so the clock of box %s cannot be read: delete the "+
			"variable (`nomad var delete %s`) and the next pass starts its term again",
			path, state, id, path)
	}
	text := strings.TrimSpace(v.Items[boxStoppedItem])
	if text == "" {
		return boxClock{}, false, errorf("%s holds no %q item, so the clock of box %s cannot be read: delete the "+
			"variable (`nomad var delete %s`) and the next pass starts its term again", path, boxStoppedItem, id, path)
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return boxClock{}, false, errorf("%s holds %q, which is not an RFC3339 time, so the clock of box %s cannot be "+
			"read: delete the variable (`nomad var delete %s`) and the next pass starts its term again",
			path, text, id, path)
	}
	return boxClock{at: at, modifyIndex: v.ModifyIndex, deleting: state == boxStoppedStateDeleting}, true, nil
}

// startBoxClock writes the clock of a box that no pass had seen not running. It
// creates the variable and never replaces one: a clock that is already there is
// the earlier pass's, and moving it would give the box a longer term than the
// first pass did.
func (a *App) startBoxClock(ctx context.Context, id string, at time.Time) (boxClock, error) {
	path := BoxStoppedVariable(id)
	value := at.UTC().Format(time.RFC3339)
	v, err := a.Nomad.VariableCreateChecked(ctx, &api.Variable{
		Path:  path,
		Items: map[string]string{boxStoppedItem: value, boxStoppedStateItem: boxStoppedStateStopped},
	})
	if err != nil {
		if isVariableConflict(err) {
			// Another pass wrote it between the read and the write: its time is
			// the one that counts.
			clock, ok, readErr := a.readBoxClock(ctx, id)
			if readErr != nil {
				return boxClock{}, readErr
			}
			if !ok {
				return boxClock{}, wrapf(err, "cannot write %s, the clock of box %s", path, id)
			}
			return clock, nil
		}
		return boxClock{}, wrapf(err, "cannot write %s, the clock of box %s%s", path, id, variableDenied(err, path))
	}
	index := uint64(0)
	if v != nil {
		index = v.ModifyIndex
	}
	return boxClock{at: at.UTC(), modifyIndex: index}, nil
}

// clearBoxClock deletes the clock of a box, and reports whether there was one. A
// clock that is already gone is not an error: every command that clears it is
// idempotent, and `box start` may be the second one to do it.
func (a *App) clearBoxClock(ctx context.Context, id string) (bool, error) {
	path := BoxStoppedVariable(id)
	if err := a.Nomad.VariableDelete(ctx, path); err != nil {
		if isNotFoundErr(err) {
			return false, nil
		}
		return false, wrapf(err, "cannot delete %s, the clock of box %s%s", path, id, variableDenied(err, path))
	}
	return true, nil
}

// claimBoxClock marks the clock of a box as being deleted, only when it is still
// the one a pass read, and reports whether it could.
//
// It is how a pass closes the race with a `box start` as far as Nomad allows. The
// start deletes the clock only with a check on the index it read, so either its
// delete comes first (this claim conflicts, and the pass leaves the disk alone),
// or the claim comes first (the start's delete conflicts, it reads the clock
// again, sees "deleting", and refuses before it registers the job). Nomad has no
// transaction across a variable and a volume, so this is the most the two
// commands can agree on.
//
// A false answer is "somebody else has this box", never an error.
func (a *App) claimBoxClock(ctx context.Context, id string, clock boxClock) (boxClock, bool, error) {
	path := BoxStoppedVariable(id)
	v := &api.Variable{
		Path:        path,
		Items:       map[string]string{boxStoppedItem: clock.at.UTC().Format(time.RFC3339), boxStoppedStateItem: boxStoppedStateDeleting},
		ModifyIndex: clock.modifyIndex,
	}
	written, err := a.Nomad.VariableUpdateChecked(ctx, v)
	if err != nil {
		switch {
		case isVariableConflict(err), isNotFoundErr(err):
			return boxClock{}, false, nil
		default:
			return boxClock{}, false, wrapf(err, "cannot claim %s, the clock of box %s%s", path, id, variableDenied(err, path))
		}
	}
	claimed := clock
	claimed.deleting = true
	if written != nil {
		claimed.modifyIndex = written.ModifyIndex
	}
	return claimed, true, nil
}

// releaseBoxClock gives a claimed clock back to "stopped" when the deletion did
// not finish, so the next pass tries again at the same time instead of leaving
// the disk unclaimed. The update is checked on the claim's own index: a clock
// somebody else deleted is not written back, because a clock nothing owns must
// not be created for a box that may be gone.
func (a *App) releaseBoxClock(ctx context.Context, id string, claimed boxClock) error {
	path := BoxStoppedVariable(id)
	v := &api.Variable{
		Path:        path,
		Items:       map[string]string{boxStoppedItem: claimed.at.UTC().Format(time.RFC3339), boxStoppedStateItem: boxStoppedStateStopped},
		ModifyIndex: claimed.modifyIndex,
	}
	if _, err := a.Nomad.VariableUpdateChecked(ctx, v); err != nil {
		switch {
		case isVariableConflict(err), isNotFoundErr(err):
			return nil
		default:
			return wrapf(err, "cannot write %s back, the clock of box %s%s", path, id, variableDenied(err, path))
		}
	}
	return nil
}

// claimBoxForStart deletes the clock of a box before `box start` registers its
// job, and refuses a box a pass is deleting now.
//
// The delete is checked, so a clock a pass claimed between the read and the
// delete is not deleted: the start reads it again, sees "deleting", and stops
// with a sentence instead of registering a job on a disk that is going.
func (a *App) claimBoxForStart(ctx context.Context, id string) error {
	path := BoxStoppedVariable(id)
	for attempt := 0; attempt < 3; attempt++ {
		clock, seen, err := a.readBoxClock(ctx, id)
		if err != nil {
			return err
		}
		if !seen {
			return nil
		}
		if clock.deleting {
			return errorf("a reap pass is deleting the disk of box %s: it stopped longer ago than the cluster's "+
				"stopped-box term, so `box start %s` cannot bring it back. `box create %s ...` makes a new box",
				id, id, id)
		}
		err = a.Nomad.VariableDeleteChecked(ctx, path, clock.modifyIndex)
		switch {
		case err == nil, isNotFoundErr(err):
			return nil
		case isVariableConflict(err):
			// A pass or another command changed the clock: read it again.
			continue
		default:
			return wrapf(err, "cannot delete %s, the clock of box %s%s", path, id, variableDenied(err, path))
		}
	}
	return errorf("the clock of box %s keeps changing, so `box start %s` did not go on: try it again in a moment", id, id)
}

// isVariableConflict reports whether Nomad refused a write because the variable
// changed since it was read.
func isVariableConflict(err error) bool {
	if err == nil {
		return false
	}
	var conflict api.ErrCASConflict
	return errors.As(err, &conflict)
}
