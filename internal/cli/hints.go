package cli

// The hints that tell an agent when to give a box back.
//
// Boxes are never stopped automatically: several projects share the nodes, and a
// box is somebody's work. The tools therefore say, in the moments an agent
// decides, which boxes are its own and what stopping one costs -- a line on
// stderr, and nothing else. No exit code changes and no box is touched here.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// idleBox is one box that holds its node's memory with no turn running in it.
type idleBox struct {
	// Record is the box as `box ls` shows it.
	Record BoxRecord
	// Idle is how long it has been idle.
	Idle time.Duration
}

// idleBoxes picks the boxes whose guest said it is doing nothing, longest idle
// first.
//
// A box is idle when its guest says so: a box that is not running has no turn to
// run, a guest that did not answer said nothing, and a boxd too old to report its
// activity is not guessed at. An idle box is a box somebody can stop; naming one
// that is at work would be advice to interrupt a turn.
func idleBoxes(boxes []BoxRecord, activities map[string]*activityResult) []idleBox {
	out := make([]idleBox, 0, len(boxes))
	for _, box := range boxes {
		res, ok := activities[box.ID]
		if !ok || res == nil || res.err != nil || res.activity == nil || res.activity.Busy {
			continue
		}
		out = append(out, idleBox{Record: box, Idle: time.Duration(res.activity.IdleSeconds) * time.Second})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Idle != out[j].Idle {
			return out[i].Idle > out[j].Idle
		}
		return out[i].Record.ID < out[j].Record.ID
	})
	return out
}

// idleHintSpan renders how long a box has been idle the short way an idle list
// reads it: "2h", "3h12m", "40m". A box that answered idle is idle at least that
// long, and one that has just started says "1m" rather than "0m".
func idleHintSpan(d time.Duration) string {
	minutes := int(d.Round(time.Minute).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	hours, rest := minutes/60, minutes%60
	switch {
	case hours == 0:
		return fmt.Sprintf("%dm", rest)
	case rest == 0:
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, rest)
}

// idleBoxList renders one group of idle boxes: "a 2h, b 40m".
func idleBoxList(boxes []idleBox) string {
	parts := make([]string, 0, len(boxes))
	for _, box := range boxes {
		parts = append(parts, box.Record.ID+" "+idleHintSpan(box.Idle))
	}
	return strings.Join(parts, ", ")
}

// noteIdleBoxes names the idle boxes on the nodes a new box could go to, grouped
// by whether they are the caller's to stop.
//
// project is the project the new box would have, so a box of the same project is
// "yours": it is the one an agent may stop. Nothing is refused for a box of
// another project, and nothing is stopped here either -- the second group says
// so, so an agent neither stops another project's box nor believes it cannot be
// fixed at all.
//
// It asks every running box's guest in parallel under the same bound `box ls`
// uses, so it costs one bound and not one per box, and a failure of any kind
// leaves the list out rather than delaying a refusal past that bound.
func (a *App) noteIdleBoxes(ctx context.Context, project, arch string) {
	if a.ErrOut == nil {
		return
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return
	}
	want := NormalizeArch(arch)
	candidates := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if want != "" && node.Arch != want {
			continue
		}
		candidates[node.NodeID] = true
	}
	if len(candidates) == 0 {
		return
	}
	boxes, err := a.collectBoxes(ctx)
	if err != nil {
		return
	}
	here := make([]BoxRecord, 0, len(boxes))
	for _, box := range boxes {
		if box.State != "running" || !candidates[box.NodeID] {
			continue
		}
		here = append(here, box)
	}
	if len(here) == 0 {
		return
	}
	idle := idleBoxes(here, a.guestActivities(ctx, here))
	if len(idle) == 0 {
		return
	}
	var mine, others []idleBox
	for _, box := range idle {
		if project != "" && box.Record.Project == project {
			mine = append(mine, box)
			continue
		}
		others = append(others, box)
	}
	if len(mine) > 0 {
		a.noteWait("your idle boxes: %s (box stop <id>)", idleBoxList(mine))
	}
	if len(others) > 0 {
		// Without a project of the caller's own, nothing is known to be its box,
		// and the list is simply the idle boxes there are.
		if project == "" {
			a.noteWait("idle boxes: %s", idleBoxList(others))
			return
		}
		a.noteWait("other idle boxes: %s (not yours to stop)", idleBoxList(others))
	}
}
