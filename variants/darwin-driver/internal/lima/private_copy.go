package lima

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed private_copy_guest.py
var privateCopyGuest string

func (c *Client) PreparePrivateCopy(ctx context.Context, name string, payload map[string]any) (map[string]any, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("private copy name invalid")
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return nil, e
	}
	out, e := c.Runner.Run(ctx, raw, "shell", "--workdir=/", name, "--", "sudo", "-n", "/usr/bin/python3", "-c", privateCopyGuest)
	var value map[string]any
	if json.Unmarshal(out, &value) != nil {
		return nil, fmt.Errorf("private copy preparation transport outcome unknown")
	}
	if e != nil || value["error"] != nil {
		return nil, fmt.Errorf("private copy preparation unconfirmed; VM/data retained")
	}
	return value, nil
}
