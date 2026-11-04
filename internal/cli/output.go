package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hashicorp/nomad/api"
)

// BoxRecord is one box as `box ls` and `box status` show it.
type BoxRecord struct {
	ID            string `json:"id"`
	Job           string `json:"job"`
	Node          string `json:"node,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Image         string `json:"image,omitempty"`
	State         string `json:"state"`
	JobStatus     string `json:"job_status,omitempty"`
	ClientStatus  string `json:"client_status,omitempty"`
	TaskState     string `json:"task_state,omitempty"`
	CPUs          int    `json:"cpus"`
	MemoryMiB     int    `json:"memory_mib"`
	DiskGiB       int    `json:"disk_gib"`
	AllocationID  string `json:"allocation_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	Volume        string `json:"volume,omitempty"`
	// VolumeCapacityBytes is how much Nomad reports the volume holds, or 0
	// when the volume is gone.
	VolumeCapacityBytes int64 `json:"volume_capacity_bytes,omitempty"`
	// DiskUsedBytes is what the box disk really occupies on its node, as the
	// box driver reports it; DiskVirtualBytes is the size it may grow to.
	DiskUsedBytes    int64 `json:"disk_used_bytes,omitempty"`
	DiskVirtualBytes int64 `json:"disk_virtual_bytes,omitempty"`
	// Note explains a state that needs a sentence.
	Note string `json:"note,omitempty"`
}

// printJSON writes one JSON document.
func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printTable writes a plain table with aligned columns.
func (a *App) printTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
}

// nodeDisks returns the per-box disk usage of one node, as the box driver
// reports it. The second value is the node's total used bytes.
//
// The lookup is cached in cache (node id -> disks) because `box ls` asks for
// every box and the answer is per node.
func (a *App) nodeDisks(ctx context.Context, cache map[string]map[string]BoxDisk, nodeID string) (map[string]BoxDisk, error) {
	if nodeID == "" {
		return nil, nil
	}
	if cache == nil {
		return nil, nil
	}
	if disks, ok := cache[nodeID]; ok {
		return disks, nil
	}
	node, err := a.Nomad.NodeInfo(ctx, nodeID)
	if err != nil {
		return nil, wrapf(err, "cannot read node %s", nodeID)
	}
	byBox := map[string]BoxDisk{}
	for _, disk := range BuildCapacity(node, nil).Disks {
		byBox[disk.BoxID] = disk
	}
	cache[nodeID] = byBox
	return byBox, nil
}

// note writes a sentence for a human reader; JSON output skips it.
func (a *App) note(format string, args ...any) {
	if a.JSON {
		return
	}
	fmt.Fprintf(a.ErrOut, format+"\n", args...)
}

// shortID shortens an allocation or session id for a table.
func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// dash renders an empty value in a table.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func dashInt(v int) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

// humanDuration renders a duration the way a person reads it.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// humanBytes renders a byte count in GiB or MiB.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(int64(1)<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/float64(int64(1)<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// isNotFoundErr reports whether Nomad answered 404.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := err.(interface{ StatusCode() int }); ok {
		return s.StatusCode() == 404
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(strings.ToLower(msg), "not found")
}

// isPermissionDeniedErr reports whether Nomad answered 403.
func isPermissionDeniedErr(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := err.(interface{ StatusCode() int }); ok {
		return s.StatusCode() == 403
	}
	return strings.Contains(err.Error(), "403")
}

// registerDenied adds to a refused job registration what the token lacks.
// Nomad answers a bare "Permission denied" on purpose and never names the
// missing rule, and submit-job is not the whole story: a job that mounts a host
// volume also needs the volume's own rule, which a token can lack while it has
// every namespace capability.
func registerDenied(err error, volume string) string {
	if !isPermissionDeniedErr(err) {
		return ""
	}
	return fmt.Sprintf(" -- the Nomad token needs submit-job in the box namespace and "+
		"host_volume %q with mount-readwrite (or a pattern such as \"box-*\"); "+
		"deploy/acl/box-operator.policy.hcl grants both", volume)
}

