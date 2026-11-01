package lima

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	scriptassets "nomad-driver-lima/plugin/scripts"
)

//go:embed vault_guest.py
var vaultGuestProgram string

// VaultToken never puts payloads in argv, errors, diagnostics or task logs.
func (c *Client) VaultToken(ctx context.Context, name, session, owner, action, token string) error {
	return c.vaultOperation(ctx, name, session, owner, action, token, "")
}

// StopVaultRuntime stops recorded processes only. Its guest branch exits before
// token-history access or HTTP construction and never claims token revocation.
func (c *Client) StopVaultRuntime(ctx context.Context, name, session, owner string) error {
	return c.vaultOperation(ctx, name, session, owner, "stop-runtime", "", "")
}
func (c *Client) RevokeVault(ctx context.Context, name, session, owner, address string) error {
	return c.vaultOperation(ctx, name, session, owner, "revoke", "", address)
}
func (c *Client) vaultOperation(ctx context.Context, name, session, owner, action, token, address string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	payload, _ := json.Marshal(map[string]string{"session": session, "owner": owner, "action": action, "token": token, "address": address})
	out, err := c.Runner.Run(ctx, payload, "shell", "--workdir=/", name, "--", "/usr/bin/python3", "-c", scriptassets.SecretProfiles+"\n"+scriptassets.SecretOperation+"\n"+vaultGuestProgram)
	if err != nil {
		return fmt.Errorf("task vault delivery failed")
	}
	var result struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if json.Unmarshal(out, &result) != nil {
		return fmt.Errorf("task vault operation not confirmed; tokens/data retained")
	}
	if !result.OK {
		switch result.Code {
		case "OWNED_RUNTIME_STOP_UNCONFIRMED", "TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED", "TASK_TOKEN_REVOKE_UNCERTAIN":
			return fmt.Errorf("%s: tokens/data retained", result.Code)
		}
		return fmt.Errorf("task vault operation not confirmed; tokens/data retained")
	}
	return nil
}
