package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// waitPollInterval is how often a wait asks Nomad for the new state.
const waitPollInterval = 2 * time.Second

// waitForBoxRunning blocks until the box runs, fails, or the timeout expires.
//
// evalID is the evaluation the registration returned; it is used to explain a
// placement failure in plain words instead of leaving the user with a job that
// silently does nothing.
func (a *App) waitForBoxRunning(ctx context.Context, id, evalID string, timeout time.Duration) (BoxRecord, error) {
	deadline := a.now().Add(timeout)
	start := a.now()
	evalChecked := false
	var (
		last        BoxRecord
		runningFrom time.Time
	)
	for {
		rec, _, err := a.findBox(ctx, id)
		if err != nil {
			return last, err
		}
		last = rec
		switch rec.State {
		case "running":
			// A box that has just come up may still be restarted by Nomad (a
			// resize stops and starts the VM, a deployment replaces the
			// allocation). Report it as running only once it stays running
			// across one poll, so the next command does not find it "starting".
			if runningFrom.IsZero() {
				runningFrom = a.now()
			} else if a.now().Sub(runningFrom) >= waitPollInterval {
				pending, err := a.pendingAllocs(ctx, id)
				if err == nil && !pending {
					return rec, nil
				}
			}
		case "failed":
			return rec, a.describeBoxFailure(ctx, rec)
		default:
			runningFrom = time.Time{}
		}
		if rec.AllocationID == "" && !evalChecked && a.now().Sub(start) >= 10*time.Second {
			evalChecked = true
			if reason := a.evalBlocker(ctx, evalID); reason != "" {
				return rec, errorf("Nomad could not place the box: %s", reason)
			}
		}
		if !a.now().Before(deadline) {
			return rec, timeoutf("the box %s did not start within %s (it is %s); `box status %s` shows what Nomad reports, `box logs %s -f` shows the guest",
				id, timeout.Round(time.Second), rec.State, id, id)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return rec, wrapf(err, "stopped waiting for the box %s", id)
		}
	}
}

// pendingAllocs reports whether the job of a box still has an allocation that
// has not started.
func (a *App) pendingAllocs(ctx context.Context, id string) (bool, error) {
	byJob, err := a.jobAllocations(ctx)
	if err != nil {
		return false, err
	}
	for _, alloc := range byJob[BoxJobName(id)] {
		if alloc == nil || isTerminalAlloc(alloc.ClientStatus) {
			continue
		}
		if alloc.ClientStatus != "running" {
			return true, nil
		}
	}
	return false, nil
}

// guestWaitTimeout is how long a guest command waits for a box that is on its
// way up (a resize or a restart) before it gives up.
const guestWaitTimeout = 2 * time.Minute

// waitRunning waits until a box runs.
//
// A box that is stopped or failed is reported at once: nothing is going to
// change by waiting. A box that is starting is given guestWaitTimeout, because
// the coordinator asked for a guest command while the driver was still working.
func (a *App) waitRunning(ctx context.Context, box BoxRecord, timeout time.Duration) (BoxRecord, error) {
	if box.State == "running" {
		return box, nil
	}
	switch box.State {
	case "stopped", "failed":
		return box, notRunning(box)
	}
	deadline := a.now().Add(timeout)
	for {
		if !a.now().Before(deadline) {
			return box, timeoutf("the box %s is still %s after %s; `box status %s` shows what Nomad reports",
				box.ID, box.State, timeout.Round(time.Second), box.ID)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return box, wrapf(err, "stopped waiting for the box %s", box.ID)
		}
		rec, _, err := a.findBox(ctx, box.ID)
		if err != nil {
			return box, err
		}
		box = rec
		if box.State == "running" {
			return box, nil
		}
		switch box.State {
		case "stopped", "failed":
			return box, notRunning(box)
		}
	}
}

// notRunning explains a box the guest cannot be reached in.
func notRunning(box BoxRecord) error {
	detail := ""
	if box.Note != "" {
		detail = " (" + box.Note + ")"
	}
	return errorf("the box %s is %s, not running%s; `box logs %s -f` and `box status %s` show what the guest and Nomad report",
		box.ID, box.State, detail, box.ID, box.ID)
}

// evalBlocker turns a placement failure of an evaluation into plain words. An
// empty string means "no placement failure the evaluation knows about".
func (a *App) evalBlocker(ctx context.Context, evalID string) string {
	if evalID == "" {
		return ""
	}
	eval, err := a.Nomad.Evaluation(ctx, evalID)
	if err != nil || eval == nil {
		return ""
	}
	return describeEval(eval)
}

