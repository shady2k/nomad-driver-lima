package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/hashicorp/nomad/api"
)

// Nomad is the part of the Nomad API the command uses.
//
// It is an interface so the unit tests run the real command code against a
// fake cluster. The real implementation is a thin wrapper over the Nomad API
// package, configured from NOMAD_ADDR, NOMAD_TOKEN, NOMAD_CACERT, NOMAD_NAMESPACE
// and the other variables the nomad CLI reads.
type Nomad interface {
	// NodeList lists the client nodes.
	NodeList(ctx context.Context) ([]*api.NodeListStub, error)
	// NodeInfo returns one node with its attributes, resources and drivers.
	NodeInfo(ctx context.Context, id string) (*api.Node, error)
	// NodeAllocations returns the allocations of a node.
	NodeAllocations(ctx context.Context, nodeID string) ([]*api.Allocation, error)

	// JobList lists the jobs.
	JobList(ctx context.Context) ([]*api.JobListStub, error)
	// Job returns one job.
	Job(ctx context.Context, id string) (*api.Job, error)
	// RegisterJob submits a job and returns the evaluation it created.
	RegisterJob(ctx context.Context, job *api.Job) (*api.JobRegisterResponse, error)
	// StopJob stops a job; purge also removes it and its history.
	StopJob(ctx context.Context, id string, purge bool) error

	// Allocations lists the allocations of the whole cluster.
	Allocations(ctx context.Context) ([]*api.AllocationListStub, error)
	// Allocation returns one allocation.
	Allocation(ctx context.Context, id string) (*api.Allocation, error)
	// Evaluation returns one evaluation, which explains why the scheduler
	// could not place a job.
	Evaluation(ctx context.Context, id string) (*api.Evaluation, error)

	// HostVolumes lists the dynamic host volumes.
	HostVolumes(ctx context.Context) ([]*api.HostVolumeStub, error)
	// HostVolume returns one dynamic host volume.
	HostVolume(ctx context.Context, id string) (*api.HostVolume, error)
	// CreateHostVolume asks a node's volume plugin to create a volume.
	CreateHostVolume(ctx context.Context, v *api.HostVolume) (*api.HostVolume, error)
	// RegisterHostVolume updates an existing dynamic host volume.
	RegisterHostVolume(ctx context.Context, v *api.HostVolume) (*api.HostVolume, error)
	// DeleteHostVolume deletes a dynamic host volume, and with it the disk.
	DeleteHostVolume(ctx context.Context, id string, force bool) error

	// VariableRead returns one Nomad Variable. A variable that does not exist
	// is (nil, nil): "not configured" is a state, not a failure.
	VariableRead(ctx context.Context, path string) (*api.Variable, error)
	// VariableWrite creates or replaces a Nomad Variable.
	VariableWrite(ctx context.Context, v *api.Variable) (*api.Variable, error)
	// VariableDelete removes a Nomad Variable.
	VariableDelete(ctx context.Context, path string) error

	// Exec runs a command inside a task and returns its exit code.
	Exec(ctx context.Context, alloc *api.Allocation, task string, cmd []string,
		stdin io.Reader, stdout, stderr io.Writer, tty bool, termSize <-chan api.TerminalSize) (int, error)
	// Logs streams one task log file ("stdout" or "stderr") of an allocation,
	// from the given byte offset of that file.
	Logs(ctx context.Context, alloc *api.Allocation, task, logType string, follow bool, offset int64) (<-chan *api.StreamFrame, <-chan error)
}

// NewNomadClient builds the Nomad API client the nomad CLI would build.
func NewNomadClient() (Nomad, error) {
	cfg := api.DefaultConfig()
	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, wrapf(err, "cannot build a Nomad client (check NOMAD_ADDR)")
	}
	return &nomadAPI{c: client}, nil
}

// nomadAPI implements Nomad over the Nomad API package.
type nomadAPI struct {
	c *api.Client
}

func (n *nomadAPI) NodeList(ctx context.Context) ([]*api.NodeListStub, error) {
	nodes, _, err := n.c.Nodes().List(nil)
	return nodes, err
}

func (n *nomadAPI) NodeInfo(ctx context.Context, id string) (*api.Node, error) {
	node, _, err := n.c.Nodes().Info(id, nil)
	return node, err
}

func (n *nomadAPI) NodeAllocations(ctx context.Context, nodeID string) ([]*api.Allocation, error) {
	allocs, _, err := n.c.Nodes().Allocations(nodeID, nil)
	return allocs, err
}

