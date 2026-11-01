# Native ACP conversation forwarding

New guest assets are required; this source change does not hotpatch existing VMs or published disks. Adapter watch projects official acpx session/update messages, tool lifecycle and terminal/error events into lifecycle.jsonl. Task main projects those rows into native Nomad stdout/stderr. Session/request and stable sequence/part identify delivery; reconnect replay and append-before-state crash duplicates are suppressed by the consumer.

Default allowance is 1 MiB per request, configurable as native_acp_log_bytes (4096..8388608). One explicit truncation notice points to watch/collect. Logical text segments are buffered through tool/end boundaries before redaction; newlines are not security boundaries. Segments over 65536 characters are omitted explicitly. This delays partial text visibility until a semantic boundary. Thoughts, raw tool inputs/outputs and environment are excluded. Quoted JSON/header/key values and known credential-shaped text are redacted across text chunks; this is not a guarantee for arbitrary unlabeled secrets printed by a model/tool. Existing full journal access controls and artifact handling still apply.

Example native records: acp_message (assistant/user), acp_tool_start, acp_tool_update, acp_tool_end, acp_error, acp_turn_end. The operation log budget is independent. No prompt replay or new client/daemon is introduced.
