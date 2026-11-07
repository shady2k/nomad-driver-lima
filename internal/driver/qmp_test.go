package driver

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// serveQMP listens on <dir>/qmp.sock and answers the way behaviour says. It is a
// real unix socket and real QMP framing: the point of the probe is the socket, so
// a fake in process memory would prove nothing.
func serveQMP(t *testing.T, dir string, behaviour func(conn net.Conn)) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, qmpSocketName)
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				behaviour(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return path
}

// qmpAnswers is a qemu that greets, takes the capabilities command and answers
// query-status with status. An event is sent before the reply on purpose: a QMP
// client has to skip what is not its answer.
func qmpAnswers(status string) func(net.Conn) {
	return func(conn net.Conn) {
		write := func(line string) {
			_, _ = conn.Write([]byte(line + "\n"))
		}
		write(`{"QMP":{"version":{"qemu":{"major":9,"minor":0,"micro":0}},"capabilities":[]}}`)
		buf := make([]byte, 4096)
		for i := 0; i < 2; i++ {
			if _, err := conn.Read(buf); err != nil {
				return
			}
			if i == 0 {
				write(`{"return":{}}`)
				continue
			}
			write(`{"timestamp":{"seconds":1,"microseconds":0},"event":"RESUME"}`)
			reply, _ := json.Marshal(map[string]any{"return": map[string]any{
				"status": status, "running": status == qemuStateRunning, "singlestep": false,
			}})
			write(string(reply))
		}
	}
}

// qmpSilent accepts the connection and never says anything, which is a qemu that
// is busy with another client or stuck: the probe has to end on its deadline.
func qmpSilent(conn net.Conn) {
	<-make(chan struct{})
}

func TestQEMUStatusOf(t *testing.T) {
	cases := []struct {
		name      string
		behaviour func(net.Conn)
		// noListener leaves the socket file of a closed listener behind, so the
		// connection is refused.
		noListener bool
		want       string
		wantOK     bool
	}{
		{name: "running", behaviour: qmpAnswers(qemuStateRunning), want: qemuStateRunning, wantOK: true},
		{name: "io-error", behaviour: qmpAnswers(qemuStateIOError), want: qemuStateIOError, wantOK: true},
		{name: "paused", behaviour: qmpAnswers("paused"), want: "paused", wantOK: true},
		{name: "postmigrate", behaviour: qmpAnswers("postmigrate"), want: "postmigrate", wantOK: true},
		{
			name: "garbage",
			behaviour: func(conn net.Conn) {
				_, _ = conn.Write([]byte("not json at all\n"))
			},
			wantOK: false,
		},
		{
			name: "an answer that is not a status",
			behaviour: func(conn net.Conn) {
				_, _ = conn.Write([]byte("{\"QMP\":{}}\n"))
				buf := make([]byte, 4096)
				for i := 0; i < 2; i++ {
					if _, err := conn.Read(buf); err != nil {
						return
					}
					_, _ = conn.Write([]byte("{\"return\":{}}\n"))
				}
			},
			wantOK: false,
		},
		{name: "accepts and never answers", behaviour: qmpSilent, wantOK: false},
		{name: "connection refused", noListener: true, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.noListener {
				// A listener that is gone leaves its socket file: dialling it is
				// refused, which is a qemu that is not up.
				path := serveQMP(t, dir, qmpAnswers(qemuStateRunning))
				require.NoError(t, os.Remove(path))
			} else if tc.behaviour != nil {
				serveQMP(t, dir, tc.behaviour)
			}
			start := time.Now()
			status, ok := qemuStatusOf(dir, 500*time.Millisecond)
			elapsed := time.Since(start)
			require.Equal(t, tc.wantOK, ok, "status %q", status)
			require.Equal(t, tc.want, status)
			require.Less(t, elapsed, 3*time.Second, "a probe that does not answer must end on its deadline")
		})
	}

	// No socket file at all: a vz box, or a VM that is not up. It is unknown
	// without waiting for anything.
	start := time.Now()
	_, ok := qemuStatusOf(t.TempDir(), time.Second)
	require.False(t, ok)
	require.Less(t, time.Since(start), time.Second)
	_, ok = qemuStatusOf("", time.Second)
	require.False(t, ok)
}

