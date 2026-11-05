package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// guestFor returns a boxctl wired to a fake guest with the given answers.
func guestFor(t *testing.T, responses map[string]string, failures map[string]boxctlAnswer) (*Boxctl, *fakeGuest) {
	t.Helper()
	guest := newFakeGuest(t)
	for key, value := range responses {
		guest.responses[key] = value
	}
	for key, value := range failures {
		guest.errors[key] = value
	}
	return NewBoxctl(guest), guest
}

func TestBoxctlInfo(t *testing.T) {
	boxctl, _ := guestFor(t, map[string]string{"info": `{
	  "boxd_version": "0.3.0",
	  "executors": [{"name": "prime", "version": "1.2.3", "available": true},
	                {"name": "claude", "version": "", "available": false}],
	  "sessions": 2, "mem_total_mib": 1024, "mem_free_mib": 700, "cpus": 2,
	  "unknown_field": true
	}`}, nil)
	info, err := boxctl.Info(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0.3.0", info.BoxdVersion)
	require.Equal(t, 2, info.Sessions)
	require.Equal(t, int64(700), info.MemFreeMiB)
	require.Len(t, info.Executors, 2)
	require.True(t, info.Executors[0].Available)
	require.False(t, info.Executors[1].Available)
}

func TestBoxctlErrorEnvelope(t *testing.T) {
	boxctl, _ := guestFor(t, nil, map[string]boxctlAnswer{
		"send s-1": {json: `{"error":{"code":"busy","message":"session s-1 is running turn 2"}}`, code: 1},
	})
	_, err := boxctl.Send(context.Background(), "s-1", "hello")
	require.Error(t, err)
	var bce *BoxctlError
	require.True(t, errors.As(err, &bce))
	require.Equal(t, "busy", bce.Code)
	require.Contains(t, err.Error(), "running turn 2")
	require.Equal(t, ExitError, ExitCode(err))
}

func TestBoxctlFailureWithoutDocument(t *testing.T) {
	boxctl, _ := guestFor(t, nil, nil)
	_, err := boxctl.Info(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "boxctl exited with code 1")
	require.Contains(t, err.Error(), "unknown command")
}

func TestBoxctlSessionNewArguments(t *testing.T) {
	boxctl, guest := guestFor(t, map[string]string{
		"session new --executor prime --model p/m --cwd /work --title fix": `{"session":"s-1"}`,
	}, nil)
	id, err := boxctl.SessionNew(context.Background(), SessionNewOptions{Executor: "prime", Model: "p/m", Cwd: "/work", Title: "fix"})
	require.NoError(t, err)
	require.Equal(t, "s-1", id)
	require.Equal(t, []string{boxctlPath, "session", "new", "--executor", "prime", "--model", "p/m", "--cwd", "/work", "--title", "fix"}, guest.calls[0])
}

func TestBoxctlSendPutsPromptOnStdin(t *testing.T) {
	boxctl, guest := guestFor(t, map[string]string{"send s-1": `{"session":"s-1","turn":3}`}, nil)
	turn, err := boxctl.Send(context.Background(), "s-1", "please fix the build")
	require.NoError(t, err)
	require.Equal(t, 3, turn)
	require.Equal(t, []string{"please fix the build"}, guest.stdin)
}

func TestBoxctlWaitArguments(t *testing.T) {
	boxctl, guest := guestFor(t, map[string]string{
		"wait s-1 --turn 2 --timeout 60": `{"session":"s-1","turn":2,"state":"done","text":"ok"}`,
	}, nil)
	turn, err := boxctl.Wait(context.Background(), "s-1", WaitOptions{Turn: 2, Timeout: 60})
	require.NoError(t, err)
	require.Equal(t, "done", turn.State)
	require.Equal(t, "ok", turn.Text)
	require.Equal(t, []string{boxctlPath, "wait", "s-1", "--turn", "2", "--timeout", "60"}, guest.calls[0])
}

func TestBoxctlEventsCursor(t *testing.T) {
	boxctl, _ := guestFor(t, map[string]string{
		"events s-1 --since 4 --limit 50": `{"events":[
		  {"seq":4,"ts":"2026-11-02T10:00:00Z","session":"s-1","turn":1,"kind":"message","summary":"thinking"},
		  {"seq":5,"ts":"2026-11-02T10:00:01Z","session":"s-1","turn":1,"kind":"tool","summary":"read a file"}
		],"next":6}`,
	}, nil)
	page, err := boxctl.Events(context.Background(), "s-1", EventsOptions{Since: 4, Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, int64(5), page.Events[1].Seq)
	require.Equal(t, "tool", page.Events[1].Kind)
	require.Equal(t, int64(6), page.Next)
	require.False(t, page.More, "a whole feed is not a page of one")

	// Without "next" the cursor is the highest sequence number plus one.
	boxctl, _ = guestFor(t, map[string]string{
		"events s-1": `{"events":[{"seq":9,"kind":"turn_end","summary":"done"}]}`,
	}, nil)
	page, err = boxctl.Events(context.Background(), "s-1", EventsOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(10), page.Next)

	// A page says how much of the feed it is.
	boxctl, _ = guestFor(t, map[string]string{
		"events s-1 --limit 2": `{"events":[{"seq":1},{"seq":2}],"next":3,"total":9,"more":true}`,
	}, nil)
	page, err = boxctl.Events(context.Background(), "s-1", EventsOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, 9, page.Total)
	require.True(t, page.More)
}

func TestBoxctlCancelAndClose(t *testing.T) {
	boxctl, guest := guestFor(t, map[string]string{
		"cancel s-1":        `{"session":"s-1","turn":2,"state":"cancelled"}`,
		"session close s-1": `{"session":"s-1","state":"closed"}`,
	}, nil)
	turn, err := boxctl.Cancel(context.Background(), "s-1")
	require.NoError(t, err)
	require.Equal(t, "cancelled", turn.State)
	require.NoError(t, boxctl.SessionClose(context.Background(), "s-1"))
	require.Len(t, guest.calls, 2)

	// A broken JSON document is reported, not silently ignored.
	boxctl, _ = guestFor(t, map[string]string{"info": "not json"}, nil)
	_, err = boxctl.Info(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not JSON")
}

func TestBoxctlTransformFailure(t *testing.T) {
	failing := &failingTransport{err: errors.New("connection closed")}
	_, err := NewBoxctl(failing).Info(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection closed")
}

type failingTransport struct{ err error }

func (f *failingTransport) Exec(ctx context.Context, opts ExecOpts) (int, error) {
	if opts.Stderr != nil {
		_, _ = opts.Stderr.Write([]byte(""))
	}
	return -1, f.err
}
