package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// A bake and a publish are tasks of the box driver that run no VM: the first turns
// the disk of a sealed box into an image in the node's cache, the second puts such
// an image into the operator's store. The CLI registers them as batch jobs, which
// is what makes them one step with a beginning, an end and a log, and gives the
// operator `nomad job status` when something goes wrong.

const (
	// bakeCPUShares and bakeMemoryMiB are what a bake reserves: qemu-img uses a
	// core and a little memory for the compression, and the job must fit on a node
	// that runs boxes.
	bakeCPUShares = 1000
	bakeMemoryMiB = 512
	// publishCPUShares and publishMemoryMiB are what a publish reserves: it
	// uploads a file and hashes it.
	publishCPUShares = 500
	publishMemoryMiB = 256
)

// BakeJobSpec is the input of RenderBakeJob.
type BakeJobSpec struct {
	// BoxID names the sealed box whose disk is baked; its volume box-<id> binds
	// the job to the node that holds the disk.
	BoxID string
	// Name, Version, Parent and Arch name the image the bake makes.
	Name    string
	Version string
	Parent  string
	Arch    string
	// Recipe is the recipe the image carries, base64.
	Recipe string
}

// RenderBakeJob builds the batch job that bakes a sealed box into an image.
//
// The job is not a box: its meta says it is a step of an image's life, so
// `box ls`, the capacity count and the box id of a job name do not take it for
// one. It mounts the box's volume read-only, which is also what places it on the
// node that holds the disk.
func RenderBakeJob(spec BakeJobSpec) (*api.Job, error) {
	if err := ValidateBoxID(spec.BoxID); err != nil {
		return nil, err
	}
	if err := ValidateImageName(spec.Name); err != nil {
		return nil, err
	}
	if err := ValidateImageVersion(spec.Version, spec.Name); err != nil {
		return nil, err
	}
	switch NormalizeArch(spec.Arch) {
	case "amd64", "arm64":
	case "":
		return nil, usagef("the bake of box %s needs an architecture: use amd64 or arm64", spec.BoxID)
	default:
		return nil, usagef("architecture %q is not supported: use amd64 or arm64", spec.Arch)
	}
	arch := NormalizeArch(spec.Arch)

	bake := map[string]any{
		"name":    spec.Name,
		"version": spec.Version,
		"arch":    arch,
	}
	if spec.Parent != "" {
		bake["parent"] = spec.Parent
	}
	if spec.Recipe != "" {
		bake["recipe"] = spec.Recipe
	}
	jobID := BoxBakeJobName(spec.BoxID)
	task := &api.Task{
		Name:   boxTaskName,
		Driver: boxDriverName,
		Config: map[string]any{
			"box_id": spec.BoxID,
			"bake":   bake,
		},
		Resources: &api.Resources{CPU: intPtr(bakeCPUShares), MemoryMB: intPtr(bakeMemoryMiB)},
		VolumeMounts: []*api.VolumeMount{{
			Volume:      strPtr(boxDiskVolume),
			Destination: strPtr(boxDiskMount),
		}},
		// A bake that fails must fail: a restart would convert the disk again for
		// the same answer.
		KillTimeout: durPtr(boxStopTimeout),
	}
	return &api.Job{
		ID:          strPtr(jobID),
		Name:        strPtr(jobID),
		Type:        strPtr("batch"),
		Datacenters: []string{"*"},
		Constraints: []*api.Constraint{{
			LTarget: "${attr.driver.box.arch}",
			Operand: "=",
			RTarget: LimaArch(arch),
		}},
		Meta: map[string]string{
			boxWorkMarker: "bake",
			"box_id":      spec.BoxID,
			"image":       spec.Version,
			"arch":        arch,
		},
		TaskGroups: []*api.TaskGroup{{
			Name:  strPtr("bake"),
			Count: intPtr(1),
			RestartPolicy: &api.RestartPolicy{
				Attempts: intPtr(0),
				Mode:     strPtr("fail"),
			},
			Volumes: map[string]*api.VolumeRequest{
				boxDiskVolume: {
					Name:     boxDiskVolume,
					Type:     "host",
					Source:   BoxVolumeName(spec.BoxID),
					ReadOnly: true,
				},
			},
			Tasks: []*api.Task{task},
		}},
	}, nil
}