func (n *nomadAPI) JobList(ctx context.Context) ([]*api.JobListStub, error) {
	jobs, _, err := n.c.Jobs().List(nil)
	return jobs, err
}

func (n *nomadAPI) Job(ctx context.Context, id string) (*api.Job, error) {
	job, _, err := n.c.Jobs().Info(id, nil)
	return job, err
}

func (n *nomadAPI) RegisterJob(ctx context.Context, job *api.Job) (*api.JobRegisterResponse, error) {
	resp, _, err := n.c.Jobs().Register(job, nil)
	return resp, err
}

func (n *nomadAPI) StopJob(ctx context.Context, id string, purge bool) error {
	_, _, err := n.c.Jobs().Deregister(id, purge, nil)
	return err
}

func (n *nomadAPI) Allocations(ctx context.Context) ([]*api.AllocationListStub, error) {
	allocs, _, err := n.c.Allocations().List(nil)
	return allocs, err
}

func (n *nomadAPI) Allocation(ctx context.Context, id string) (*api.Allocation, error) {
	alloc, _, err := n.c.Allocations().Info(id, nil)
	return alloc, err
}

func (n *nomadAPI) Evaluation(ctx context.Context, id string) (*api.Evaluation, error) {
	eval, _, err := n.c.Evaluations().Info(id, nil)
	return eval, err
}

func (n *nomadAPI) HostVolumes(ctx context.Context) ([]*api.HostVolumeStub, error) {
	vols, _, err := n.c.HostVolumes().List(&api.HostVolumeListRequest{}, nil)
	return vols, err
}

func (n *nomadAPI) HostVolume(ctx context.Context, id string) (*api.HostVolume, error) {
	vol, _, err := n.c.HostVolumes().Get(id, nil)
	return vol, err
}

func (n *nomadAPI) CreateHostVolume(ctx context.Context, v *api.HostVolume) (*api.HostVolume, error) {
	resp, _, err := n.c.HostVolumes().Create(&api.HostVolumeCreateRequest{Volume: v}, nil)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Volume == nil {
		return nil, fmt.Errorf("the server accepted the volume but returned no record")
	}
	return resp.Volume, nil
}

func (n *nomadAPI) RegisterHostVolume(ctx context.Context, v *api.HostVolume) (*api.HostVolume, error) {
	resp, _, err := n.c.HostVolumes().Register(&api.HostVolumeRegisterRequest{Volume: v}, nil)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Volume == nil {
		return nil, fmt.Errorf("the server accepted the volume but returned no record")
	}
	return resp.Volume, nil
}

func (n *nomadAPI) DeleteHostVolume(ctx context.Context, id string, force bool) error {
	_, _, err := n.c.HostVolumes().Delete(&api.HostVolumeDeleteRequest{ID: id, Force: force}, nil)
	return err
}

func (n *nomadAPI) VariableRead(ctx context.Context, path string) (*api.Variable, error) {
	v, _, err := n.c.Variables().Read(path, nil)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (n *nomadAPI) VariableWrite(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	written, _, err := n.c.Variables().Create(v, nil)
	if err != nil {
		return nil, err
	}
	return written, nil
}

func (n *nomadAPI) VariableDelete(ctx context.Context, path string) error {
	_, err := n.c.Variables().Delete(path, nil)
	return err
}

func (n *nomadAPI) Exec(ctx context.Context, alloc *api.Allocation, task string, cmd []string,
	stdin io.Reader, stdout, stderr io.Writer, tty bool, termSize <-chan api.TerminalSize) (int, error) {
	return n.c.Allocations().Exec(ctx, alloc, task, tty, cmd, stdin, stdout, stderr, termSize, nil)
}

func (n *nomadAPI) Logs(ctx context.Context, alloc *api.Allocation, task, logType string, follow bool, offset int64) (<-chan *api.StreamFrame, <-chan error) {
	cancel := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(cancel)
	}()
	// A follow starts at the end of the file, which is what "follow" means for a
	// log nobody has read yet; a resume starts at the byte it left off at, which
	// is what makes a restarted stream repeat nothing and skip nothing.
	origin := "start"
	if follow && offset == 0 {
		origin = "end"
	}
	return n.c.AllocFS().Logs(alloc, follow, task, logType, origin, offset, cancel, nil)
}
