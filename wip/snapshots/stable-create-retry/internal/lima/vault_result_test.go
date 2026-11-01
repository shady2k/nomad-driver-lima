package lima

import (
	"context"
	"strings"
	"testing"
)

type vaultResultRunner struct{ result string }

func (r vaultResultRunner) Run(context.Context, []byte, ...string) ([]byte, error) {
	return []byte(r.result), nil
}
func TestVaultResultClassificationNeverTrusts403OrLeaksUnlistedCode(t *testing.T) {
	for _, tc := range []struct{ result, want string }{
		{`{"ok":false,"code":"TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED"}`, "TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED"},
		{`{"ok":false,"code":"OWNED_RUNTIME_STOP_UNCONFIRMED"}`, "OWNED_RUNTIME_STOP_UNCONFIRMED"},
		{`{"ok":false,"code":"synthetic-secret-value"}`, "task vault operation not confirmed"},
	} {
		c := Client{Runner: vaultResultRunner{tc.result}}
		e := c.RevokeVault(context.Background(), "owned-aaaaaaaaaaaa", "00000000-0000-4000-8000-000000000001", strings.Repeat("a", 32), "https://fixture.invalid")
		if e == nil || !strings.Contains(e.Error(), tc.want) || strings.Contains(e.Error(), "synthetic-secret-value") {
			t.Fatalf("unsafe result: %v", e)
		}
	}
}
