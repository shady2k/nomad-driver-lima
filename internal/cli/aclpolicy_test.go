package cli

// The ACL policies the repository ships must grant what the command needs and
// nothing else. Nomad's own ACL engine decides that, so it is what these tests
// parse and query: a capability name that Nomad does not know, or a path that
// the policy does not cover, fails here instead of at the first command.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/acl"
	"github.com/stretchr/testify/require"
)

// aclPolicyDir is where deploy/ ships the policy files.
func aclPolicyDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "deploy", "acl"))
	require.NoError(t, err)
	return dir
}

// readACLPolicy parses one policy file the way Nomad does when it applies it.
func readACLPolicy(t *testing.T, name string) *acl.Policy {
	t.Helper()
	path := filepath.Join(aclPolicyDir(t), name)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "the policy file %s must exist", path)
	policy, err := acl.Parse(string(data), true)
	require.NoError(t, err, "%s must be a valid Nomad ACL policy", path)
	require.NotEmpty(t, policy.Raw)
	return policy
}

// aclOf builds the decision object Nomad builds from a token's policies.
func aclOf(t *testing.T, policies ...*acl.Policy) *acl.ACL {
	t.Helper()
	obj, err := acl.NewACL(false, policies)
	require.NoError(t, err)
	return obj
}

func TestBoxOperatorPolicyGrantsWhatTheCommandNeeds(t *testing.T) {
	operator := aclOf(t, readACLPolicy(t, "box-operator.policy.hcl"))

	for _, capability := range []string{
		acl.NamespaceCapabilityListJobs,           // box ls
		acl.NamespaceCapabilityReadJob,            // box status, waits, allocations
		acl.NamespaceCapabilitySubmitJob,          // create/start/stop/rm/resize
		acl.NamespaceCapabilityAllocExec,          // run/ssh/cp/boxctl
		acl.NamespaceCapabilityReadLogs,           // box logs
		acl.NamespaceCapabilityHostVolumeCreate,   // box create
		acl.NamespaceCapabilityHostVolumeDelete,   // box rm
		acl.NamespaceCapabilityHostVolumeRegister, // box resize
		acl.NamespaceCapabilityHostVolumeRead,     // box ls, status
	} {
		require.True(t, operator.AllowNsOp("default", capability), "the operator must be able to %s", capability)
		require.False(t, operator.AllowNsOp("agent-workers", capability),
			"the operator policy is for the box namespace only, not agent-workers")
	}

	// The executors variable: read, write and delete on one path.
	for _, op := range []string{acl.VariablesCapabilityRead, acl.VariablesCapabilityWrite, acl.VariablesCapabilityDestroy} {
		require.True(t, operator.AllowVariableOperation("default", boxExecutorsVariable, op, nil),
			"the operator must be able to %s %s", op, boxExecutorsVariable)
	}
	// The image store: the operator writes and reads it, and a publish job reads it
	// with a policy of its own.
	for _, op := range []string{acl.VariablesCapabilityRead, acl.VariablesCapabilityWrite, acl.VariablesCapabilityDestroy} {
		require.True(t, operator.AllowVariableOperation("default", boxStoreVariable, op, nil),
			"the operator must be able to %s %s", op, boxStoreVariable)
	}
	// The description of a box, where its job cannot be: create/start/resize/
	// upgrade write it, ls/status/start read it, rm deletes it.
	for _, op := range []string{acl.VariablesCapabilityRead, acl.VariablesCapabilityWrite, acl.VariablesCapabilityDestroy} {
		require.True(t, operator.AllowVariableOperation("default", BoxSpecVariable("dev1"), op, nil),
			"the operator must be able to %s %s", op, BoxSpecVariable("dev1"))
	}
	// And nothing else: no other variable, no listing, no other namespace.
	require.False(t, operator.AllowVariableOperation("default", "nomad/jobs/other-dev1", acl.VariablesCapabilityWrite, nil),
		"the description path is the box's own, not every job's")
	require.False(t, operator.AllowVariableOperation("default", "other/secret", acl.VariablesCapabilityRead, nil))
	require.False(t, operator.AllowVariableOperation("agent-workers", boxExecutorsVariable, acl.VariablesCapabilityRead, nil))

	// Registering the box job mounts its disk: Nomad checks the volume by name,
	// apart from submit-job, and refuses the job without it.
	require.True(t, operator.AllowHostVolumeOperation(BoxVolumeName("dev1"), acl.HostVolumeCapabilityMountReadWrite),
		"box create registers a job that mounts %s", BoxVolumeName("dev1"))
	require.False(t, operator.AllowHostVolumeOperation("other-disk", acl.HostVolumeCapabilityMountReadOnly),
		"only the boxes' own disks")

	// Nodes: read for box capacity, never write.
	require.True(t, operator.AllowNodeRead(), "box capacity reads the nodes")
	require.False(t, operator.AllowNodeWrite())

	// The command needs no agent, operator, ACL or plugin access.
	require.False(t, operator.AllowAgentRead())
	require.False(t, operator.AllowOperatorRead())
	require.False(t, operator.AllowNamespaceOperation("default", acl.NamespaceCapabilityCSIMountVolume))
}