// jobAllocations groups the cluster allocations by job.
func (a *App) jobAllocations(ctx context.Context) (map[string][]*api.AllocationListStub, error) {
	stubs, err := a.Nomad.Allocations(ctx)
	if err != nil {
		return nil, wrapf(err, "cannot list the allocations")
	}
	byJob := map[string][]*api.AllocationListStub{}
	for _, stub := range stubs {
		if stub == nil {
			continue
		}
		byJob[stub.JobID] = append(byJob[stub.JobID], stub)
	}
	for _, list := range byJob {
		sort.Slice(list, func(i, j int) bool { return list[i].CreateIndex < list[j].CreateIndex })
	}
	return byJob, nil
}

// boxRecord assembles the record of one box from its job and allocations.
func boxRecord(job *api.Job, allocs []*api.AllocationListStub, now time.Time) BoxRecord {
	spec, err := SpecFromJob(job)
	rec := BoxRecord{
		ID:        spec.ID,
		Job:       derefStr(job.ID, derefStr(job.Name, "")),
		Arch:      spec.Arch,
		Image:     spec.ImageVersion,
		CPUs:      spec.CPUs,
		MemoryMiB: spec.MemoryMiB,
		DiskGiB:   spec.DiskGiB,
		Volume:    BoxVolumeName(spec.ID),
		JobStatus: derefStr(job.Status, ""),
	}
	if err != nil {
		rec.Note = err.Error()
	}
	if job.Stop != nil && *job.Stop {
		rec.State = "stopped"
	}
	if rec.Arch == "" && job.Meta != nil {
		rec.Arch = NormalizeArch(job.Meta["arch"])
	}

	alloc := latestAllocation(allocs)
	if alloc == nil {
		if rec.State == "" {
			rec.State = "stopped"
		}
		if rec.Note == "" {
			rec.Note = "the box has no allocation; `box start " + rec.ID + "` runs it again"
		}
		return rec
	}
	rec.AllocationID = alloc.ID
	rec.Node = alloc.NodeName
	rec.NodeID = alloc.NodeID
	rec.ClientStatus = alloc.ClientStatus
	if task := alloc.TaskStates[boxTaskName]; task != nil {
		rec.TaskState = task.State
		if !task.StartedAt.IsZero() && (task.FinishedAt.IsZero() || task.FinishedAt.After(task.StartedAt)) {
			end := task.FinishedAt
			if end.IsZero() {
				end = now
			}
			if d := end.Sub(task.StartedAt); d > 0 {
				rec.UptimeSeconds = int64(d.Seconds())
			}
		}
	}
	switch {
	case rec.ClientStatus == "running" && rec.TaskState == "running":
		rec.State = "running"
	case rec.ClientStatus == "pending":
		rec.State = "starting"
	case rec.ClientStatus == "unknown":
		rec.State = "unknown"
	case rec.ClientStatus == "failed":
		rec.State = "failed"
	case rec.ClientStatus == "complete":
		if rec.State == "" {
			rec.State = "stopped"
		}
	case rec.State == "":
		rec.State = rec.ClientStatus
	}
	if rec.State != "running" && rec.State != "stopped" && rec.Note == "" && alloc.ClientDescription != "" {
		rec.Note = oneLine(alloc.ClientDescription, 200)
	}
	return rec
}

// latestAllocation returns the newest allocation that is not complete.
func latestAllocation(allocs []*api.AllocationListStub) *api.AllocationListStub {
	var best *api.AllocationListStub
	for _, alloc := range allocs {
		if alloc == nil {
			continue
		}
		if alloc.ClientStatus == "complete" && best != nil {
			continue
		}
		if alloc.ClientStatus == "complete" {
			continue
		}
		if best == nil || alloc.CreateIndex > best.CreateIndex {
			best = alloc
		}
	}
	if best == nil && len(allocs) > 0 {
		// Everything is terminal: report the newest one so the state and the
		// task events are still visible.
		for _, alloc := range allocs {
			if alloc == nil {
				continue
			}
			if best == nil || alloc.CreateIndex > best.CreateIndex {
				best = alloc
			}
		}
	}
	return best
}

