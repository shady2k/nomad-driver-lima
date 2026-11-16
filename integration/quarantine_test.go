//go:build integration || integration_cli

package integration

import (
	"os"
	"testing"
)

// quarantined skips a test that is known to fail in the Integration workflow.
// It is the one switch both integration suites use: with BOX_RUN_QUARANTINED=1
// the test runs as it did, otherwise the skip message names the task and why
// the test is parked.
func quarantined(t *testing.T, task, reason string) {
	t.Helper()
	if os.Getenv("BOX_RUN_QUARANTINED") == "1" {
		return
	}
	t.Skipf("quarantined %s: %s (set BOX_RUN_QUARANTINED=1 to run it)", task, reason)
}
