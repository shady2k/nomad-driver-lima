package cli

// `box reap`: one pass over the cluster that deletes the disks of boxes whose
// term ran out. The periodic Nomad job reap-boxes runs the same pass on a node
// (deploy/reap-boxes.nomad.hcl); this command is for a person, and for a cluster
// whose job is not registered yet.

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/api"
)

// cmdReap runs one pass, shows what a pass would delete, or reads and writes the
// cluster's term.
func (a *App) cmdReap(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "term" {
		return a.cmdReapTerm(ctx, args[1:])
	}
	fs := newFlagSet("reap")
	dryRun := fs.Bool("dry-run", false, "list what a pass would delete and when, and delete nothing")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	return a.RunReap(ctx, *dryRun)
}

// cmdReapTerm shows, sets or removes the cluster's stopped-box term. It is a
// Nomad Variable, so an operator never writes it by hand: a typo in a hand
// written variable is a cluster that deletes disks almost at once.
func (a *App) cmdReapTerm(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.cmdReapTermShow(ctx, nil)
	}
	switch args[0] {
	case "show":
		return a.cmdReapTermShow(ctx, args[1:])
	case "set":
		return a.cmdReapTermSet(ctx, args[1:])
	case "unset":
		return a.cmdReapTermUnset(ctx, args[1:])
	case "help", "-h", "--help":
		a.printReapTermHelp()
		return nil
	}
	return usagef("unknown reap term subcommand %q: use show, set <duration> or unset", args[0])
}

// reapTermOutput is the JSON document of `box reap term`.
type reapTermOutput struct {
	// Term is the term as a Go duration, and TermSeconds the same number in
	// seconds.
	Term        string `json:"term"`
	TermSeconds int64  `json:"term_seconds"`
	// Default is true when the operator has not set a term: the variable or its
	// item is missing, and the term is boxStoppedTermDefault.
	Default bool `json:"default"`
	// Variable and Item name where the setting lives.
	Variable string `json:"variable"`
	Item     string `json:"item"`
}

// cmdReapTermShow prints the cluster's term. Unlike `box ls` and `box status`,
// this command exists to show the setting, so a setting it cannot read is a
// failure and not an unknown term.
func (a *App) cmdReapTermShow(ctx context.Context, args []string) error {
	fs := newFlagSet("reap term")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	term, set, err := a.boxStoppedTermSetting(ctx)
	if err != nil {
		return err
	}
	out := reapTermOutput{Term: formatBoxStoppedTerm(term), TermSeconds: int64(term.Seconds()),
		Default: !set, Variable: boxReapVariable, Item: boxReapTermItem}
	if a.JSON {
		return a.printJSON(out)
	}
	if !set {
		a.note("the stopped-box term is %s, the default: %s holds no %q item",
			termWords(term), boxReapVariable, boxReapTermItem)
		a.note("`box reap term set 48h` sets another; `box reap term unset` returns to the default")
		return nil
	}
	a.note("the stopped-box term is %s (variable %s, item %s)", termWords(term), boxReapVariable, boxReapTermItem)
	a.note("a change applies at once: a box already stopped for longer than the term loses its disk at the next pass")
	return nil
}

// cmdReapTermSet writes the cluster's term.
func (a *App) cmdReapTermSet(ctx context.Context, args []string) error {
	fs := newFlagSet("reap term set")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	term, err := parseBoxStoppedTerm(fs.Arg(0))
	if err != nil {
		return usagef("box reap term set: %s", err)
	}
	value := formatBoxStoppedTerm(term)
	if _, err := a.Nomad.VariableWrite(ctx, &api.Variable{
		Path:  boxReapVariable,
		Items: map[string]string{boxReapTermItem: value},
	}); err != nil {
		return wrapf(err, "Nomad refused the variable %s%s", boxReapVariable, stoppedTermDenied(err, true))
	}
	out := reapTermOutput{Term: value, TermSeconds: int64(term.Seconds()), Variable: boxReapVariable, Item: boxReapTermItem}
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("the stopped-box term is now %s (variable %s, item %s)", termWords(term), boxReapVariable, boxReapTermItem)
	a.note("a change applies at once: a box already stopped for longer than %s loses its disk at the next pass "+
		"(`box reap` runs every 15 minutes, and a lower term deletes the disks past it at that pass)",
		termWords(term))
	return nil
}

// cmdReapTermUnset removes the setting, which returns the cluster to the
// default term.
func (a *App) cmdReapTermUnset(ctx context.Context, args []string) error {
	fs := newFlagSet("reap term unset")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	if err := a.Nomad.VariableDelete(ctx, boxReapVariable); err != nil && !isNotFoundErr(err) {
		return wrapf(err, "Nomad refused to delete the variable %s%s", boxReapVariable, stoppedTermDenied(err, true))
	}
	out := reapTermOutput{Term: formatBoxStoppedTerm(boxStoppedTermDefault),
		TermSeconds: int64(boxStoppedTermDefault.Seconds()), Default: true,
		Variable: boxReapVariable, Item: boxReapTermItem}
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("the stopped-box term is back to the default, %s (%s is deleted)", termWords(boxStoppedTermDefault),
		boxReapVariable)
	a.note("a change applies at once: a box already stopped for longer than %s loses its disk at the next pass",
		termWords(boxStoppedTermDefault))
	return nil
}

// printReapTermHelp is the help of `box reap term`.
func (a *App) printReapTermHelp() {
	fmt.Fprintf(a.ErrOut, "usage: box reap term [show]\n"+
		"       box reap term set <duration>\n"+
		"       box reap term unset\n\n"+
		"The term is how long the disk of a box that is not running is kept: one value\n"+
		"for the whole cluster, in the Nomad variable %s, item %s. It is a Go\n"+
		"duration such as 48h, and at least 1h. No setting means the default, %s.\n\n"+
		"A change applies at once: a box already stopped for longer than the new term\n"+
		"loses its disk at the next pass, and `box reap` runs every 15 minutes.\n",
		boxReapVariable, boxReapTermItem, termWords(boxStoppedTermDefault))
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
