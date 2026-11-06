package cli

import (
	"context"
	"sync"
	"time"
)

// BoxActivity is what the guest of a running box says about its turns: whether one
// runs now, and how long since the last one ended.
//
// A box is idle most of the time -- an agent waits for its model -- so this is
// what tells a box worth stopping from one at work. Nothing acts on it here: the
// CLI stops no box, ever.
type BoxActivity struct {
	Busy        bool  `json:"busy"`
	IdleSeconds int64 `json:"idle_seconds"`
}

// activityResult is one box's answer: the guest's report, or why there is none.
type activityResult struct {
	activity *BoxActivity
	err      error
}

// guestActivities asks the guest of every running box what it is doing.
//
// The calls run in parallel, each under the guest call bound, so a table of ten
// boxes costs one bound and not ten: a guest out of memory does not answer, and
// ten bounds in a row would be a command nobody waits for. A box that is not
// running is not asked (it has no answer to give), and a boxd that predates the
// report answers without an activity.
func (a *App) guestActivities(ctx context.Context, boxes []BoxRecord) map[string]*activityResult {
	out := make(map[string]*activityResult, len(boxes))
	running := make([]BoxRecord, 0, len(boxes))
	for _, box := range boxes {
		if box.State == "running" {
			running = append(running, box)
		}
	}
	if len(running) == 0 {
		return out
	}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, box := range running {
		wg.Add(1)
		go func(box BoxRecord) {
			defer wg.Done()
			res := a.askActivity(ctx, box)
			mu.Lock()
			out[box.ID] = res
			mu.Unlock()
		}(box)
	}
	wg.Wait()
	return out
}

// askActivity asks one box's guest what it is doing.
func (a *App) askActivity(ctx context.Context, box BoxRecord) *activityResult {
	res := &activityResult{}
	client, err := a.boxctl(ctx, box)
	if err != nil {
		res.err = err
		return res
	}
	err = a.guestCall(ctx, "`boxctl info`", func(callCtx context.Context) error {
		info, err := client.Info(callCtx)
		if err != nil {
			return err
		}
		res.activity = info.Activity
		return nil
	})
	if err != nil {
		res.err = err
	}
	return res
}

// idleCell renders how long a box has been idle, or that a turn runs now: "idle
// 3h12m", "busy", "-" for a box that is not running or a boxd that does not
// report it, and "?" for a guest that did not answer.
func idleCell(box BoxRecord) string {
	return activityCell(box.Activity, box.GuestError)
}

// activityCell is idleCell for a report and the reason there is none.
func activityCell(activity *BoxActivity, guestErr string) string {
	switch {
	case activity != nil && activity.Busy:
		return "busy"
	case activity != nil:
		return "idle " + span(time.Duration(activity.IdleSeconds)*time.Second)
	case guestErr != "":
		return "?"
	default:
		return "-"
	}
}
