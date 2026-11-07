package cli

// The term of a box that is not running: one value for the whole cluster.
//
// It is a Nomad Variable, nomad/jobs/box-reap, item stopped_term, and not an
// environment variable: `box ls`, `box status` and the periodic pass run on
// different hosts, and they must agree on one number. The variable is its own,
// like nomad/jobs/box-store, so the pass reads one path and the operator CLI
// writes it.
//
// No variable, or a variable without the item, means the default of
// boxStoppedTermDefault. Anything else that cannot be used -- a variable that
// cannot be read, an item that is not a Go duration, or one below
// boxStoppedTermMin -- is an error and never the default: a cluster set to 7
// days must not have its disks deleted after 24 hours because a read failed.
//
// The pass reads the term once, at the start, and uses it for every box in that
// pass. A change applies at once to boxes already stopped, because the term is
// compared with the recorded time of the clock (nomad/jobs/box-<id>/stopped):
// lowering it means the disks past the new term go at the next pass.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// boxStoppedTermDefault is how long the disk of a box that is not running is
	// kept when the operator has not set another term. It is the default of the
	// cluster-wide setting, and the only place the default value is written.
	boxStoppedTermDefault = 24 * time.Hour
	// boxStoppedTermMin is the shortest term the setting accepts. A typo such as
	// "1m" would otherwise delete the disks of boxes stopped a moment ago.
	boxStoppedTermMin = time.Hour
	// boxReapVariable is the Nomad Variable that holds the settings of the reap
	// pass. It is one variable for the whole cluster.
	boxReapVariable = "nomad/jobs/box-reap"
	// boxReapTermItem is the item of boxReapVariable that holds the term, a Go
	// duration such as "48h".
	boxReapTermItem = "stopped_term"
)

// boxStoppedTermSetting returns the cluster's term, and whether the operator set
// it. A missing variable and a missing item are the default and are not
// failures; an unreadable or unusable one is an error.
func (a *App) boxStoppedTermSetting(ctx context.Context) (time.Duration, bool, error) {
	v, err := a.Nomad.VariableRead(ctx, boxReapVariable)
	if err != nil {
		if isNotFoundErr(err) {
			return boxStoppedTermDefault, false, nil
		}
		return 0, false, wrapf(err, "cannot read %s, the cluster's stopped-box term%s",
			boxReapVariable, stoppedTermDenied(err, false))
	}
	if v == nil {
		return boxStoppedTermDefault, false, nil
	}
	text, ok := v.Items[boxReapTermItem]
	if !ok {
		return boxStoppedTermDefault, false, nil
	}
	term, err := parseBoxStoppedTerm(text)
	if err != nil {
		return 0, false, errorf("%s holds the item %q, and it is not a usable term: %s",
			boxReapVariable, boxReapTermItem, err)
	}
	return term, true, nil
}

// readBoxStoppedTerm returns the cluster's term, or the reason it cannot be
// used. It never falls back to the default for a value that is there and wrong.
func (a *App) readBoxStoppedTerm(ctx context.Context) (time.Duration, error) {
	term, _, err := a.boxStoppedTermSetting(ctx)
	return term, err
}

// parseBoxStoppedTerm parses one value of the setting: a Go duration of at least
// one hour.
func parseBoxStoppedTerm(text string) (time.Duration, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, errorf("it is empty: give a Go duration such as 48h")
	}
	term, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, errorf("%q is not a Go duration such as 48h", trimmed)
	}
	if term < boxStoppedTermMin {
		return 0, errorf("%q is less than one hour, and a shorter term would delete the disks of boxes stopped "+
			"a moment ago; give 1h or more", trimmed)
	}
	return term, nil
}

// formatBoxStoppedTerm renders a term as the value of the setting: "48h" for a
// whole number of hours, "90m" for a whole number of minutes, and the Go form
// otherwise.
func formatBoxStoppedTerm(term time.Duration) string {
	switch {
	case term%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(term/time.Hour))
	case term%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(term/time.Minute))
	default:
		return term.String()
	}
}

// boxTerm is the cluster's term as a command that only shows it read it. When
// why is not empty the term is unknown, and a display command says so: it does
// not fail, and it does not invent the default.
type boxTerm struct {
	term time.Duration
	why  string
	// read says whether the setting was read at all. The zero value is unknown,
	// so a command that never read it cannot accidentally print a term of zero.
	read bool
}

func (t boxTerm) known() bool { return t.read && t.why == "" }

// readBoxTerm reads the term for a command that only displays it.
func (a *App) readBoxTerm(ctx context.Context) boxTerm {
	term, err := a.readBoxStoppedTerm(ctx)
	if err != nil {
		return boxTerm{why: oneLine(err.Error(), 240), read: true}
	}
	return boxTerm{term: term, read: true}
}

// readBoxTermIfStopped reads the term only when the listing has a box that is not
// running: a command that shows only running boxes prints no term, so it makes no
// read for one.
func (a *App) readBoxTermIfStopped(ctx context.Context, boxes []BoxRecord) boxTerm {
	for _, box := range boxes {
		if box.State != "running" {
			return a.readBoxTerm(ctx)
		}
	}
	return boxTerm{}
}

// termWords renders a term the way a sentence reads it: "24 hours", "48 hours",
// "90 minutes".
func termWords(term time.Duration) string {
	hours := int(term / time.Hour)
	minutes := int((term % time.Hour) / time.Minute)
	var parts []string
	if hours > 0 {
		parts = append(parts, countWords(hours, "hour"))
	}
	if minutes > 0 {
		parts = append(parts, countWords(minutes, "minute"))
	}
	if len(parts) == 0 {
		return term.String()
	}
	return strings.Join(parts, " ")
}

func countWords(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// stoppedTermDenied adds to a refused read or write of the setting what the
// token lacks. Nomad answers a bare "Permission denied" and never names the
// missing rule.
func stoppedTermDenied(err error, write bool) string {
	if !isPermissionDeniedErr(err) {
		return ""
	}
	if write {
		return fmt.Sprintf(" -- the Nomad token needs the variables capabilities read, write and destroy on %q; "+
			"deploy/acl/box-operator.policy.hcl grants them", boxReapVariable)
	}
	return fmt.Sprintf(" -- the Nomad token needs the variables capability read on %q; "+
		"deploy/acl/box-reap.policy.hcl grants it to the pass, deploy/acl/box-operator.policy.hcl to the CLI",
		boxReapVariable)
}
