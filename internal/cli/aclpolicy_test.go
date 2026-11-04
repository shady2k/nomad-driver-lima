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
	// And nothing else: no other variable, no listing, no other namespace.
	require.False(t, operator.AllowVariableOperation("default", "nomad/jobs/box-dev1", acl.VariablesCapabilityWrite, nil))
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
