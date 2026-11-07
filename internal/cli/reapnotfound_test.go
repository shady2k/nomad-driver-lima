package cli

// A pass decides what to delete from what Nomad says is not there, so "not there"
// has to be Nomad's own answer. An error that only reads like one -- a port with
// 404 in it, a 500 that mentions something not found -- is a question that went
// unanswered, and a box whose state is unknown keeps its disk.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

const (
	refusedOnAPortWith404 = `Get "http://10.0.0.1:14046/v1/x": dial tcp 10.0.0.1:14046: connect: connection refused`
	failedSayingNotFound  = "rpc error: evaluation not found on the leader"
)

// TestAnUnreadableTermIsNotAMissingOne: a cluster set to 7 days whose setting
// cannot be read must not have its disks deleted after the default 24 hours.
func TestAnUnreadableTermIsNotAMissingOne(t *testing.T) {
	for name, err := range map[string]error{
		"a port with 404 in it":       errors.New(refusedOnAPortWith404),
		"a failure saying not found":  errors.New(failedSayingNotFound),
		"a 500 that says not found":   &httpError{status: 500, msg: failedSayingNotFound},
		"a 500 with 404 in its words": &httpError{status: 500, msg: "backend 404 from the leader"},
	} {
		t.Run(name, func(t *testing.T) {
			ta := newReapApp(t)
			ta.addBox(t, "dev1")
			ta.stopForReap(t, "dev1")
			ta.setReapClock(t, "dev1", reapNow.Add(-30*time.Hour))
			ta.cluster.mu.Lock()
			ta.cluster.variableReadErrPath = map[string]error{boxReapVariable: err}
			ta.cluster.mu.Unlock()

			require.Equal(t, ExitError, ta.run(t, "reap"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "deleted nothing")
			ta.requireDiskKept(t, "dev1")
		})
	}
}

// TestReapKeepsADiskWhenAnErrorOnlyReadsLikeNotFound: a running box whose job and
// allocations cannot be read looks, to a check on the words of the error, like a
// box Nomad has collected; with an old clock that deletes the disk under the VM.
func TestReapKeepsADiskWhenAnErrorOnlyReadsLikeNotFound(t *testing.T) {
	for name, err := range map[string]error{
		"a port with 404 in it":      errors.New(refusedOnAPortWith404),
		"a failure saying not found": errors.New(failedSayingNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			ta := newReapApp(t)
			ta.addBox(t, "dev1")
			ta.setReapClock(t, "dev1", reapNow.Add(-40*time.Hour))
			ta.cluster.mu.Lock()
			ta.cluster.jobErr = err
			ta.cluster.jobAllocsErr = err
			ta.cluster.mu.Unlock()

			require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
			require.Contains(t, ta.errOut.String(), "cannot read the job of box dev1")
			ta.requireDiskKept(t, "dev1")
		})
	}
}

// TestAClockThatCannotBeReadIsNotAMissingOne: taking it for missing would start a
// new term and throw away the time the box has already served.
func TestAClockThatCannotBeReadIsNotAMissingOne(t *testing.T) {
	ta := newReapApp(t)
	ta.addBox(t, "dev1")
	ta.stopForReap(t, "dev1")
	ta.cluster.mu.Lock()
	ta.cluster.variableReadErrPath = map[string]error{BoxStoppedVariable("dev1"): errors.New(failedSayingNotFound)}
	ta.cluster.mu.Unlock()

	require.Equal(t, ExitOK, ta.run(t, "reap"), ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "cannot read")
	require.Empty(t, ta.callsWith("VariableCreateChecked"), "a clock that cannot be read is not started again")
	ta.requireDiskKept(t, "dev1")
}

// TestNomadsOwnNotFoundIsRecognised: the fakes answer in their own type, so this
// asks the real API client, against a server that answers as Nomad does. A box
// whose job was collected has to read as gone, or a pass would never delete it.
func TestNomadsOwnNotFoundIsRecognised(t *testing.T) {
	status := http.StatusNotFound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("job not found"))
	}))
	defer server.Close()
	cfg := api.DefaultConfig()
	cfg.Address = server.URL
	nomad, err := NewNomadClientWithConfig(cfg)
	require.NoError(t, err)
	ctx := t.Context()

	_, err = nomad.VariableRead(ctx, boxReapVariable)
	require.Error(t, err)
	require.True(t, isConfirmedNotFound(err), "a variable that is not there: %v", err)
	_, err = nomad.Job(ctx, "box-gone")
	require.Error(t, err)
	require.True(t, isConfirmedNotFound(err), "a job that is not there: %v", err)
	_, err = nomad.JobAllocations(ctx, "box-gone")
	require.Error(t, err)
	require.True(t, isConfirmedNotFound(err), "the allocations of a job that is not there: %v", err)

	status = http.StatusInternalServerError
	_, err = nomad.VariableRead(ctx, boxReapVariable)
	require.Error(t, err)
	require.False(t, isConfirmedNotFound(err), "a 500 whose body says not found: %v", err)
	_, err = nomad.Job(ctx, "box-gone")
	require.Error(t, err)
	require.False(t, isConfirmedNotFound(err), "a 500 whose body says not found: %v", err)

	server.Close()
	_, err = nomad.VariableRead(ctx, boxReapVariable)
	require.Error(t, err)
	require.False(t, isConfirmedNotFound(err), "a server that is gone: %v", err)
	require.False(t, isConfirmedNotFound(errors.New(refusedOnAPortWith404)))
	require.False(t, isConfirmedNotFound(nil))
}
