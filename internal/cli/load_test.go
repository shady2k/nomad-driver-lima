package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// usageJSON is a boxctl info answer whose usage carries what the new fields are
// for: the busiest processes, a process that holds a whole core, and work running
// with no turn in progress.
const usageJSON = `{"boxd_version":"0.1.15","usage":{"at":"2026-11-04T16:44:00Z","cpu_percent":91.5,"iowait_percent":42,` +
	`"load1":3.2,"load5":2.1,"mem_total_mib":1960,"mem_available_mib":120,"disk_path":"/","disk_total_mib":15360,` +
	`"disk_free_mib":11264,"top":"go 900 MiB, node 300 MiB",` +
	`"processes":[{"pid":341,"command":"cc1 -c big.c","cpu_percent":87.3,"mem_mib":512},` +
	`{"pid":342,"command":"go build ./...","cpu_percent":12.5,"mem_mib":300}],` +
	`"hogs":[{"pid":851,"command":"systemd-logind","cpu_percent":100,"for_seconds":25200}],` +
	`"leftover":{"count":12,"processes":[{"pid":123,"command":"go build ./...","age_seconds":250,"cpu_seconds":12.5,"children":4},` +
	`{"pid":456,"command":"python3 -m http.server","age_seconds":30,"cpu_seconds":0.2,"children":0}],"more":10}}}`

// TestTopShowsTheBusiestProcesses: box top prints the biggest processes without a
// flag. A summary line alone says the box is busy and not what makes it busy, and
// an agent that had to know the flag never asked.
func TestTopShowsTheBusiestProcesses(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = usageJSON
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	out := ta.errOut.String()
	for _, want := range []string{
		"the busiest processes:",
		"pid 341  87.3% of a core  512 MiB  cc1 -c big.c",
		"pid 342  12.5% of a core  300 MiB  go build ./...",
		"systemd-logind (pid 851) has used a whole core for 7h0m",
	} {
		require.Contains(t, out, want)
	}
	// The summary line is still the first line of box top.
	require.Less(t, strings.Index(out, "CPU 92% (iowait 42%)"), strings.Index(out, "the busiest processes:"))

	// --json carries the same three lists.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1", "--json"), ta.errOut.String())
	var doc topOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Usage.Processes, 2)
	require.Equal(t, 341, doc.Usage.Processes[0].Pid)
	require.Equal(t, 87.3, doc.Usage.Processes[0].CPUPercent)
	require.Equal(t, uint64(512), doc.Usage.Processes[0].MemMiB)
	require.Len(t, doc.Usage.Hogs, 1)
	require.Equal(t, 851, doc.Usage.Hogs[0].Pid)
	require.Equal(t, 25200, doc.Usage.Hogs[0].ForSeconds)
}

// TestTopWithoutBusyProcesses: a boxd that measures no process adds no line, and
// box top is still the summary line.
func TestTopWithoutBusyProcesses(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = `{"boxd_version":"0.1.15","usage":{"at":"2026-11-04T16:44:00Z","cpu_percent":12,` +
		`"mem_total_mib":4096,"mem_available_mib":3900,"disk_path":"/","disk_total_mib":10240,"disk_free_mib":9216}}`
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "CPU 12% (iowait 0%)")
	require.NotContains(t, out, "the busiest processes")
	require.NotContains(t, out, "whole core")
	require.NotContains(t, out, "no turn in progress")
}

// TestStatusAndTopSayWhatRunsWithNoTurn: work left by an earlier turn is why a box
// is busy while its sessions are idle, and both commands that show what the guest
// is doing say so in one line.
func TestStatusAndTopSayWhatRunsWithNoTurn(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = usageJSON
	ta.guest.responses["session ls"] = `{"sessions":[]}`
	const want = "12 work process(es) are running with no turn in progress: " +
		"go build ./... (pid 123, 4m10s, 4 children, 12.5s CPU); " +
		"python3 -m http.server (pid 456, 30s, 0.2s CPU); and 10 more"

	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), want)

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), want)

	// The JSON document carries the field itself, count and lines.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"), ta.errOut.String())
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.NotNil(t, doc.Guest)
	require.NotNil(t, doc.Guest.Usage.Leftover)
	require.Equal(t, 12, doc.Guest.Usage.Leftover.Count)
	require.Equal(t, 10, doc.Guest.Usage.Leftover.More)
	require.Equal(t, 123, doc.Guest.Usage.Leftover.Processes[0].Pid)
}

