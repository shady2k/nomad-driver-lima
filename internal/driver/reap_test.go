package driver

// The reap task reaches the Nomad API through the task API. Its address and its
// token are in the environment Nomad builds for the *task*, not in the plugin
// process's own environment: the plugin is one process for every task of the
// node, and it must not answer a task's request with its own environment.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"
)

// TestReapTaskTakesItsAddressAndTokenFromTheTaskEnvironment: the two variables
// Nomad sets for a task with `identity { env = true }` are read from the task.
func TestReapTaskTakesItsAddressAndTokenFromTheTaskEnvironment(t *testing.T) {
	tc := &drivers.TaskConfig{Env: map[string]string{
		envTaskAPIAddr:  "unix:///alloc/secrets/api.sock",
		envTaskAPIToken: "a-task-identity-token",
	}}
	getenv := taskEnvLookup(tc)
	// The address stays as it is when this process cannot see the socket: the
	// connection error then names the path Nomad gave.
	require.Equal(t, "unix:///alloc/secrets/api.sock", getenv(envTaskAPIAddr))
	require.Equal(t, "a-task-identity-token", getenv(envTaskAPIToken))

	client, err := nomadClientFromTask(getenv)
	require.NoError(t, err)
	require.NotNil(t, client)
}

// TestReapTaskDialsTheHostPathOfTheTaskAPISocket: Nomad builds the address for
// the task's filesystem isolation, which is unix:///secrets/api.sock for a driver
// that declares image isolation. The plugin runs on the host, where the socket is
// <alloc dir>/<task>/secrets/api.sock, and that is what it dials.
func TestReapTaskDialsTheHostPathOfTheTaskAPISocket(t *testing.T) {
	allocDir := t.TempDir()
	secrets := filepath.Join(allocDir, "reap", "secrets")
	require.NoError(t, os.MkdirAll(secrets, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "api.sock"), nil, 0o600))

	tc := &drivers.TaskConfig{Name: "reap", AllocDir: allocDir, Env: map[string]string{
		envTaskAPIAddr:  "unix:///secrets/api.sock",
		envTaskAPIToken: "a-task-identity-token",
	}}
	require.Equal(t, "unix://"+filepath.Join(secrets, "api.sock"), taskEnvLookup(tc)(envTaskAPIAddr))

	// A host path that exists is left alone.
	tc.Env[envTaskAPIAddr] = "unix://" + filepath.Join(secrets, "api.sock")
	require.Equal(t, "unix://"+filepath.Join(secrets, "api.sock"), taskEnvLookup(tc)(envTaskAPIAddr))
}

// TestReapTaskWithoutAnIdentitySaysHowToGetOne: the plugin's own environment is
// not consulted, so a job that forgot `identity { env = true }` fails with the
// sentence that says what to add, not with a token that is not the task's.
func TestReapTaskWithoutAnIdentitySaysHowToGetOne(t *testing.T) {
	t.Setenv(envTaskAPIToken, "a-token-from-whoever-started-the-plugin")
	t.Setenv(envTaskAPIAddr, "http://127.0.0.1:4646")

	_, err := nomadClientFromTask(taskEnvLookup(&drivers.TaskConfig{Env: map[string]string{}}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "identity { env = true }")

	_, err = nomadClientFromTask(taskEnvLookup(nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "identity { env = true }")
}
