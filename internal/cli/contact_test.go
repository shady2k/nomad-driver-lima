package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errLostHere is what Nomad reports when the client it talks to for an exec is
// gone: the shape of the failure that took three waits down with exit 1.
var errLostHere = errors.New("cannot run boxctl in the box: unexpected EOF")

// stopBox makes the fake cluster report the box as stopped.
func stopBox(t *testing.T, ta *testApp, id string) {
	t.Helper()
	require.NoError(t, ta.cluster.StopJob(context.Background(), BoxJobName(id), false))
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	for _, alloc := range ta.cluster.allocs {
		if alloc.JobID != BoxJobName(id) {
			continue
		}
		alloc.ClientStatus = "complete"
		if task := alloc.TaskStates[boxTaskName]; task != nil {
			task.State = "dead"
		}
	}
}

// TestWaitSurvivesALostConnection is the report that started this: a turn was
// running, the connection to the box went away, and the wait ended with exit 1
// although the turn went on. It prints one line when contact goes and one when it
// comes back, and ends with the turn's real result.
func TestWaitSurvivesALostConnection(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.lost = 2
	ta.guest.lostErr = errLostHere
	ta.guest.responses["wait s-1"] = `{"session":"s-1","turn":3,"state":"done","text":"the build passed","stop_reason":"end_turn"}`

	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "5m")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.out.String(), "the build passed")
	require.Equal(t, 1, strings.Count(ta.out.String(), "the build passed"), "the answer is printed once, however many attempts it took")
	errText := ta.errOut.String()
	require.Contains(t, errText, "lost contact with the box; retrying")
	require.Contains(t, errText, "back in contact after ")
	require.Equal(t, 1, strings.Count(errText, "lost contact with the box"), "one line for the loss, not one per attempt")
	require.Equal(t, 1, strings.Count(errText, "back in contact after"), "one line for the return")
}

// TestWaitReportsAnUnknownTurn: the box never comes back within --timeout. The
// turn may still run, so this is neither a failure nor a timeout; it is the exit
// code that says the state is unknown.
func TestWaitReportsAnUnknownTurn(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.lost = 1 << 20
	ta.guest.lostErr = errLostHere

	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "10s")
	require.Equal(t, ExitUnknown, rc, ta.errOut.String())
	msg := ta.errOut.String()
	require.Contains(t, msg, "lost contact with the box dev1 for ")
	require.Contains(t, msg, "may still be running")
	require.Contains(t, msg, "neither a failure nor a timeout")
}

// TestWaitEndsWithABoxThatIsGone: a lost connection is not an end, but a box that
// was stopped is. The wait says so with the box's own sentence, and not with the
// unknown exit code.
func TestWaitEndsWithABoxThatIsStopped(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	stopped := false
	ta.guest.loseWhen = func(command []string, call int) error {
		if !stopped {
			stopped = true
			stopBox(t, ta, "dev1")
		}
		return errLostHere
	}
	rc := ta.run(t, "wait", "dev1", "s-1", "--timeout", "10m")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
}

// TestEventsFollowResumesWithoutRepeatsOrGaps: a stream that dies and comes back
// continues at the last sequence it printed, so every event is printed once.
func TestEventsFollowResumesWithoutRepeatsOrGaps(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["events s-1 --limit 200"] =
		`{"events":[{"seq":1,"kind":"turn_start","summary":"one"},{"seq":2,"kind":"tool","summary":"two"},{"seq":3,"kind":"tool_result","summary":"three"}],"next":4,"total":3,"more":true}`
	ta.guest.responses["events s-1 --since 4 --limit 200"] =
		`{"events":[{"seq":4,"kind":"message","summary":"four"},{"seq":5,"kind":"turn_end","summary":"five"}],"next":6,"total":2,"more":true}`
	ta.guest.responses["events s-1 --since 6 --limit 200"] = `{"events":[],"next":6,"total":0}`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventsCalls := 0
	ta.guest.loseWhen = func(command []string, call int) error {
		if len(command) > 1 && command[1] == "events" {
			eventsCalls++
			switch eventsCalls {
			case 2:
				return errLostHere
			case 4:
				// The feed is read to the end: the caller stops following.
				cancel()
			}
		}
		return nil
	}
	rc := ta.Run(ctx, []string{"events", "dev1", "s-1", "-f"})
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	out := ta.out.String()
	for _, want := range []string{"one", "two", "three", "four", "five"} {
		require.Equal(t, 1, strings.Count(out, want), "%s must be printed once: %q", want, out)
	}
	require.Contains(t, ta.errOut.String(), "lost contact with the box; retrying")
	require.Contains(t, ta.errOut.String(), "back in contact after ")
}