// TestBoxStorePolicyIsForAPublishOnly: the store's url and token reach a publish
// job through its own policy, and a box never reads them.
// TestBoxReapPolicyGrantsWhatThePassNeeds: the periodic reap task authenticates
// with its own workload identity, so this policy is what lets it look at every
// box on the cluster and delete the disk of one that served its term -- and it
// grants nothing of the operator's.
func TestBoxReapPolicyGrantsWhatThePassNeeds(t *testing.T) {
	reap := aclOf(t, readACLPolicy(t, "box-reap.policy.hcl"))

	for _, capability := range []string{
		acl.NamespaceCapabilityListJobs,       // the boxes, and the bakes and publishes
		acl.NamespaceCapabilityReadJob,        // one box's job, its allocations, the alloc list
		acl.NamespaceCapabilitySubmitJob,      // purge the job of a box whose disk goes
		acl.NamespaceCapabilityHostVolumeRead, // list the box disks
		acl.NamespaceCapabilityHostVolumeDelete,
	} {
		require.True(t, reap.AllowNsOp("default", capability), "the reap pass must be able to %s", capability)
	}
	for _, capability := range []string{
		acl.NamespaceCapabilityAllocExec,
		acl.NamespaceCapabilityReadLogs,
		acl.NamespaceCapabilityHostVolumeCreate,
		acl.NamespaceCapabilityHostVolumeRegister,
		acl.NamespaceCapabilityHostVolumeWrite,
		acl.NamespaceCapabilityCSIWriteVolume,
	} {
		require.False(t, reap.AllowNsOp("default", capability),
			"the reap pass has no business with %s: it deletes a disk that is already over", capability)
	}
	require.True(t, reap.AllowNodeRead(), "a node that is down is never a reason to delete a disk")
	require.False(t, reap.AllowNodeWrite())
	require.False(t, reap.AllowAgentRead())
	require.False(t, reap.AllowOperatorRead())

	// The clock and the description of a box. The two paths are named one by one
	// so that the pass cannot write the variable that holds the executor
	// credentials, which a nomad/jobs/box-* glob would also match.
	for _, path := range []string{BoxStoppedVariable("dev1")} {
		for _, op := range []string{acl.VariablesCapabilityRead, acl.VariablesCapabilityWrite, acl.VariablesCapabilityDestroy} {
			require.True(t, reap.AllowVariableOperation("default", path, op, nil), "the reap pass must %s %s", op, path)
		}
	}
	require.True(t, reap.AllowVariableOperation("default", BoxSpecVariable("dev1"), acl.VariablesCapabilityDestroy, nil),
		"the description goes with the disk")
	require.False(t, reap.AllowVariableOperation("default", boxExecutorsVariable, acl.VariablesCapabilityWrite, nil),
		"the executor credentials are not the pass's business")
	require.False(t, reap.AllowVariableOperation("default", boxStoreVariable, acl.VariablesCapabilityRead, nil))
	require.False(t, reap.AllowVariableOperation("default", "somestore/secret", acl.VariablesCapabilityRead, nil))
}

func TestBoxStorePolicyIsForAPublishOnly(t *testing.T) {
	store := aclOf(t, readACLPolicy(t, "box-store.policy.hcl"))
	job := BoxPublishJobName("project-1")
	require.True(t, store.AllowVariableOperation("default", boxStoreVariable, acl.VariablesCapabilityRead, jobContext(job)),
		"a publish job reads %s", boxStoreVariable)
	require.False(t, store.AllowVariableOperation("default", boxStoreVariable, acl.VariablesCapabilityWrite, jobContext(job)),
		"a publish job reads the store, it never writes its own credential")
	require.False(t, store.AllowVariableOperation("default", boxExecutorsVariable, acl.VariablesCapabilityRead, jobContext(job)),
		"a publish job has no business with the executor credentials")

	// The default scoping of a publish job's own identity does not cover the path:
	// that is why the policy exists at all.
	require.False(t, aclOf(t, readACLPolicy(t, "box-store.policy.hcl")).AllowVariableOperation("default",
		"nomad/jobs/other-job", acl.VariablesCapabilityRead, jobContext(job)))

	// The workload policy of the boxes grants the executor credentials and nothing
	// of the store.
	workloads := aclOf(t, readACLPolicy(t, "box-workload.policy.hcl"))
	require.False(t, workloads.AllowVariableOperation("default", boxStoreVariable, acl.VariablesCapabilityRead, jobContext("box-dev1")),
		"a box must not read the store's write token")
}

