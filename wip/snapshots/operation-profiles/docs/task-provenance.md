# Source and task metadata

New requests require `--task-title` describing the actual work. Supply
`--source-agent-name` and `--source-agent-type` unless genuine native hook fields
or private `source_agent: {name, type}` defaults provide them. A native agent type
is never reused as its name. Missing descriptive fields fail before job submission.
No model is used to infer the caller's name/type.

Native optional `agent_name`, `agent_type`, `agent_id` values are recorded only
when actually present on SessionStart. Not every caller supplies them; absence is
normal. PreToolUse cannot replace identity/generation or add invented names.
The record never reads transcripts or tool payloads. These fields and private
operator defaults are descriptive observations, not cryptographic identity/liveness.

Nomad Job.Meta keeps authoritative creator_agent_id, creator_session_id,
creator_generation and worker_session_id. It adds source_agent_name/type, each
field's evidence, source_native_agent_id, source_native_session_id (empty if not genuinely observed), source_native_session_status,
source_start_source and source_task_title. Optional task_link must be a credential-
free HTTP(S) URL without a query. Missing links are empty with
source_task_link_status=not_provided; no fake issue/task link is generated.
Runtime coordinator model is separately named runtime_coordinator_model; the old
coordinator_model field remains a compatibility alias. Neither identifies the
initiating coordinator.

New request idempotency includes descriptions, so changed task/source under the
same request ID is rejected. Historical intents without these fields retain their
original lookup contract; they can be inspected without supplying fabricated
metadata. Existing owner records, allocations and jobs are never rewritten.
