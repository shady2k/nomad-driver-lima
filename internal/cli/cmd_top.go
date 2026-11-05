package cli

import (
	"context"
	"time"
)

// topEvery is how often box top -f prints; boxd measures every five seconds.
const topEvery = 5 * time.Second

// topOutput is the JSON document of box top.
type topOutput struct {
	Box   string      `json:"box"`
	Usage *GuestUsage `json:"usage"`
	// GuestError explains why the guest did not answer. It is set only when the
	// guest ran out of its call bound, and the command then exits 4.
	GuestError string `json:"guest_error,omitempty"`
}

// cmdTop prints what the guest is doing, once or, with -f, every few seconds.
func (a *App) cmdTop(ctx context.Context, args []string) error {
	fs := newFlagSet("top")
	follow := fs.Bool("f", false, "print it again every five seconds until interrupted")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	for {
		var info *BoxInfo
		err := a.guestCall(ctx, "`boxctl info`", func(callCtx context.Context) error {
			got, err := client.Info(callCtx)
			if err != nil {
				return err
			}
			info = got
			return nil
		})
		if err != nil {
			if isGuestSilent(err) {
				return a.guestSilentFailure(id, err, topOutput{Box: id, GuestError: err.Error()})
			}
			return wrapf(err, "box %s did not say what it is doing", id)
		}
		if info.Usage == nil {
			return errorf("box %s runs a boxd that does not measure its load; recreate it on a newer image", id)
		}
		if a.JSON {
			if err := a.printJSON(topOutput{Box: id, Usage: info.Usage}); err != nil {
				return err
			}
		} else {
			a.note("%s  %s", shortTime(info.Usage.At), info.Usage.Line())
			a.printGuestProcesses(info.Usage)
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(topEvery):
		}
	}
}

// printGuestProcesses prints what the guest's CPU and memory are going to, under
// the line that says how much of them there is:
//
//   - the work running with no turn in progress, when there is any: a box whose
//     earlier turn left a build behind has a core taken and nothing to show for
//     it in its sessions;
//   - the busiest processes by CPU, always: a summary line alone says the box is
//     busy and not what makes it busy, which is what `box top` is asked for;
//   - a process that has held a whole core for ten minutes, which boxd also said
//     in the journal.
func (a *App) printGuestProcesses(u *GuestUsage) {
	if line := u.Leftover.Summary(); line != "" {
		a.note("  %s", line)
	}
	if len(u.Processes) > 0 {
		a.note("  the busiest processes:")
		for _, p := range u.Processes {
			a.note("    %s", p.Line())
		}
	}
	for _, h := range u.Hogs {
		a.note("  %s", h.Line())
	}
}

// guestSilentFailure ends a command whose guest did not answer: it prints doc
// as the JSON document when --json was asked for (so a script reads the same
// fact from a field) and returns the timeout exit code with a sentence that
// says what to look at.
func (a *App) guestSilentFailure(id string, err error, doc any) error {
	if a.JSON {
		if jsonErr := a.printJSON(doc); jsonErr != nil {
			return jsonErr
		}
	}
	return timeoutf("box %s: %v; `box logs %s` shows the guest journal and `box status %s` what Nomad knows about the box",
		id, err, id, id)
}

// shortTime is an RFC 3339 time as hh:mm:ss UTC.
func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("15:04:05")
}