// jobContext is the claim context Nomad passes for a job's workload identity.
func jobContext(job string) *acl.ACLClaim {
	return &acl.ACLClaim{Namespace: "default", Job: job}
}

func TestBoxWorkloadPolicyCoversTheSharedVariable(t *testing.T) {
	workload := aclOf(t, readACLPolicy(t, "box-workload.policy.hcl"))
	claim := &acl.ACLClaim{Namespace: "default", Job: "box-dev1", Group: boxGroupName, Task: boxTaskName}

	require.True(t, workload.AllowVariableOperation("default", boxExecutorsVariable, acl.VariablesCapabilityRead, claim),
		"a box template reads %s with the allocation's workload identity", boxExecutorsVariable)
	require.False(t, workload.AllowVariableOperation("default", boxExecutorsVariable, acl.VariablesCapabilityWrite, claim),
		"a box must never write the variable that holds its own credentials")
	require.False(t, workload.AllowVariableOperation("default", "nomad/jobs/box-other", acl.VariablesCapabilityRead, claim))

	// The policy grants nothing else at all: it is a variable rule, not a
	// namespace or a node rule.
	require.False(t, workload.AllowNsOp("default", acl.NamespaceCapabilitySubmitJob))
	require.False(t, workload.AllowNodeRead())
}

func TestDefaultWorkloadIdentityScopingDoesNotCoverTheSharedPath(t *testing.T) {
	// This is the reason the workload policy exists: with no policy at all, a
	// workload identity reads its own job's variables and nothing beside them.
	defaults := aclOf(t)
	claim := &acl.ACLClaim{Namespace: "default", Job: "box-dev1", Group: boxGroupName, Task: boxTaskName}

	require.True(t, defaults.AllowVariableOperation("default", "nomad/jobs", acl.VariablesCapabilityRead, claim))
	require.True(t, defaults.AllowVariableOperation("default", "nomad/jobs/box-dev1", acl.VariablesCapabilityRead, claim))
	require.True(t, defaults.AllowVariableOperation("default", "nomad/jobs/box-dev1/box", acl.VariablesCapabilityList, claim))
	require.True(t, defaults.AllowVariableOperation("default", "nomad/jobs/box-dev1/box/box", acl.VariablesCapabilityRead, claim))

	require.False(t, defaults.AllowVariableOperation("default", boxExecutorsVariable, acl.VariablesCapabilityRead, claim),
		"nomad/jobs/box-executors is a sibling of nomad/jobs/box-dev1, not below it")
	require.False(t, defaults.AllowVariableOperation("default", "nomad/jobs/box-other", acl.VariablesCapabilityRead, claim))

	// The description of a box is a sibling of the paths a workload identity may
	// read, so no box can read or write its own: the description is the CLI's, and
	// a box does not need it -- it gets a rendered job, not a description.
	require.False(t, defaults.AllowVariableOperation("default", BoxSpecVariable("dev1"), acl.VariablesCapabilityRead, claim))
	require.False(t, defaults.AllowVariableOperation("default", BoxSpecVariable("dev1"), acl.VariablesCapabilityWrite, claim),
		"a box must never be able to change what a start registers")
}

func TestACLPoliciesUseTheNamesTheCommandUses(t *testing.T) {
	// The policies name the variable path and the namespace the command uses;
	// a rename in the command must fail here, not on a live cluster.
	require.Equal(t, "nomad/jobs/box-executors", boxExecutorsVariable)
	for _, name := range []string{"box-operator.policy.hcl", "box-workload.policy.hcl"} {
		data, err := os.ReadFile(filepath.Join(aclPolicyDir(t), name))
		require.NoError(t, err)
		require.Contains(t, string(data), `"`+boxExecutorsVariable+`"`,
			"%s must name the variable the command reads and writes", name)
		require.Contains(t, string(data), `namespace "default"`,
			"%s must be for the namespace a box is created in", name)
	}
}
