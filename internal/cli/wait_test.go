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