// PublishJobSpec is the input of RenderPublishJob.
type PublishJobSpec struct {
	Version string
	Arch    string
	SHA256  string
	// NodeID is the node that holds the draft: a publish reads the image from that
	// node's cache, so the job is pinned to it.
	NodeID string
	// StoreItems are the items of the store Variable the job renders into its
	// secrets: "url", and "token" or "token.tmpl" (a template that reads the token
	// from Vault at render time).
	StoreItems []ExecutorItem
}

// RenderPublishJob builds the batch job that publishes one image.
//
// The store's url and token reach the task as templated files, never as task
// config: a job spec is readable by anyone with read-job.
func RenderPublishJob(spec PublishJobSpec) (*api.Job, error) {
	if err := ValidateImageVersion(spec.Version, ""); err != nil {
		return nil, err
	}
	switch NormalizeArch(spec.Arch) {
	case "amd64", "arm64":
	case "":
		return nil, usagef("publishing %s needs an architecture: use amd64 or arm64", spec.Version)
	default:
		return nil, usagef("architecture %q is not supported: use amd64 or arm64", spec.Arch)
	}
	sum := strings.ToLower(strings.TrimSpace(spec.SHA256))
	if len(sum) != 64 {
		return nil, usagef("publishing %s needs the image's 64 character sha256, got %q", spec.Version, spec.SHA256)
	}
	if strings.TrimSpace(spec.NodeID) == "" {
		return nil, usagef("publishing %s needs the node that holds the draft", spec.Version)
	}
	jobID := BoxPublishJobName(spec.Version)
	task := &api.Task{
		Name:   boxTaskName,
		Driver: boxDriverName,
		Config: map[string]any{
			"publish": map[string]any{
				"version": spec.Version,
				"arch":    NormalizeArch(spec.Arch),
				"sha256":  sum,
			},
		},
		Resources:   &api.Resources{CPU: intPtr(publishCPUShares), MemoryMB: intPtr(publishMemoryMiB)},
		Templates:   storeTemplates(spec.StoreItems),
		KillTimeout: durPtr(boxStopTimeout),
		Vault:       storeVaultBlock(spec.StoreItems),
	}
	return &api.Job{
		ID:          strPtr(jobID),
		Name:        strPtr(jobID),
		Type:        strPtr("batch"),
		Datacenters: []string{"*"},
		// The image is in the cache of the node that baked it: nowhere else.
		Constraints: []*api.Constraint{{
			LTarget: "${node.unique.id}",
			Operand: "=",
			RTarget: spec.NodeID,
		}},
		Meta: map[string]string{
			boxWorkMarker: "publish",
			"image":       spec.Version,
			"arch":        NormalizeArch(spec.Arch),
		},
		TaskGroups: []*api.TaskGroup{{
			Name:  strPtr("publish"),
			Count: intPtr(1),
			RestartPolicy: &api.RestartPolicy{
				Attempts: intPtr(0),
				Mode:     strPtr("fail"),
			},
			Tasks: []*api.Task{task},
		}},
	}, nil
}

// storeDirInTask is where the store's files land in the task directory. The driver
// reads them from the task's secrets directory; nothing is in the job but names.
const storeDirInTask = "store"

// storeTemplateKeys are the items of the store Variable a publish renders, in the
// order they are rendered.
var storeTemplateKeys = []string{"url", "token", "token.tmpl"}

