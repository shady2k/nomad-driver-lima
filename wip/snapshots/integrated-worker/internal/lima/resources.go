package lima

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"go.yaml.in/yaml/v3"
)

type ResourceObservation struct {
	CPUs            int    `json:"cpus"`
	MemoryBytes     int64  `json:"memory_bytes"`
	DiskBytes       int64  `json:"disk_bytes"`
	PartitionBytes  int64  `json:"partition_bytes"`
	FilesystemBytes int64  `json:"filesystem_bytes"`
	FSType          string `json:"fs_type"`
	FSUUID          string `json:"fs_uuid"`
	DirectPartition bool   `json:"direct_partition"`
}

// Native stopped-only edit. No --start, shell interpolation, qemu-img bypass,
// formatting, partition commands, or disk shrink are exposed here.
func (c *Client) EditResources(ctx context.Context, name string, cpus, memoryMiB, diskGiB int) error {
	if !validName.MatchString(name) || cpus < 1 || memoryMiB < 256 || diskGiB < 1 || diskGiB > 1048576 {
		return fmt.Errorf("invalid resource edit")
	}
	_, e := c.Runner.Run(ctx, nil, "edit", "--tty=false", "--cpus="+strconv.Itoa(cpus), "--set=.memory = \""+strconv.Itoa(memoryMiB)+"MiB\"", "--disk="+strconv.Itoa(diskGiB), name)
	return e
}

// Full semantic fingerprint except the three explicitly mutable resource fields.
// Unknown settings remain covered, including ownership labels and mounts.
func (c *Client) NonResourceConfigHash(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid instance name")
	}
	p := filepath.Join(c.Home, name, "lima.yaml")
	st, e := os.Lstat(p)
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return "", fmt.Errorf("regular bounded Lima config required")
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return "", e
	}
	var doc map[string]any
	if yaml.Unmarshal(b, &doc) != nil || doc == nil {
		return "", fmt.Errorf("invalid Lima configuration")
	}
	for _, k := range []string{"cpus", "memory", "disk"} {
		delete(doc, k)
	}
	canonical, e := json.Marshal(doc)
	if e != nil {
		return "", e
	}
	return Hash(canonical), nil
}
