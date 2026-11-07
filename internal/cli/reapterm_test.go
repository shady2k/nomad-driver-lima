package cli

// The stopped-box term is one value for the whole cluster: a Nomad Variable that
// the operator sets with `box reap term`, and that a pass reads once before it
// looks at a box. These tests are the two halves of that promise: a value that
// is set is used everywhere, and a value that cannot be used deletes nothing.

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// setBoxTerm writes the cluster's stopped-box term the way `box reap term set`
// does.
func (ta *testApp) setBoxTerm(t *testing.T, value string) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	ta.cluster.variableSeq++
	ta.cluster.variables[boxReapVariable] = &api.Variable{
		Path:        boxReapVariable,
		Items:       map[string]string{boxReapTermItem: value},
		ModifyIndex: ta.cluster.variableSeq,
		CreateIndex: ta.cluster.variableSeq,
	}
}

// boxTermValue reads the cluster's stopped-box term out of the fake cluster.
func (ta *testApp) boxTermValue(t *testing.T) (string, bool) {
	t.Helper()
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	v, ok := ta.cluster.variables[boxReapVariable]
	if !ok {
		return "", false
	}
	return v.Items[boxReapTermItem], true
}

// callsWith returns the recorded Nomad calls that start with prefix.
func (ta *testApp) callsWith(prefix string) []string {
	ta.cluster.callsMu.Lock()
	defer ta.cluster.callsMu.Unlock()
	var out []string
	for _, call := range ta.cluster.calls {
		if strings.HasPrefix(call, prefix) {
			out = append(out, call)
		}
	}
	return out
}

// TestReapUsesTheDefaultTermWhenNoTermIsSet: no variable, and a variable without
// the item, both mean 24 hours.
func TestReapUsesTheDefaultTermWhenNoTermIsSet(t *testing.T) {
	for _, setting := range []string{"missing", "item missing"} {
		t.Run(setting, func(t *testing.T) {
			ta := newReapApp(t)
			ta.addBox(t, "dev1")
			ta.stopForReap(t, "dev1")
			ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))
			if setting == "item missing" {
				ta.cluster.mu.Lock()
				ta.cluster.variableSeq++
				ta.cluster.variables[boxReapVariable] = &api.Variable{
					Path: boxReapVariable, Items: map[string]string{"other": "x"},
					ModifyIndex: ta.cluster.variableSeq, CreateIndex: ta.cluster.variableSeq,
				}
				ta.cluster.mu.Unlock()
			}

			require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "deleted box dev1")
			ta.cluster.mu.Lock()
			defer ta.cluster.mu.Unlock()
			require.Empty(t, ta.cluster.volumes, "25 hours is past the default 24 hours")
		})
	}
}

// TestReapUsesTheClusterTerm: the term the operator set is what the pass compares
// with, both ways.
func TestReapUsesTheClusterTerm(t *testing.T) {
	t.Run("48h keeps a box stopped 30 hours", func(t *testing.T) {
		ta := newReapApp(t)
		ta.addBox(t, "dev1")
		ta.stopForReap(t, "dev1")
		ta.setReapClock(t, "dev1", reapNow.Add(-30*time.Hour))
		ta.setBoxTerm(t, "48h")

		require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
		require.Contains(t, ta.errOut.String(), "its disk goes at "+reapNow.Add(18*time.Hour).Format(time.RFC3339))
		ta.requireDiskKept(t, "dev1")
	})

	t.Run("2h deletes a box stopped 3 hours", func(t *testing.T) {
		ta := newReapApp(t)
		ta.addBox(t, "dev1")
		ta.stopForReap(t, "dev1")
		ta.setReapClock(t, "dev1", reapNow.Add(-3*time.Hour))
		ta.setBoxTerm(t, "2h")

		require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
		require.Contains(t, ta.errOut.String(), "deleted box dev1")
		ta.cluster.mu.Lock()
		defer ta.cluster.mu.Unlock()
		require.Empty(t, ta.cluster.volumes, "3 hours is past the 2 hours the operator set")
	})
}

// TestListAndStatusUseTheClusterTerm: a display command computes removes_at from
// the term the operator set, not from the default.
func TestListAndStatusUseTheClusterTerm(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-25*time.Hour))
	ta.setBoxTerm(t, "48h")

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "23h", "48 hours minus the 25 hours it has been stopped")
	require.NotContains(t, ta.out.String(), "not seen")
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--json"), ta.out.String())
	require.Contains(t, ta.out.String(), `"removes_at": "2026-11-03T11:00:00Z"`)
	require.NotContains(t, ta.out.String(), `"term_unknown"`)

	ta.JSON = false
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted at 2026-11-03T11:00:00Z, 48 hours after it stopped")
	require.Contains(t, ta.errOut.String(), "in 23h")
}