// TestLogsResumeWhereTheyStopped: the two log streams pick up at the byte they
// left off at, so a client that restarts mid-read costs no repeated line.
func TestLogsResumeWhereTheyStopped(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.cluster.logSteps = map[string][]logStep{
		"stdout": {
			{frames: []string{"first line\n"}, err: errLostHere},
			{frames: []string{"second line\n"}},
		},
		"stderr": {{frames: []string{"a warning\n"}}},
	}
	rc := ta.run(t, "logs", "dev1", "-f")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.out.String(), "first line")
	require.Contains(t, ta.out.String(), "second line")
	require.Equal(t, 1, strings.Count(ta.out.String(), "first line"), "a resumed stream must not repeat what it printed")
	require.Contains(t, ta.errOut.String(), "lost contact with the box; retrying")
	require.Contains(t, ta.errOut.String(), "back in contact after ")
	// The second read of stdout starts after the 11 bytes the first one read.
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Equal(t, 2, ta.cluster.logCalls["stdout"])
	var offsets []string
	for _, call := range ta.cluster.calls {
		if strings.HasPrefix(call, "Logs ") && strings.Contains(call, " stdout ") {
			offsets = append(offsets, call)
		}
	}
	require.Contains(t, strings.Join(offsets, " | "), " stdout 11", "the stream resumes at the byte it read to")
}

// TestRunIsNotSentTwice is the one command that cannot be retried: a stream that
// was lost may have run the command to the end or not at all, and nobody can be
// asked. It runs once, and says the result is unknown.
func TestRunIsNotSentTwice(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.lost = 1 << 20
	ta.guest.lostErr = errLostHere
	rc := ta.run(t, "run", "dev1", "--", "make", "test")
	require.Equal(t, ExitUnknown, rc, ta.errOut.String())
	msg := ta.errOut.String()
	require.Contains(t, msg, "what the command did is not known")
	require.Contains(t, msg, "may still be running in the box")
	require.Contains(t, msg, "It is not run again")
	runs := 0
	for _, call := range ta.guest.callLines() {
		if strings.Contains(call, "make test") {
			runs++
		}
	}
	require.Equal(t, 1, runs, "the command must be sent once: %v", ta.guest.callLines())
}

// TestRunEndsWithABoxThatIsGone: a command in a box that stopped is not an
// unknown result; the box's own sentence says what happened.
func TestRunEndsWithABoxThatIsStopped(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.lost = 1 << 20
	ta.guest.lostErr = errLostHere
	stopBox(t, ta, "dev1")
	rc := ta.run(t, "run", "dev1", "--", "true")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "the box dev1 is stopped")
}

// TestRemoveSaysWhenItsDescriptionIsLeftBehind: the disk is deleted and the
// record of the box is not, so the command says it is partial instead of
// reporting a box that is still there.
func TestRemoveSaysWhenItsDescriptionIsLeftBehind(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	// A variable delete that the token may not do.
	ta.cluster.mu.Lock()
	ta.cluster.variableDeleteErr = errors.New("Permission denied")
	ta.cluster.mu.Unlock()
	rc := ta.run(t, "rm", "dev1", "--yes")
	require.Equal(t, ExitError, rc, ta.errOut.String())
	msg := ta.errOut.String()
	require.Contains(t, msg, "the box dev1 is gone")
	require.Contains(t, msg, "its description")
	require.Contains(t, msg, "is left behind")
	require.Contains(t, msg, "nomad var delete")
	ta.cluster.mu.Lock()
	defer ta.cluster.mu.Unlock()
	require.Empty(t, ta.cluster.volumes, "the disk is deleted before the record is tried")
}