// TestNoLeftoverLineWhenTurnsRun: a boxd that leaves the field out (a turn is
// running, or nothing is left) prints no line at all, so the line always means
// something.
func TestNoLeftoverLineWhenTurnsRun(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = `{"boxd_version":"0.1.15","usage":{"at":"2026-11-04T16:44:00Z","cpu_percent":12,` +
		`"mem_total_mib":4096,"mem_available_mib":3900,"disk_path":"/","disk_total_mib":10240,"disk_free_mib":9216}}`
	ta.guest.responses["session ls"] = `{"sessions":[]}`
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "no turn in progress")
	require.Equal(t, ExitOK, ta.run(t, "top", "dev1"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "no turn in progress")
	// An empty leftover is no line either.
	empty := &Leftover{Count: 0}
	require.Equal(t, "", empty.Summary())
}

// TestLeftoverSummaryIsOneLine: the line says how many processes there are, names
// the few a person needs and counts the rest.
func TestLeftoverSummaryIsOneLine(t *testing.T) {
	one := &Leftover{Count: 1, Processes: []StillRunningProcess{
		{Pid: 7, Command: "sleep 300", AgeSeconds: 5},
	}}
	require.Equal(t, "1 work process(es) are running with no turn in progress: sleep 300 (pid 7, 5s, 0.0s CPU)",
		one.Summary())

	// Only the top few are named: the line is a summary and the rest is a count.
	many := &Leftover{Count: 15, Processes: []StillRunningProcess{
		{Pid: 1, Command: "a", AgeSeconds: 1},
		{Pid: 2, Command: "b", AgeSeconds: 1},
		{Pid: 3, Command: "c", AgeSeconds: 1},
		{Pid: 4, Command: "d", AgeSeconds: 1},
	}, More: 11}
	got := many.Summary()
	require.Contains(t, got, "15 work process(es) are running with no turn in progress: a (pid 1")
	require.NotContains(t, got, "d (pid 4", "the fourth line is not named")
	require.Contains(t, got, "and 12 more")
	var none *Leftover
	require.Equal(t, "", none.Summary())
}

// TestEventsSaysWhenTheFeedWasCut: a page of a feed says so, with the cursor that
// continues it. An agent that read the limit for the end of the events waited for
// events that were already there.
func TestEventsSaysWhenTheFeedWasCut(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["events s-1 --limit 200"] = `{"events":[` +
		`{"seq":1,"ts":"2026-11-02T10:00:00Z","kind":"turn_start","summary":"started"},` +
		`{"seq":2,"ts":"2026-11-02T10:00:01Z","kind":"tool","summary":"read a file"}],"next":3,"total":9,"more":true}`
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "turn_start")
	require.Contains(t, ta.errOut.String(), "2 of 9 events, more with --since 3")

	// --json carries the same fact as the last document on stdout.
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1", "--json"), ta.errOut.String())
	lines := strings.Split(strings.TrimSpace(ta.out.String()), "\n")
	require.Len(t, lines, 3, "two events and the cursor")
	require.Contains(t, lines[0], `"kind":"turn_start"`)
	var cursor struct {
		NextSince int64 `json:"next_since"`
		More      bool  `json:"more"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &cursor))
	require.Equal(t, int64(3), cursor.NextSince)
	require.True(t, cursor.More)
	require.Empty(t, strings.TrimSpace(ta.errOut.String()), "a JSON answer says it in the document")
}

// TestEventsSaysNothingWhenTheFeedIsWhole: a feed that fits in one call is not a
// page, and neither output says it is.
func TestEventsSaysNothingWhenTheFeedIsWhole(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["events s-1 --limit 200"] = `{"events":[` +
		`{"seq":1,"ts":"2026-11-02T10:00:00Z","kind":"turn_start","summary":"started"}],"next":2,"total":1,"more":false}`
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "turn_start")
	require.NotContains(t, ta.errOut.String(), "more with --since")

	// An old boxd that reports neither field reads as a whole feed too.
	ta.guest.responses["events s-1 --limit 200"] = `{"events":[{"seq":1,"kind":"turn_start","summary":"started"}],"next":2}`
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1", "--json"), ta.errOut.String())
	require.NotContains(t, ta.out.String(), "next_since")
}
