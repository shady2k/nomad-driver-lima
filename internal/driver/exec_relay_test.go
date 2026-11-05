package driver

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// relayRun runs a command through guestRelay with captured streams, the way ssh
// holds them: Run returns only when nothing has them open.
func relayRun(t *testing.T, stdin string, command ...string) (string, string, int, time.Duration) {
	t.Helper()
	argv := relayedCommand(command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	started := time.Now()
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code, time.Since(started)
}

func TestRelayPassesStreamsAndExitCode(t *testing.T) {
	out, errOut, code, took := relayRun(t, "from stdin\n", "sh", "-c", `cat; echo to-err >&2; exit 7`)
	require.Equal(t, "from stdin\n", out)
	require.Equal(t, "to-err\n", errOut)
	require.Equal(t, 7, code)
	require.Less(t, took, time.Second, "a command with nothing in the background takes no grace")
}

func TestRelayKeepsArguments(t *testing.T) {
	out, _, code, _ := relayRun(t, "", "printf", "%s|", "a b", `c"d`, "$e")
	require.Equal(t, 0, code)
	require.Equal(t, `a b|c"d|$e|`, out)
}

func TestRelayBinaryOutputIsIntact(t *testing.T) {
	out, _, code, _ := relayRun(t, "", "sh", "-c", `head -c 2000000 /dev/zero | tr '\0' 'x'; printf '\001\377'`)
	require.Equal(t, 0, code)
	require.Len(t, out, 2000002)
	require.True(t, strings.HasSuffix(out, "\x01\xff"))
}

// The bug this exists for: a background process that keeps the streams open must
// not hold the exec.
func TestRelayDoesNotWaitForABackgroundProcess(t *testing.T) {
	out, _, code, took := relayRun(t, "", "sh", "-c", `sleep 30 & echo started; exit 3`)
	require.Equal(t, "started\n", out)
	require.Equal(t, 3, code)
	require.Less(t, took, 10*time.Second)
}

func TestRelayFallsBackWithoutAPlaceForFifos(t *testing.T) {
	argv := relayedCommand([]string{"sh", "-c", "echo plain; exit 4"})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"TMPDIR=/nonexistent/dir", "PATH=/run/current-system/sw/bin:/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 4, exitErr.ExitCode())
	require.Equal(t, "plain\n", string(out))
}
