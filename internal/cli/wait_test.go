package cli

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// TestAllocFailureTextPrefersTheDriversOwnWord: a failed bake or publish ends with
// Nomad's "Not Restarting: Policy allows no restarts", which says what Nomad did
// and not why the work stopped. What the user needs is the driver's own message,
// measured on a second publish: "proj-1 amd64 is published in this store, and a
// published version never changes. Bake another version instead".
func TestAllocFailureTextPrefersTheDriversOwnWord(t *testing.T) {
	alloc := &api.Allocation{
		ClientDescription: "Failed tasks",
		TaskStates: map[string]*api.TaskState{
			boxTaskName: {Events: []*api.TaskEvent{
				{Type: "Received", DisplayMessage: "Task received by client"},
				{Type: "Driver", DisplayMessage: "publishing proj-1 amd64 failed: http://example.com/box-base/proj-1/amd64/x.qcow2 already exists: proj-1 amd64 is published in this store, and a published version never changes. Bake another version instead"},
				{Type: "Terminated", DisplayMessage: "Exit Code: 1"},
				{Type: "Not Restarting", DisplayMessage: "Policy allows no restarts"},
			}},
		},
	}
	text := allocFailureText(alloc)
	require.Contains(t, text, "already exists", "the driver's message is the one that says why")
	require.NotContains(t, text, "Policy allows no restarts")

	// Without a driver event the old behaviour stands: the last thing Nomad said.
	plain := &api.Allocation{TaskStates: map[string]*api.TaskState{
		boxTaskName: {Events: []*api.TaskEvent{
			{Type: "Failed Validation", DisplayMessage: `No argument or block type is named "recipe"`},
		}},
	}}
	require.Contains(t, allocFailureText(plain), "recipe")
}

// TestAllocFailureTextSkipsTheCleanupOfAFailedBake: a bake reports its steps as
// driver events, and the last of them is the clone being deleted. The failure is
// the event that says how the task ended, and that is the one a person needs.
func TestAllocFailureTextSkipsTheCleanupOfAFailedBake(t *testing.T) {
	alloc := &api.Allocation{TaskStates: map[string]*api.TaskState{
		boxTaskName: {Events: []*api.TaskEvent{
			{Type: "Driver", Details: map[string]string{"kind": "bake"}, DisplayMessage: "cloned box a into a-bake-1"},
			{Type: "Driver", Details: map[string]string{"kind": "exit"},
				DisplayMessage: "the seal of box a failed in the clone: guest/no-secrets.sh failed in this box: FAIL /home/user/auth.json"},
			{Type: "Driver", Details: map[string]string{"kind": "bake"},
				DisplayMessage: "deleted the clone a-bake-1: the copy of the box's disk is gone"},
			{Type: "Terminated", DisplayMessage: "Exit Code: 1"},
		}},
	}}
	text := allocFailureText(alloc)
	require.Contains(t, text, "guest/no-secrets.sh failed in this box")
	require.NotContains(t, text, "deleted the clone")
}

// A turn that started a long check and ended while it ran is reported as done by
// boxd, and the wait prints the warning it always prints. --strict is what lets a
// script tell that turn from one that finished its work: exit 6, and one sentence
// on stderr that says why and what to send next.
func TestWaitStrictExitsSixOnATurnThatLeftWorkRunning(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done",
		"text":"the test run is still going, I will commit after it",
		"stop_reason":"end_turn",
		"still_running":{"processes":[
			{"pid":123,"command":"go test ./...","age_seconds":250,"cpu_seconds":12.5,"children":4}
		]}}`
	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--strict")
	require.Equal(t, ExitLeftRunning, rc)
	// Everything a plain wait prints is printed, unchanged.
	require.Contains(t, ta.out.String(), "the test run is still going, I will commit after it")
	err := ta.errOut.String()
	require.Contains(t, err, "the turn ended, but these processes it may have started are still running in the box:")
	require.Contains(t, err, "go test ./... (pid 123, 4m10s, 4 children, 12.5s CPU)")
	// And stderr adds the one sentence that explains the exit code.
	require.Contains(t, err, "the turn 1 of session s-1 left work running in the box, so this wait exits 6")
	require.Contains(t, err, "`box send dev1 s-1` with \"wait for it, then finish\" is the usual next step")
}

// Without --strict the exit code stays the turn's: a script that did not ask for
// the distinction does not get it.
func TestWaitWithoutStrictKeepsZeroWithLeftovers(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"all done",
		"still_running":{"processes":[{"pid":123,"command":"go build ./...","age_seconds":5}]}}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "exits 6")
}

// A turn that left nothing running is a finished turn, and --strict has nothing
// to say about it.
func TestWaitStrictExitsZeroWhenNothingIsLeftRunning(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"all done","stop_reason":"end_turn"}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--strict"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "exits 6")
}

// 6 belongs to a turn that would otherwise exit 0. A turn that ended in an error
// keeps its 1 under --strict, even when it left processes running.
func TestWaitStrictKeepsTheExitCodeOfAFailedTurn(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"error","error":"Model gw/nope not found",
		"still_running":{"processes":[{"pid":123,"command":"sleep 600","age_seconds":5}]}}`
	require.Equal(t, ExitError, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--strict"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "ended error")
	require.NotContains(t, ta.errOut.String(), "exits 6")
}

// A wait that runs out while the turn goes on is a timeout under --strict too;
// the turn's processes are not what ended that wait.
func TestWaitStrictKeepsTheTimeoutExitCode(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 9"] = `{"session":"s-1","turn":2,"state":"running",
		"still_running":{"processes":[{"pid":123,"command":"go test ./...","age_seconds":5}]}}`
	require.Equal(t, ExitTimeout, ta.run(t, "wait", "dev1", "s-1", "--timeout", "10s", "--strict"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "exits 6")
}

// The JSON document is the same with and without --strict: it already carries
// still_running, and the flag changes the exit code alone (plus the sentence on
// stderr that says why the exit is not 0).
func TestWaitStrictPrintsTheSameJSONDocument(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["wait s-1 --timeout 59"] = `{"session":"s-1","turn":1,"state":"done","text":"the build is still running",
		"still_running":{"processes":[{"pid":123,"command":"go build ./...","age_seconds":5,"cpu_seconds":1.5,"children":2}],"more":1}}`
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--json"), ta.errOut.String())
	plain := ta.out.String()
	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitLeftRunning, ta.run(t, "wait", "dev1", "s-1", "--timeout", "1m", "--json", "--strict"))
	require.Equal(t, plain, ta.out.String(), "the document is the same with and without --strict")
	require.Contains(t, ta.errOut.String(), "exits 6", "the document cannot carry the exit code, so stderr says it")
}
