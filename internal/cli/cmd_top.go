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
		info, err := client.Info(ctx)
		if err != nil {
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

// shortTime is an RFC 3339 time as hh:mm:ss UTC.
func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("15:04:05")
}