// TestEventsTailPrintsTheLastEvents: --tail is the news of a long session, one
// call for the count and one for the events, and the same with --json.
func TestEventsTailPrintsTheLastEvents(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["events s-1 --limit 1"] = `{"events":[{"seq":1,"kind":"turn_start","summary":"one"}],"next":2,"total":120,"more":true}`
	ta.guest.responses["events s-1 --since 118 --limit 3"] =
		`{"events":[{"seq":118,"kind":"tool","summary":"old"},{"seq":119,"kind":"tool","summary":"recent"},{"seq":120,"kind":"turn_end","summary":"last"}],"next":121,"total":3}`

	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1", "--tail", "3"), ta.errOut.String())
	out := ta.out.String()
	require.Contains(t, out, "recent")
	require.Contains(t, out, "last")
	require.NotContains(t, out, "one", "the first event of the feed is not in the tail")

	ta.out.Reset()
	ta.JSON = true
	require.Equal(t, ExitOK, ta.run(t, "events", "dev1", "s-1", "--tail", "3"), ta.errOut.String())
	lines := strings.Split(strings.TrimSpace(ta.out.String()), "\n")
	require.Len(t, lines, 3, ta.out.String())
	require.Contains(t, lines[2], `"seq":120`)
}

// TestEventsTailRefusesToFollow keeps the two ways of reading a feed apart.
func TestEventsTailRefusesToFollow(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	require.Equal(t, ExitUsage, ta.run(t, "events", "dev1", "s-1", "--tail", "3", "-f"))
}

// TestLostContactClassification pins what is a lost connection and what is an
// answer from the box: the second must never be retried as if it were the first.
func TestLostContactClassification(t *testing.T) {
	require.True(t, lostContact(errors.New("cannot run boxctl in the box: unexpected EOF")))
	require.True(t, lostContact(errors.New("No path to node")))
	require.True(t, lostContact(errors.New("dial tcp 127.0.0.1:4646: connect: connection refused")))
	require.True(t, lostContact(wrapf(fmt.Errorf("Post \"http://nomad/v1/jobs\": EOF"), "cannot read the job")))
	require.False(t, lostContact(nil))
	require.False(t, lostContact(errorf("session s-1 is still working on a turn")))
	require.False(t, lostContact(&BoxctlError{Code: "not_found", Message: "no session \"s-9\""}))
	require.False(t, lostContact(boxNotRunning(BoxRecord{ID: "dev1", State: "stopped"})))
}

// TestASessionActiveOutsideATurnIsShownAsSuch: an executor still working after a
// turn that ended in an error is neither idle nor dead, and saying so is what
// keeps an agent from opening a second writer in the same working copy.
func TestASessionActiveOutsideATurnIsShownAsSuch(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session ls"] = `{"sessions":[{"id":"s-1","executor":"prime","model":"p/m","state":"idle","last_turn":4,"cwd":"/work/proj","executor_active":42}]}`
	require.Equal(t, ExitOK, ta.run(t, "session", "ls", "dev1"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "active outside a turn")

	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "active outside a turn")
}

// TestSessionNewWarnsAboutAnotherSessionInTheSameDirectory: the warning is a
// warning, not a refusal: the session is created and the exit code stays 0.
func TestSessionNewWarnsAboutAnotherSessionInTheSameDirectory(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session new --executor prime --model p/m"] = `{"session":"s-2"}`
	ta.guest.responses["session get s-2"] = `{"id":"s-2","executor":"prime","model":"p/m","state":"idle","cwd":"/work/proj"}`
	ta.guest.responses["session ls"] = `{"sessions":[` +
		`{"id":"s-1","executor":"prime","model":"p/m","state":"running","last_turn":2,"cwd":"/work/proj"},` +
		`{"id":"s-2","executor":"prime","model":"p/m","state":"idle","cwd":"/work/proj"}]}`

	rc := ta.run(t, "session", "new", "dev1", "--executor", "prime", "--model", "p/m")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "session s-1 is working in the same directory")
	require.NotContains(t, ta.errOut.String(), "session s-2 is working")
}

