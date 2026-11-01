package lima

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type recording struct {
	args     []string
	body     []byte
	response []byte
}

func (r *recording) Run(_ context.Context, b []byte, args ...string) ([]byte, error) {
	r.args = args
	r.body = b
	return r.response, nil
}
func TestCreateIsolationAndStructuredMetadata(t *testing.T) {
	r := &recording{}
	c := &Client{Runner: r}
	name := "nomad-" + strings.Repeat("a", 24)
	e := c.Create(t.Context(), Request{Name: name, Owner: strings.Repeat("b", 32), Key: strings.Repeat("c", 64), Template: "/operator/pinned.yaml", CPUs: 1, MemoryMiB: 512, DiskGiB: 8})
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(r.args, " ")
	probeFound := false
	for _, arg := range r.args {
		if strings.HasPrefix(arg, "--set=.probes += ") {
			var probes []map[string]string
			if e := json.Unmarshal([]byte(strings.TrimPrefix(arg, "--set=.probes += ")), &probes); e != nil {
				t.Fatal(e)
			}
			if len(probes) != 1 || !strings.Contains(probes[0]["script"], "{{.Param.nomad_driver_id}}") || !strings.Contains(probes[0]["script"], "{{.Param.nomad_task_key}}") {
				t.Fatal("ownership parameters must be referenced by a valid upstream probe")
			}
			probeFound = true
		}
	}
	if !probeFound {
		t.Fatal("unused ownership params rejected by upstream Lima")
	}

	for _, required := range []string{"--mount-none", "--plain", "forwardAgent = false", "loadDotSSHPubKeys = false", "propagateProxyEnv = false"} {
		if !strings.Contains(joined, required) {
			t.Fatal("missing", required)
		}
	}
	r.response = []byte(`{"name":"` + name + `","status":"Running","param":{"nomad_driver_id":"owner"}}`)
	v, e := c.Get(t.Context(), name)
	if e != nil || v.Param["nomad_driver_id"] != "owner" {
		t.Fatal(v, e)
	}
	r.response = []byte("not-json")
	if _, e = c.List(t.Context()); e == nil {
		t.Fatal("accepted malformed metadata")
	}
}
func TestPayloadNeverBecomesHostShellCommand(t *testing.T) {
	r := &recording{response: []byte(`{"phase":"running","stdout":"","stderr":""}`)}
	c := &Client{Runner: r}
	payload := map[string]any{"action": "launch", "command": []string{"/bin/sh", "-c", "echo $(not-on-host)"}}
	_, _, e := c.Guest(t.Context(), "nomad-"+strings.Repeat("a", 24), payload)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(strings.Join(r.args, " "), "$(not-on-host)") {
		t.Fatal("guest command leaked into host args")
	}
	var out map[string]any
	if json.Unmarshal(r.body, &out) != nil {
		t.Fatal("payload not stdin JSON")
	}
	if _, e = c.Get(t.Context(), "--host-option"); e == nil {
		t.Fatal("accepted CLI option as instance")
	}
}

func TestUpstreamSocketBudgetFailsBeforeCreate(t *testing.T) {
	if e := ValidateHome("/tmp/lima"); e != nil {
		t.Fatal(e)
	}
	if e := ValidateHome("/tmp/" + strings.Repeat("x", 100)); e == nil {
		t.Fatal("long socket path accepted")
	}
}