// TestAnUnusableTermDeletesNothing: a variable that cannot be read, an item that
// does not parse and an item below one hour all stop the pass before it looks at
// a box. None of them falls back to the default, which would delete a disk that
// the operator's term still protects.
func TestAnUnusableTermDeletesNothing(t *testing.T) {
	cases := map[string]func(ta *testApp){
		"unreadable": func(ta *testApp) {
			ta.cluster.mu.Lock()
			defer ta.cluster.mu.Unlock()
			ta.cluster.variableReadErrPath = map[string]error{
				boxReapVariable: &httpError{status: 403, msg: "Permission denied"},
			}
		},
		"unparsable":     func(ta *testApp) { ta.setBoxTerm(t, "soon") },
		"below one hour": func(ta *testApp) { ta.setBoxTerm(t, "1m") },
		"empty item":     func(ta *testApp) { ta.setBoxTerm(t, "") },
	}
	for name, broken := range cases {
		t.Run(name, func(t *testing.T) {
			ta := newReapApp(t)
			ta.addBox(t, "dev1")
			ta.stopForReap(t, "dev1")
			at := reapNow.Add(-30 * time.Hour)
			ta.setReapClock(t, "dev1", at)
			broken(ta)

			ta.cluster.callsMu.Lock()
			ta.cluster.calls = nil
			ta.cluster.callsMu.Unlock()

			require.Equal(t, ExitError, ta.run(t, "reap"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "deleted nothing and wrote no clock")
			ta.requireDiskKept(t, "dev1")
			clock, ok := ta.reapClock(t, "dev1")
			require.True(t, ok)
			require.Equal(t, at.Format(time.RFC3339), clock, "the pass wrote no clock")

			// It failed before it looked at a box: the only call is the read of
			// the setting.
			for _, call := range ta.callsWith("") {
				require.True(t, strings.HasPrefix(call, "VariableRead "),
					"a pass with an unusable term must write nothing, and it called %s", call)
			}

			// A display command still prints, and says the time is unknown.
			ta.errOut.Reset()
			require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
			require.Contains(t, ta.out.String(), "unknown")
			require.Contains(t, ta.errOut.String(), "cannot be read")
			ta.errOut.Reset()
			require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "cannot say when")
			require.Contains(t, ta.errOut.String(), "cannot be read")
		})
	}
}

// TestReapTermSetRefusesABadValue: a typo must not become a term that deletes
// disks almost at once, and a refused value writes nothing.
func TestReapTermSetRefusesABadValue(t *testing.T) {
	for _, value := range []string{"1m", "0h", "-24h", "soon", "", "24"} {
		t.Run(value, func(t *testing.T) {
			ta := newReapApp(t)
			ta.cluster.callsMu.Lock()
			ta.cluster.calls = nil
			ta.cluster.callsMu.Unlock()

			require.Equal(t, ExitUsage, ta.run(t, "reap", "term", "set", value), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "box reap term set")
			require.Empty(t, ta.callsWith("VariableWrite"))
			_, set := ta.boxTermValue(t)
			require.False(t, set, "a refused value writes nothing")
		})
	}
}

// TestReapTermSetShowAndUnset: the command an operator uses, and what it says.
func TestReapTermSetShowAndUnset(t *testing.T) {
	ta := newReapApp(t)

	require.Equal(t, ExitOK, ta.run(t, "reap", "term"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "24 hours, the default")
	require.Empty(t, ta.callsWith("VariableWrite"))

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term", "set", "48h"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "now 48 hours")
	require.Contains(t, ta.errOut.String(), "a change applies at once")
	value, set := ta.boxTermValue(t)
	require.True(t, set)
	require.Equal(t, "48h", value)

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term", "show"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "48 hours")
	require.Contains(t, ta.errOut.String(), boxReapVariable)

	// A duration that is not a whole number of hours is kept as it parses, and
	// shown in words.
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term", "set", "90m"), ta.errOut.String())
	value, _ = ta.boxTermValue(t)
	require.Equal(t, "90m", value)
	require.Contains(t, ta.errOut.String(), "1 hour 30 minutes")

	// JSON says the same, without a value a person has to parse.
	ta.out.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term", "show", "--json"), ta.out.String())
	require.Contains(t, ta.out.String(), `"term": "90m"`)
	require.Contains(t, ta.out.String(), `"term_seconds": 5400`)
	require.Contains(t, ta.out.String(), `"default": false`)

	ta.JSON = false
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term", "unset"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "back to the default, 24 hours")
	_, set = ta.boxTermValue(t)
	require.False(t, set)

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "reap", "term"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "24 hours, the default")
}

// TestARealPassDoesNotWriteTheSetting: the pass reads the term and deletes the
// disk; it never writes the cluster's setting.
func TestARealPassDoesNotWriteTheSetting(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.setReapClock(t, "dev1", reapNow.Add(-2*time.Hour))
	ta.setBoxTerm(t, "1h")
	ta.cluster.callsMu.Lock()
	ta.cluster.calls = nil
	ta.cluster.callsMu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "deleted box dev1")
	value, set := ta.boxTermValue(t)
	require.True(t, set, "the pass leaves the setting in place")
	require.Equal(t, "1h", value)
	for _, call := range ta.callsWith("VariableWrite") {
		require.NotContains(t, call, boxReapVariable, "the pass must not write the setting")
	}
	for _, call := range ta.callsWith("VariableCreateChecked") {
		require.NotContains(t, call, boxReapVariable)
	}
	for _, call := range ta.callsWith("VariableUpdateChecked") {
		require.NotContains(t, call, boxReapVariable)
	}
	for _, call := range ta.callsWith("VariableDelete") {
		require.NotContains(t, call, boxReapVariable)
	}
}

// TestListDoesNotReadTheTermWhenNoBoxIsStopped: the setting is read for a box
// that is not running, not for every `box ls`.
func TestListDoesNotReadTheTermWhenNoBoxIsStopped(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.cluster.callsMu.Lock()
	ta.cluster.calls = nil
	ta.cluster.callsMu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "ls"), ta.errOut.String())
	require.Empty(t, ta.callsWith("VariableRead "+boxReapVariable),
		"a listing of running boxes prints no term, so it reads none")
}