// TestSendWarnsAboutAnotherSessionInTheSameDirectory covers the second writer:
// the session is asked to be told, and the exit code does not change.
func TestSendWarnsAboutAnotherSessionInTheSameDirectory(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session ls"] = `{"sessions":[` +
		`{"id":"s-1","executor":"prime","model":"p/m","state":"idle","last_turn":2,"cwd":"/work/proj","executor_active":12},` +
		`{"id":"s-2","executor":"prime","model":"p/m","state":"idle","cwd":"/work/proj"}]}`
	ta.guest.responses["send s-2"] = `{"session":"s-2","turn":1}`

	rc := ta.run(t, "send", "dev1", "s-2", "go on")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "session s-1 is working in the same directory")
}

// TestNoWarningForAnotherDirectory: two sessions in one box are normal, and a
// session somewhere else is nothing to say.
func TestNoWarningForAnotherDirectory(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session new --executor prime --model p/m"] = `{"session":"s-2"}`
	ta.guest.responses["session get s-2"] = `{"id":"s-2","executor":"prime","model":"p/m","state":"idle","cwd":"/work/other"}`
	ta.guest.responses["session ls"] = `{"sessions":[` +
		`{"id":"s-1","executor":"prime","model":"p/m","state":"running","last_turn":2,"cwd":"/work/proj"},` +
		`{"id":"s-2","executor":"prime","model":"p/m","state":"idle","cwd":"/work/other"},` +
		`{"id":"s-3","executor":"prime","model":"p/m","state":"idle","last_turn":1,"cwd":"/work/proj"}]}`

	require.Equal(t, ExitOK, ta.run(t, "session", "new", "dev1", "--executor", "prime", "--model", "p/m"), ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "working in the same directory")
}

// TestWaitSaysWhenContactIsBackWhileTheTurnGoesOn: the line that says the box
// answers again comes when it does, not when the turn ends: a wait that is back
// in contact and still waiting for a slow turn has to say so, or the line reads
// as if the whole time was lost.
func TestWaitSaysWhenContactIsBackWhileTheTurnGoesOn(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["session get s-1"] = `{"id":"s-1","executor":"prime","model":"p/m","state":"running","last_turn":2}`
	ta.guest.responses["wait s-1 --turn 2"] = `{"session":"s-1","turn":2,"state":"done","text":"turn two"}`

	waitCalls := 0
	ta.guest.loseWhen = func(command []string, call int) error {
		if len(command) > 1 && command[1] == "wait" {
			waitCalls++
			if waitCalls == 1 {
				return errLostHere
			}
		}
		return nil
	}
	require.Equal(t, ExitOK, ta.run(t, "wait", "dev1", "s-1", "--timeout", "2m"), ta.errOut.String())
	require.Contains(t, ta.out.String(), "turn two")
	errText := ta.errOut.String()
	lost := strings.Index(errText, "lost contact with the box; retrying")
	back := strings.Index(errText, "back in contact after ")
	require.True(t, lost >= 0 && back > lost, "the loss and the return are both said, in that order: %q", errText)
	require.Equal(t, 1, strings.Count(errText, "back in contact after"), errText)
}

// TestWaitGivesUpOnACallThatNeverAnswers: Nomad's exec can leave a call waiting
// while its server looks for a node that is restarting, and its context does not
// end it. The wait gives up on that call by its own bound and ends with the exit
// code that says the turn's state is unknown, instead of hanging past --timeout.
func TestWaitGivesUpOnACallThatNeverAnswers(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.deaf["wait s-1"] = true
	defer close(ta.guest.deafCh)

	done := make(chan int, 1)
	go func() { done <- ta.run(t, "wait", "dev1", "s-1", "--timeout", "3s") }()
	select {
	case rc := <-done:
		require.Equal(t, ExitUnknown, rc, ta.errOut.String())
		require.Contains(t, ta.errOut.String(), "lost contact with the box; retrying")
		require.Contains(t, ta.errOut.String(), "may still be running")
	case <-time.After(15 * time.Second):
		t.Fatal("box wait hung on a call that never answers")
	}
}
