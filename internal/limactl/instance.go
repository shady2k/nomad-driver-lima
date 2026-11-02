// Package limactl is a thin, testable wrapper around the Lima command line.
//
// Every call shells out to the `limactl` binary with LIMA_HOME pointed at the
// driver's Lima home directory. The wrapper knows nothing about Nomad: it only
// encodes how Lima 2.x reports instance state and which flags we pass.
package limactl

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Lima instance status values, from github.com/lima-vm/lima/v2/pkg/limatype
// (pkg/limatype/lima_instance.go, measured against limactl 2.2.0).
const (
	StatusUnknown       = ""
	StatusUninitialized = "Uninitialized"
	StatusInstalling    = "Installing"
	StatusBroken        = "Broken"
	StatusStopped       = "Stopped"
	StatusRunning       = "Running"
)

// Instance is the JSON shape that `limactl list --format json` prints, one
// object per line. Only the fields the driver uses are modelled; unknown
// fields are ignored by encoding/json.
type Instance struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Status        string `json:"status"`
	Dir           string `json:"dir"`
	VMType        string `json:"vmType"`
	Arch          string `json:"arch"`
	CPUs          int    `json:"cpus"`
	Memory        int64  `json:"memory"` // bytes
	Disk          int64  `json:"disk"`   // bytes
	Message       string `json:"message"`
	SSHLocalPort  int    `json:"sshLocalPort"`
	SSHConfigFile string `json:"sshConfigFile"`
	HostAgentPID  int    `json:"hostAgentPID"`
	DriverPID     int    `json:"driverPID"` // the qemu / vz process
	Errors        Errors `json:"errors"`
	LimaVersion   string `json:"limaVersion"`
}

// Errors is Lima's error list of one instance. Lima prints it as an array of
// strings in some versions and as an array of {Op, Path, Err} objects in others
// (2.2.0 prints the objects for a broken instance), and one instance in an
// unexpected shape must not make the whole list unreadable: the driver lists the
// instances to find every box on the node.
type Errors []string

// UnmarshalJSON accepts an array of strings, an array of objects with
// Op/Path/Err, and a single string.
func (e *Errors) UnmarshalJSON(data []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		var one string
		if err2 := json.Unmarshal(data, &one); err2 == nil {
			*e = Errors{one}
			return nil
		}
		return fmt.Errorf("instance errors: %w", err)
	}
	out := make(Errors, 0, len(items))
	for _, item := range items {
		var text string
		if err := json.Unmarshal(item, &text); err == nil {
			out = append(out, text)
			continue
		}
		var obj struct {
			Op   string          `json:"Op"`
			Path string          `json:"Path"`
			Err  json.RawMessage `json:"Err"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			out = append(out, string(item))
			continue
		}
		msg := strings.TrimSpace(obj.Op + " " + obj.Path)
		if detail := strings.Trim(strings.TrimSpace(string(obj.Err)), `"`); detail != "" && detail != "null" {
			msg = strings.TrimSpace(msg + ": " + detail)
		}
		if msg == "" {
			msg = string(item)
		}
		out = append(out, msg)
	}
	*e = out
	return nil
}

// IsRunning reports whether Lima considers the instance up.
func (i *Instance) IsRunning() bool { return i.Status == StatusRunning }

// IsBroken reports whether Lima lost control of the instance (for example
// because the hostagent was killed while qemu kept running).
func (i *Instance) IsBroken() bool { return i.Status == StatusBroken }

// StatusMessage returns a human readable status, including Lima's own message
// when it set one.
func (i *Instance) StatusMessage() string {
	if i.Message != "" {
		return fmt.Sprintf("%s (%s)", i.Status, i.Message)
	}
	if len(i.Errors) > 0 {
		return fmt.Sprintf("%s (%s)", i.Status, strings.Join(i.Errors, "; "))
	}
	return i.Status
}

// MemoryBytes returns the configured guest memory in bytes.
func (i *Instance) MemoryBytes() int64 { return i.Memory }

// DiskBytes returns the configured disk size in bytes.
func (i *Instance) DiskBytes() int64 { return i.Disk }

// InstanceDir returns the directory Lima keeps for the instance.
func (i *Instance) InstanceDir(home string) string {
	if i.Dir != "" {
		return i.Dir
	}
	return filepath.Join(home, i.Name)
}

// parseList decodes `limactl list --format json` output. Lima prints JSON Lines
// (one object per line); an empty list prints "No instance found" to stderr and
// nothing to stdout. A JSON array is accepted too, which keeps the fake limactl
// used by the unit tests simple.
func parseList(stdout []byte) ([]*Instance, error) {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []*Instance
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("parse limactl list output as array: %w", err)
		}
		return list, nil
	}
	var out []*Instance
	dec := json.NewDecoder(strings.NewReader(trimmed))
	for {
		var inst Instance
		err := dec.Decode(&inst)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("parse limactl list output: %w", err)
		}
		copied := inst
		out = append(out, &copied)
	}
	return out, nil
}

// parseVersion extracts the version from `limactl --version` ("limactl version 2.2.0").
func parseVersion(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.EqualFold(f, "version") && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return strings.TrimSpace(stdout)
}
