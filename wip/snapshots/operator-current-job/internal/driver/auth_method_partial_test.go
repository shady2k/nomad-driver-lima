package driver

import (
	"encoding/json"
	"github.com/hashicorp/nomad/nomad/structs"
	"testing"
	"time"
)

// Exercise the actual upstream merge called by ACL.UpsertAuthMethods, not a mock.
func TestUpstreamAuthMethodPartialTTLDoesNotReplaceConfig(t *testing.T) {
	for _, defaultValue := range []bool{true, false} {
		existing := &structs.ACLAuthMethod{Name: "synthetic-owner", Type: "OIDC", Default: defaultValue, TokenLocality: "local", MaxTokenTTL: time.Hour, Config: &structs.ACLAuthMethodConfig{OIDCClientSecret: "synthetic-not-a-live-credential"}}
		raw, e := json.Marshal(map[string]any{"Name": existing.Name, "MaxTokenTTL": "720h", "Default": defaultValue})
		if e != nil {
			t.Fatal(e)
		}
		var update structs.ACLAuthMethod
		if e = json.Unmarshal(raw, &update); e != nil {
			t.Fatal(e)
		}
		if update.Config != nil {
			t.Fatal("partial request included Config")
		}
		update.Merge(existing)
		if update.Config != existing.Config || update.Config.OIDCClientSecret != "synthetic-not-a-live-credential" || update.Default != defaultValue || update.MaxTokenTTL != 720*time.Hour {
			t.Fatal("partial update changed protected config/default")
		}
	}
}
