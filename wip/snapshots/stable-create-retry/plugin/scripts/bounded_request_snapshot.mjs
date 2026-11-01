// Candidate only. Caller must run its existing private outer/config/record/prompt
// binding guard and pass b. This module never starts an agent or writes state.
const safeEnum = value => typeof value === 'string' && /^[a-zA-Z0-9_.-]{1,80}$/.test(value) ? value : 'unknown';
function snapshotTerminalSummary(value) {
  if (!value || !['completed', 'failed', 'cancelled'].includes(value.status)) return null;
  return {status: value.status, stop_reason: safeEnum(value.stopReason ?? value.stop_reason)};
}
function eventSummary(event) {
  // Do not emit raw ACP fields, text, thoughts, tool inputs, or error messages.
  const out = {type: safeEnum(event.type), sequence: event.sequence};
  if (event.type === 'message') {
    const update = event.message?.params?.update;
    out.update = safeEnum(update?.sessionUpdate);
    if (['tool_call', 'tool_call_update'].includes(update?.sessionUpdate)) {
      out.tool = safeEnum(update.kind);
      out.status = safeEnum(update.status);
    }
  }
  return out;
}
function heartbeatSummary(proof, b, now, staleMs) {
  // This is an injected attestation, NOT raw owner JSON. Caller must validate
  // private owner record, generation/process identity AND exact current request.
  if (!proof || proof.validated !== true || proof.record_id !== b.state.record_id ||
      proof.request_id !== b.turn.acpx_request_id || !Number.isSafeInteger(proof.owner_generation) || proof.owner_generation <= 0) {
    return {state: 'unknown', reason: 'exact_request_owner_proof_unavailable'};
  }
  const at = Date.parse(proof.heartbeat_at), age = now - at;
  if (!Number.isFinite(age) || age < 0) return {state: 'unknown', reason: 'heartbeat_clock_unverifiable'};
  return {state: age <= staleMs ? 'fresh' : 'stale', age_ms: age, owner_generation: proof.owner_generation};
}
export async function boundedRequestSnapshot(b, Reader, options = {}) {
  const maxBytes = options.maxBytes ?? 262144;
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 1024 || maxBytes > 262144) throw new Error('REQUEST_SNAPSHOT_BOUND_INVALID');
  const maxSegments = b.record?.event_log?.max_segments;
  if (!Number.isSafeInteger(maxSegments) || maxSegments < 1 || maxSegments > 64) throw new Error('REQUEST_SNAPSHOT_RETENTION_UNSUPPORTED');
  const reader = new Reader({acpxRecordId: b.state.record_id, eventLog: {max_segments: maxSegments}});
  for (const method of ['openSnapshot', 'closeSnapshot', 'consumeLine', 'discoverPaths', 'matchesSnapshot']) {
    if (typeof reader[method] !== 'function') throw new Error('REQUEST_SNAPSHOT_READER_UNSUPPORTED');
  }
  const now = options.nowMs ?? Date.now();
  const owner = heartbeatSummary(options.ownerProof, b, now, options.heartbeatStaleMs ?? 10000);
  const receiptTerminal = b.turn.phase === 'terminal' ? snapshotTerminalSummary(b.turn.terminal) : null;
  const out = {type: 'request_status_snapshot', state: receiptTerminal ? 'terminal' : 'unknown',
    terminal: receiptTerminal, terminal_source: receiptTerminal ? 'retained_turn_receipt' : null,
    last_observed_event: null, owner_heartbeat: owner, journal_request_open: null,
    journal_state: 'unknown', scan_bytes_read: 0, scan_byte_limit: maxBytes,
    observed_unix_ms: now, read_only: true, prompt_retry: false, snapshot_not_watch: true};
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), options.timeoutMs ?? 1500);
  let snapshot;
  try {
    snapshot = await reader.openSnapshot(controller.signal);
    let newest = true, newestState = null, journalTerminal = null, last = null, starts = 0, results = 0;
    for (const segment of [...snapshot].reverse()) {
      controller.signal.throwIfAborted();
      const size = Number(segment.opened.stat.size);
      if (!Number.isSafeInteger(size) || size < 0) throw new Error('REQUEST_SNAPSHOT_SIZE_INVALID');
      if (size === 0) continue;
      const length = Math.min(size, maxBytes - out.scan_bytes_read);
      if (length <= 0) break;
      const start = size - length, buffer = Buffer.alloc(length);
      const {bytesRead} = await segment.opened.handle.read(buffer, 0, length, start);
      out.scan_bytes_read += bytesRead;
      if (bytesRead !== length) throw new Error('REQUEST_SNAPSHOT_CHANGED');
      // Drop the first line whenever starting mid-file: it may be a suffix of
      // a huge credential-bearing or marker-looking string. Never fabricate an anchor.
      let offset = start === 0 ? 0 : buffer.indexOf(10) + 1;
      if (start > 0 && offset === 0) offset = buffer.length;
      const state = {offset: 0, pending: Buffer.alloc(0), sequence: 0, messageSequence: 0, requestId: null};
      const page = {events: [], after: -1, bytes: 0, maxBytes: Infinity};
      let partial = false;
      while (offset < buffer.length) {
        controller.signal.throwIfAborted();
        const end = buffer.indexOf(10, offset);
        if (end < 0) { partial = true; break; }
        // Unanchored lifecycle lines are ignored, rather than accepted by a
        // second, weaker parser. Only real segment anchors establish sequence.
        const line = buffer.subarray(offset, end).toString('utf8');
        if (state.firstSequence === undefined) {
          let value; try { value = JSON.parse(line); } catch { offset = end + 1; continue; }
          if (value?.type !== 'segment') { offset = end + 1; continue; }
        }
        reader.consumeLine(line, state, page, end - offset + 1);
        offset = end + 1;
      }
      if (newest) {
        newestState = state.firstSequence !== undefined && !partial ? state : null;
        out.journal_state = state.firstSequence === undefined ? 'tail_unanchored' : partial ? 'partial_line' : 'anchored';
        newest = false;
      }
      for (const event of page.events) {
        if (event.requestId !== b.turn.acpx_request_id) continue;
        if (last === null || event.sequence > last.sequence) last = eventSummary(event);
        if (event.type === 'turn_started') starts++;
        if (event.type === 'turn_result') { results++; journalTerminal = snapshotTerminalSummary(event.result); }
      }
      // If this whole segment was read, older segments may contain the latest
      // exact-request event. A partial suffix exhausts the budget.
      if (start !== 0) break;
    }
    const scan = await reader.discoverPaths();
    if (!await reader.matchesSnapshot(scan.paths, snapshot, scan.holes)) throw new Error('REQUEST_SNAPSHOT_ROTATION_CHANGED');
    // Open handles preserve identity, but in-place mutation/appends must also
    // invalidate the snapshot. No second data read or unbounded retry.
    for (const segment of snapshot) {
      const current = await segment.opened.handle.stat();
      if (current.size !== segment.opened.stat.size || current.mtimeMs !== segment.opened.stat.mtimeMs || current.ctimeMs !== segment.opened.stat.ctimeMs) throw new Error('REQUEST_SNAPSHOT_CHANGED');
    }
    if (starts > 1 || results > 1 || (journalTerminal && newestState?.requestId === b.turn.acpx_request_id)) throw new Error('REQUEST_SNAPSHOT_REQUEST_ID_REUSED');
    out.last_observed_event = last;
    out.journal_request_open = newestState ? newestState.requestId === b.turn.acpx_request_id : null;
    if (journalTerminal) {
      if (receiptTerminal && JSON.stringify(receiptTerminal) !== JSON.stringify(journalTerminal)) throw new Error('REQUEST_SNAPSHOT_TERMINAL_CONFLICT');
      out.state = 'terminal'; out.terminal = journalTerminal; out.terminal_source = 'acpx_journal';
    } else if (!receiptTerminal && out.journal_request_open === true && owner.state === 'fresh') out.state = 'active';
    out.reason = out.state === 'terminal' ? 'exact_terminal_observed' : out.state === 'active' ? 'anchored_request_and_fresh_validated_owner' : 'current_request_outcome_unproven';
  } catch (error) {
    // Keep an exact private terminal receipt even if the bounded journal fails.
    // Never expose raw error messages, paths, journal data, or ACP payloads.
    out.journal_state = 'unavailable';
    out.reason = /^[A-Z][A-Z0-9_]{0,100}$/.test(error?.message ?? '') ? error.message : 'REQUEST_SNAPSHOT_UNAVAILABLE';
    if (['REQUEST_SNAPSHOT_TERMINAL_CONFLICT', 'REQUEST_SNAPSHOT_REQUEST_ID_REUSED'].includes(out.reason)) { out.state = 'unknown'; out.terminal = null; out.terminal_source = null; }
  } finally {
    clearTimeout(timer);
    if (snapshot) await reader.closeSnapshot(snapshot);
  }
  return out;
}