// collectBoxes returns every box on the cluster.
func (a *App) collectBoxes(ctx context.Context) ([]BoxRecord, error) {
	stubs, err := a.Nomad.JobList(ctx)
	if err != nil {
		return nil, wrapf(err, "cannot list the Nomad jobs")
	}
	byJob, err := a.jobAllocations(ctx)
	if err != nil {
		return nil, err
	}
	now := a.now()
	out := make([]BoxRecord, 0, len(stubs))
	for _, stub := range stubs {
		if stub == nil {
			continue
		}
		// The jobs list does not always carry the meta block, so the name is
		// the filter and the job record itself is the proof.
		if _, byName := BoxIDFromJobName(stub.ID); !byName && !IsBoxJobStub(stub) {
			continue
		}
		job, err := a.Nomad.Job(ctx, stub.ID)
		if err != nil {
			if isNotFoundErr(err) {
				continue
			}
			return nil, wrapf(err, "cannot read job %s", stub.ID)
		}
		out = append(out, boxRecord(job, byJob[stub.ID], now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// findBox returns one box, or a not-found error that says what to do.
func (a *App) findBox(ctx context.Context, id string) (BoxRecord, *api.Job, error) {
	if err := ValidateBoxID(id); err != nil {
		return BoxRecord{}, nil, err
	}
	job, err := a.Nomad.Job(ctx, BoxJobName(id))
	if err != nil {
		if isNotFoundErr(err) {
			return BoxRecord{}, nil, a.noSuchBox(ctx, id)
		}
		return BoxRecord{}, nil, wrapf(err, "cannot read the job %s", BoxJobName(id))
	}
	if !IsBoxJob(job) {
		return BoxRecord{}, nil, notFoundf("the job %s exists but is not a box (it has no meta.box = \"true\"); `box ls` lists the boxes",
			BoxJobName(id))
	}
	byJob, err := a.jobAllocations(ctx)
	if err != nil {
		return BoxRecord{}, nil, err
	}
	return boxRecord(job, byJob[BoxJobName(id)], a.now()), job, nil
}

// noSuchBox builds the not-found error, listing the boxes that do exist.
func (a *App) noSuchBox(ctx context.Context, id string) error {
	boxes, err := a.collectBoxes(ctx)
	if err != nil {
		return notFoundf("there is no box %q", id)
	}
	if len(boxes) == 0 {
		return notFoundf("there is no box %q; this cluster has no boxes yet: `box create` makes one", id)
	}
	ids := make([]string, 0, len(boxes))
	for _, box := range boxes {
		ids = append(ids, box.ID)
	}
	return notFoundf("there is no box %q; the boxes here are: %s", id, strings.Join(ids, ", "))
}

// runningAlloc returns the running allocation of a box, fetching the full
// record the exec API needs.
func (a *App) runningAlloc(ctx context.Context, box BoxRecord) (*api.Allocation, error) {
	if box.AllocationID == "" {
		return nil, errorf("the box %s has no allocation; run `box start %s` and try again", box.ID, box.ID)
	}
	if box.State != "running" {
		return nil, errorf("the box %s is %s, not running; the guest cannot be reached. `box start %s` starts it again",
			box.ID, box.State, box.ID)
	}
	alloc, err := a.Nomad.Allocation(ctx, box.AllocationID)
	if err != nil {
		return nil, wrapf(err, "cannot read allocation %s", box.AllocationID)
	}
	return alloc, nil
}

// boxctlFor finds a box, waits for it to run and returns its guest client.
//
// A guest command is asked for while the driver may still be starting or
// resizing the VM; waiting a little is better than an error the coordinator
// would answer with a retry loop.
func (a *App) boxctlFor(ctx context.Context, id string) (*Boxctl, BoxRecord, error) {
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return nil, box, err
	}
	box, err = a.waitRunning(ctx, box, guestWaitTimeout)
	if err != nil {
		return nil, box, err
	}
	client, err := a.boxctl(ctx, box)
	if err != nil {
		return nil, box, err
	}
	return client, box, nil
}

// boxctl returns the guest client of a box.
func (a *App) boxctl(ctx context.Context, box BoxRecord) (*Boxctl, error) {
	alloc, err := a.runningAlloc(ctx, box)
	if err != nil {
		return nil, err
	}
	tr, err := a.guestTransport(ctx, alloc)
	if err != nil {
		return nil, err
	}
	return NewBoxctl(tr), nil
}

// writeAll copies the whole stream to w. The caller has already checked the
// error the writer reported through its own channel; a short write here is a
// closed stream, which has nothing left to say.
func writeAll(w io.Writer, data []byte) {
	_, _ = w.Write(data)
}