// describeEval explains why the scheduler could not place a job.
func describeEval(eval *api.Evaluation) string {
	if eval == nil {
		return ""
	}
	var parts []string
	if eval.Status != "complete" && eval.Status != "" {
		parts = append(parts, fmt.Sprintf("the scheduler is still working on this placement (evaluation %s)", eval.Status))
	}
	if eval.StatusDescription != "" && eval.Status == "failed" {
		parts = append(parts, oneLine(eval.StatusDescription, 200))
	}
	if len(eval.FailedTGAllocs) == 0 {
		return strings.Join(parts, "; ")
	}
	groups := make([]string, 0, len(eval.FailedTGAllocs))
	for group := range eval.FailedTGAllocs {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		metric := eval.FailedTGAllocs[group]
		if metric == nil {
			continue
		}
		if text := describeMetric(metric); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// describeMetric turns one allocation metric into plain words.
func describeMetric(metric *api.AllocationMetric) string {
	var parts []string
	for _, entry := range sortedCounts(metric.ClassFiltered) {
		parts = append(parts, fmt.Sprintf("%d node(s) lack %s", entry.count, describeClass(entry.name)))
	}
	for _, entry := range sortedCounts(metric.ConstraintFiltered) {
		parts = append(parts, fmt.Sprintf("%d node(s) were filtered out by the constraint %s", entry.count, entry.name))
	}
	for _, entry := range sortedCounts(metric.DimensionExhausted) {
		parts = append(parts, fmt.Sprintf("%d node(s) ran out of %s", entry.count, entry.name))
	}
	for _, entry := range sortedCounts(metric.ClassExhausted) {
		parts = append(parts, fmt.Sprintf("%d node(s) of class %s have no room left", entry.count, entry.name))
	}
	for _, name := range sortedKeys(metric.ResourcesExhausted) {
		parts = append(parts, fmt.Sprintf("there is no node left with enough %s", name))
	}
	switch {
	case metric.NodesEvaluated == 0:
		parts = append(parts, "no node was evaluated: the cluster may have no client at all")
	case len(metric.NodesAvailable) > 0:
		dcs := make([]string, 0, len(metric.NodesAvailable))
		for dc, n := range metric.NodesAvailable {
			dcs = append(dcs, fmt.Sprintf("%d in %s", n, dc))
		}
		sort.Strings(dcs)
		parts = append(parts, fmt.Sprintf("%d node(s) were evaluated (%s)", metric.NodesEvaluated, strings.Join(dcs, ", ")))
	default:
		parts = append(parts, fmt.Sprintf("%d node(s) were evaluated", metric.NodesEvaluated))
	}
	return strings.Join(parts, ", ")
}

// describeClass explains a scheduler class in words a person understands.
func describeClass(name string) string {
	switch name {
	case "box", "driver.box":
		return "the box driver"
	case boxVolumePluginID:
		return "the " + boxVolumePluginID + " host volume plugin"
	default:
		return "the " + name + " driver"
	}
}

type namedCount struct {
	name  string
	count int
}

func sortedCounts(m map[string]int) []namedCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]namedCount, 0, len(m))
	for name, count := range m {
		out = append(out, namedCount{name: name, count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describeBoxFailure explains why a box could not start, from the task events
// Nomad recorded.
func (a *App) describeBoxFailure(ctx context.Context, box BoxRecord) error {
	if box.AllocationID == "" {
		return errorf("the box %s failed before Nomad created an allocation", box.ID)
	}
	alloc, err := a.Nomad.Allocation(ctx, box.AllocationID)
	if err != nil {
		return wrapf(err, "the box %s failed (allocation %s)", box.ID, box.AllocationID)
	}
	if text := allocFailureText(alloc); text != "" {
		return errorf("the box %s failed: %s (`box status %s` shows the full task events)",
			box.ID, text, box.ID)
	}
	return errorf("the box %s failed (allocation %s is %s)", box.ID, alloc.ID, alloc.ClientStatus)
}

// allocFailureText returns the last useful message of a failed allocation.
func allocFailureText(alloc *api.Allocation) string {
	if alloc == nil {
		return ""
	}
	task := alloc.TaskStates[boxTaskName]
	if task == nil {
		return oneLine(alloc.ClientDescription, 300)
	}
	// The driver's own words first: a task that fails ends with Nomad's "Not
	// Restarting: Policy allows no restarts", which says what Nomad did and not
	// why the work stopped. The driver's event is the one that names the cause
	// ("proj-1 amd64 is published in this store, and a published version never
	// changes"), so it wins over anything Nomad adds after it.
	for i := len(task.Events) - 1; i >= 0; i-- {
		event := task.Events[i]
		if event == nil || event.Type != "Driver" {
			continue
		}
		if v := strings.TrimSpace(event.Details["message"]); v != "" {
			return oneLine(v, 300)
		}
		if event.DisplayMessage != "" {
			return oneLine(event.DisplayMessage, 300)
		}
	}
	for i := len(task.Events) - 1; i >= 0; i-- {
		event := task.Events[i]
		if event == nil {
			continue
		}
		for _, key := range []string{"error", "message", "reason"} {
			if v := event.Details[key]; strings.TrimSpace(v) != "" {
				return oneLine(event.Type+": "+v, 300)
			}
		}
		if event.DisplayMessage != "" {
			return oneLine(event.DisplayMessage, 300)
		}
		if event.Message != "" {
			return oneLine(event.Message, 300)
		}
	}
	return oneLine(alloc.ClientDescription, 300)
}

// taskEvents returns the recent task events of an allocation, oldest first.
func taskEvents(alloc *api.Allocation, limit int) []*api.TaskEvent {
	if alloc == nil {
		return nil
	}
	task := alloc.TaskStates[boxTaskName]
	if task == nil {
		return nil
	}
	events := task.Events
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events
}

// eventTime renders a task event timestamp.
func eventTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(0, ts).UTC().Format(time.RFC3339)
}