// storeTemplates turns the items of the store Variable into one task template per
// file, the way the executor credentials reach a box: a plain item is read out of
// the Variable at render time, a ".tmpl" item is a template that reads the token
// from Vault, so neither the job nor the Variable has to hold it.
func storeTemplates(items []ExecutorItem) []*api.Template {
	byKey := map[string]ExecutorItem{}
	for _, item := range items {
		byKey[item.Key] = item
	}
	var out []*api.Template
	seen := map[string]bool{}
	for _, key := range storeTemplateKeys {
		item, ok := byKey[key]
		if !ok {
			continue
		}
		dest := storeDirInTask + "/" + templateDestKey(key)
		if seen[dest] {
			// A plain token and a token template both render to store/token: the
			// template item wins, because it is the one that holds no value.
			continue
		}
		seen[dest] = true
		text := item.Text
		if !isTemplateKey(key) {
			text = variableTemplateText(key)
		}
		mode := "noop"
		perms := "0600"
		out = append(out, &api.Template{
			DestPath:     &dest,
			EmbeddedTmpl: &text,
			ChangeMode:   &mode,
			Perms:        &perms,
		})
	}
	return out
}

// storeVaultBlock gives a publish task a Vault block when its token is a template
// that reads one: without the block Nomad has no token to render it with.
func storeVaultBlock(items []ExecutorItem) *api.Vault {
	for _, item := range items {
		if item.Key == "token.tmpl" {
			mode := "noop"
			return &api.Vault{Role: strings.TrimSpace(item.Text), ChangeMode: &mode, DisableFile: boolPtr(true), Env: boolPtr(false)}
		}
	}
	return nil
}

// variableTemplateText is the template that reads one item out of the store
// Variable, the way a box reads its executor credentials.
func variableTemplateText(key string) string {
	return fmt.Sprintf(
		`{{- if nomadVarExists %q -}}{{- with nomadVar %q -}}{{- (index . %q).Value -}}{{- end -}}{{- end -}}`,
		boxStoreVariable, boxStoreVariable, key)
}

// workEvents returns the events a bake or a publish emitted, newest first: the
// driver passes its own data in the event's Details (the annotations of a driver
// event land there, beside the message).
func workEvents(alloc *api.Allocation, kind string) []*api.TaskEvent {
	if alloc == nil {
		return nil
	}
	state, ok := alloc.TaskStates[boxTaskName]
	if !ok {
		return nil
	}
	var out []*api.TaskEvent
	for i := len(state.Events) - 1; i >= 0; i-- {
		ev := state.Events[i]
		if ev == nil || ev.Details == nil || ev.Details["kind"] != kind {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// bakeResult is what a bake reported: the image it made, from the event the driver
// emitted for it. The event carries the fields of the record, because the CLI
// cannot read the node's image cache.
type bakeResult struct {
	Version      string `json:"version"`
	Arch         string `json:"arch"`
	Name         string `json:"name,omitempty"`
	Parent       string `json:"parent,omitempty"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	VirtualBytes int64  `json:"virtual_bytes"`
}

// bakeResultOf reads the result of a bake out of its allocation: the driver's last
// event that carries the image's digest is the one that made it.
func bakeResultOf(alloc *api.Allocation) (*bakeResult, bool) {
	for _, ev := range workEvents(alloc, "bake") {
		if ev.Details["sha256"] == "" {
			continue
		}
		res := &bakeResult{
			Version: ev.Details["version"],
			Arch:    ev.Details["arch"],
			SHA256:  ev.Details["sha256"],
			Name:    ev.Details["name"],
			Parent:  ev.Details["parent"],
		}
		res.Size, _ = strconv.ParseInt(ev.Details["size"], 10, 64)
		res.VirtualBytes, _ = strconv.ParseInt(ev.Details["virtual_bytes"], 10, 64)
		return res, true
	}
	return nil, false
}

// publishResultOf reads the result of a publish out of its allocation.
func publishResultOf(alloc *api.Allocation) (map[string]string, bool) {
	for _, ev := range workEvents(alloc, "publish") {
		if ev.Details["index"] == "" {
			continue
		}
		return ev.Details, true
	}
	return nil, false
}
