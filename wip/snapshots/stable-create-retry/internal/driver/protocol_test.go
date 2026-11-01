package driver

import (
	"context"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"testing"
	"time"
)

func TestNomadSDKWireLifecycle(t *testing.T) {
	f := newFake()
	f.phase = "exited"
	d := setup(t, f, "")
	conn, server := plugin.TestPluginGRPCConn(t, true, map[string]plugin.Plugin{base.PluginTypeBase: &base.PluginBase{Impl: d}, base.PluginTypeDriver: drivers.NewDriverPlugin(d, log.NewNullLogger())})
	defer conn.Close()
	defer server.Stop()
	raw, e := conn.Dispense(base.PluginTypeDriver)
	if e != nil {
		t.Fatal(e)
	}
	client := raw.(drivers.DriverPlugin)
	info, e := client.PluginInfo()
	if e != nil || info.Name != "lima" {
		t.Fatal(info, e)
	}
	if _, e = client.ConfigSchema(); e != nil {
		t.Fatal(e)
	}
	if _, e = client.TaskConfigSchema(); e != nil {
		t.Fatal(e)
	}
	fpctx, fpcancel := context.WithTimeout(context.Background(), time.Second)
	fingerprints, err := client.Fingerprint(fpctx)
	if err != nil {
		fpcancel()
		t.Fatal(err)
	}
	select {
	case fp := <-fingerprints:
		if fp == nil {
			fpcancel()
			t.Fatal("missing fingerprint")
		}
		scope, ok := fp.Attributes["driver.lima.metrics.memory.scope"].GetString()
		guest, known := fp.Attributes["driver.lima.metrics.memory.guest_usage"].GetBool()
		if !ok || scope != "host_vm_process_rss" || !known || guest {
			fpcancel()
			t.Fatal("wire metric scope lost")
		}
	case <-fpctx.Done():
		fpcancel()
		t.Fatal("fingerprint timeout")
	}
	fpcancel()
	cfg := fixture(t)
	h, _, e := client.StartTask(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if h.Config.ID != cfg.ID {
		t.Fatal("task binding lost")
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	wait, e := client.WaitTask(ctx, cfg.ID)
	if e != nil {
		t.Fatal(e)
	}
	if r := <-wait; r == nil || r.ExitCode != 7 {
		t.Fatal(r)
	}
	if _, e = client.InspectTask(cfg.ID); e != nil {
		t.Fatal(e)
	}
	if e = client.DestroyTask(cfg.ID, false); e != nil {
		t.Fatal(e)
	}
}
