package lima

import (
	"context"
	"encoding/json"
	"fmt"
	scriptassets "nomad-driver-lima/plugin/scripts"
)

func (c *Client) DeliverSecretProfiles(ctx context.Context, name string, p map[string]any) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid secret profile instance")
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	program := scriptassets.SecretProfiles + "\n" + scriptassets.SecretOperation + "\n" + scriptassets.SecretDelivery
	out, e := c.Runner.Run(ctx, raw, "shell", "--workdir=/", name, "--", "/usr/bin/python3", "-c", program)
	var receipt struct {
		OK bool `json:"ok"`
	}
	if e != nil || json.Unmarshal(out, &receipt) != nil || !receipt.OK {
		return fmt.Errorf("secret profile delivery unconfirmed")
	}
	return nil
}
