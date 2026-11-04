package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// silentGuest returns a test app whose box is running and whose guest never
// answers the named boxctl calls, and which waits a few milliseconds for the
// answer instead of twenty seconds.
func silentGuest(t *testing.T, calls ...string) *testApp {
	t.Helper()
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	for _, call := range calls {
		ta.guest.silent[call] = true
	}
	ta.GuestTimeout = 20 * time.Millisecond
	return ta
}

// TestGuestCallBoundIsTwentySeconds keeps the number the messages promise.
func TestGuestCallBoundIsTwentySeconds(t *testing.T) {
	require.Equal(t, 20*time.Second, guestCallTimeout)
}

// TestStatusOfASilentGuestStillShowsWhatNomadKnows: a guest out of memory does
// not answer, and box status is still a status: everything Nomad knows, one
// line that says the guest is the problem, and the exit code of a status that
// worked.
func TestStatusOfASilentGuestStillShowsWhatNomadKnows(t *testing.T) {
	ta := silentGuest(t, "info", "session ls")
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	out := ta.errOut.String()
	require.Contains(t, out, "box dev1")
	require.Contains(t, out, "state   running", "the facts Nomad has are printed")
	require.Contains(t, out, "the guest does not answer (no reply in 20ms): `boxctl info`")
	require.NotContains(t, out, "sessions:", "a guest that does not answer has no sessions to show")
}

// TestStatusSilentGuestInJSON carries the same fact as a field.
func TestStatusSilentGuestInJSON(t *testing.T) {
	ta := silentGuest(t, "info", "session ls")
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"), ta.errOut.String())
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Contains(t, doc.GuestError, "the guest does not answer (no reply in 20ms)")
	require.Nil(t, doc.Guest, "no guest report when the guest is silent")
	require.NotNil(t, doc.Allocation, "the Nomad facts are still in the document")
}

// TestTopOfASilentGuestFails: box top has nothing to show without the guest, so
// it says so, names what to look at, and exits as a timeout.
func TestTopOfASilentGuestFails(t *testing.T) {
	ta := silentGuest(t, "info")
	require.Equal(t, ExitTimeout, ta.run(t, "top", "dev1"))
	errOut := ta.errOut.String()
	require.Contains(t, errOut, "the guest does not answer (no reply in 20ms)")
	require.Contains(t, errOut, "`box logs dev1`")
	require.Contains(t, errOut, "`box status dev1`")
}

// TestTopOfASilentGuestInJSON carries the same fact as a field.
func TestTopOfASilentGuestInJSON(t *testing.T) {
	ta := silentGuest(t, "info")
	require.Equal(t, ExitTimeout, ta.run(t, "top", "dev1", "--json"))
	var doc topOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "dev1", doc.Box)
	require.Nil(t, doc.Usage)
	require.Contains(t, doc.GuestError, "the guest does not answer")
}

// TestSessionListOfASilentGuestFails: a session list cannot be guessed, so the
// command fails with a timeout and names box logs and box status.
func TestSessionListOfASilentGuestFails(t *testing.T) {
	ta := silentGuest(t, "session ls")
	require.Equal(t, ExitTimeout, ta.run(t, "session", "ls", "dev1"))
	errOut := ta.errOut.String()
	require.Contains(t, errOut, "the guest does not answer (no reply in 20ms): `boxctl session ls`")
	require.Contains(t, errOut, "`box logs dev1`")
	require.Contains(t, errOut, "`box status dev1`")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitTimeout, ta.run(t, "session", "ls", "dev1", "--json"))
	var doc sessionListOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "dev1", doc.Box)
	require.Contains(t, doc.GuestError, "the guest does not answer")
}

// TestStatusWithoutNomadFactsIsATimeout: when the guest was the whole of what a
// status had to show, its exit code says timeout; when Nomad knows something
// about the box, the silence costs a line and the status still succeeds.
func TestStatusWithoutNomadFactsIsATimeout(t *testing.T) {
	silent := &guestSilent{timeout: guestCallTimeout, what: "`boxctl info`"}
	empty := &statusOutput{Box: BoxRecord{ID: "dev1", State: "running"}}
	require.False(t, empty.showedNomadFacts(), "a box with no allocation, no volume and no events shows nothing Nomad knows")
	err := statusSilentError("dev1", silent, empty)
	require.Equal(t, ExitTimeout, ExitCode(err))
	require.Contains(t, err.Error(), "the guest does not answer (no reply in 20s)")
	require.Contains(t, err.Error(), "Nomad knows nothing else about it")

	require.NoError(t, statusSilentError("dev1", nil, empty), "a guest that answered needs no exit code")

	for name, out := range map[string]*statusOutput{
		"allocation": {Allocation: &statusAlloc{ID: "alloc-1"}},
		"volume":     {Volume: &statusVolume{Name: "box-dev1"}},
		"events":     {TaskEvents: []*api.TaskEvent{{Type: "Started"}}},
	} {
		require.True(t, out.showedNomadFacts(), "a status with a %s shows what Nomad knows", name)
		require.NoError(t, statusSilentError("dev1", silent, out), "a status with a %s still succeeds", name)
	}
}
