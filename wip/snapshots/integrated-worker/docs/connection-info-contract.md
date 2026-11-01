# Human connection information (proposed read-only contract)

Not implemented or activated by the progress release. No SSH key, port, ACL,
listener, user account, wrapper, alias or firewall rule is created by this command.

Proposed CLI: `nomad-workers --config TARGET connection-info --session UUID
--format text|json`. The initiating agent invokes it when a human asks how to
connect. It checks existing owner/session/attempt and current allocation/node
identity. A response is information, not an access grant. It never fetches Lima's
private SSH key/config or puts credentials in an argv, URL, response or log.

Infrastructure lives only in private installation configuration. Select an access
profile by the allocation's actual node identity, not a hardcoded host name,
architecture or global default. The profile must specify the existing human SSH
alias and a verified native runtime-shell argv prefix/environment. The selected
instance comes from authoritative driver metadata for this exact allocation,
not a guessed name. If it cannot be read through existing permitted metadata,
return missing instance information instead of adding a host-exec permission.

A usable profile can return:

```json
{
  "status": "available",
  "session_id": "<session-uuid>",
  "allocation_id": "<allocation-uuid>",
  "node_id": "<node-uuid>",
  "transport": "existing-ssh-profile",
  "profile_id": "<configured-profile-id>",
  "ssh_argv": ["ssh", "-t", "<existing-node-alias>", "limactl shell <instance>"],
  "command": "ssh -t <existing-node-alias> 'limactl shell <instance>'",
  "access_verified": false,
  "verification": "profile configured; this read-only command did not log in"
}
```

That short form is valid ONLY if the configured account already selects the right
runtime environment and has `limactl` in PATH. Do not pretend the service-specific
LIMA_HOME is the human's default. If a convenient SSH alias/native shell entrypoint
is absent, report it; configuring one is a separately reviewed operator task,
not an invitation to concatenate private key paths/sudo/environment into a huge
agent-facing command. The public code uses no installation-specific user, key,
address, domain, host path, LIMA_HOME or node alias. Use shlex quoting over the
trusted profile argv and validated instance, not shell interpolation of metadata.

Unknown/unsupported profile response:

```json
{
  "status": "unavailable",
  "reason": "access_profile_missing_or_incomplete",
  "missing_fields": ["node_access_profile", "human_ssh_alias", "verified_runtime_shell_environment"],
  "command": null,
  "access_grants_changed": false
}
```

Do not advertise remote access or another runtime implementation merely because
readiness metadata names one. Existing Prime implementation and model catalog
semantics are unchanged.