func TestQEMUStatusFromReply(t *testing.T) {
	cases := []struct {
		reply  string
		want   string
		wantOK bool
	}{
		{`{"status":"running","running":true,"singlestep":false}`, "running", true},
		{`{"status":"io-error","running":false,"singlestep":false}`, "io-error", true},
		{`{"running":false}`, "", false},
		{`not json`, "", false},
		{``, "", false},
	}
	for _, tc := range cases {
		status, ok := qemuStatusFromReply([]byte(tc.reply))
		require.Equal(t, tc.wantOK, ok, tc.reply)
		require.Equal(t, tc.want, status, tc.reply)
	}
}

// TestQEMUStateEventOncePerTransition: a VM that stays paused is one event, and a
// VM that runs again is one more.
func TestQEMUStateEventOncePerTransition(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	tc := taskConfig(t, filepath.Join(t.TempDir(), "volume"))
	h := newTaskHandle(tc, TaskConfig{BoxID: "box1"})
	d.tasks.Add(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := d.TaskEvents(ctx)
	require.NoError(t, err)

	next := func() *drivers.TaskEvent {
		t.Helper()
		select {
		case event := <-events:
			return event
		case <-time.After(2 * time.Second):
			return nil
		}
	}

	d.applyVMStates(map[string]string{"box1": qemuStateIOError})
	event := next()
	require.NotNil(t, event, "a VM qemu paused is one event")
	require.Contains(t, event.Message, "paused by qemu (io-error)")
	require.Contains(t, event.Message, "is free on the node", "the free space of the node is part of the sentence")

	// The same state again is not news.
	d.applyVMStates(map[string]string{"box1": qemuStateIOError})
	require.Nil(t, next(), "one event per transition, not one per probe")

	// Running again is a transition of its own.
	d.applyVMStates(map[string]string{"box1": qemuStateRunning})
	event = next()
	require.NotNil(t, event)
	require.Contains(t, event.Message, "runs again")
	require.NotContains(t, event.Message, "io-error")

	// A VM that is not paused, and was not, says nothing at all.
	d.applyVMStates(map[string]string{"box1": qemuStateRunning})
	require.Nil(t, next())
}

// TestFingerprintReportsAPausedVM: the qemu state is a per-instance attribute, and
// a VM qemu runs normally has none.
func TestFingerprintReportsAPausedVM(t *testing.T) {
	f := fakelima.New(t)
	d := newTestDriver(t, f)
	vol := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.MkdirAll(vol, 0o755))
	url := fakelima.ImageServer(t, "IMAGE")
	tc := taskConfig(t, vol)
	require.NoError(t, tc.EncodeConcreteDriverConfig(startConfig(t, url, sha256Hex("IMAGE"))))
	_, _, err := d.StartTask(tc)
	require.NoError(t, err)

	// A healthy VM has no key: a reader that sees one knows qemu is not running it.
	fp := d.buildFingerprint(context.Background())
	_, ok := fp.Attributes["driver.box.instance.box1"+instanceQEMUStateSuffix]
	require.False(t, ok, "a running VM publishes no qemu state: %v", fp.Attributes)

	d.applyVMStates(map[string]string{"box1": qemuStateIOError})
	fp = d.buildFingerprint(context.Background())
	require.Equal(t, qemuStateIOError, attrValue(t, fp, "driver.box.instance.box1"+instanceQEMUStateSuffix))

	// A VM that runs again loses the key: the fingerprint after the transition
	// sends it empty, which is how Nomad drops an attribute, and the one after
	// that does not name it.
	d.applyVMStates(map[string]string{"box1": qemuStateRunning})
	fp = d.buildFingerprint(context.Background())
	require.Equal(t, "", attrValue(t, fp, "driver.box.instance.box1"+instanceQEMUStateSuffix))
	fp = d.buildFingerprint(context.Background())
	_, ok = fp.Attributes["driver.box.instance.box1"+instanceQEMUStateSuffix]
	require.False(t, ok, "the key of a VM that runs again is not sent again")
}
