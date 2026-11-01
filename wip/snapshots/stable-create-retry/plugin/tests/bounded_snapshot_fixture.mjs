import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import {boundedRequestSnapshot} from '../scripts/bounded_request_snapshot.mjs';
const packageDir = process.argv[2];
if (!packageDir) throw new Error('Usage: node test_snapshot.mjs /path/to/acpx');
assert.equal(JSON.parse(await fs.readFile(path.join(packageDir, 'package.json'))).version, '0.21.0');
const ipc = (await fs.readdir(path.join(packageDir, 'dist'))).filter(x => /^ipc-.*\.js$/.test(x));
assert.equal(ipc.length, 1);
const runtime = await import(pathToFileURL(path.join(packageDir, 'dist', ipc[0])));
const ActualReader = Object.values(runtime).find(x => typeof x === 'function' && x.name === 'SessionJournalReader');
assert.ok(ActualReader);
const fixture = await fs.mkdtemp(path.join(os.tmpdir(), 'bounded-request-synthetic-'));
let files = [], actualBytes = 0;
// Only path discovery is redirected into synthetic files. Snapshot open/close,
// anchor validation, ACP decoding, event sequencing and result validation are
// the unmodified official cached acpx runtime.
class Reader extends ActualReader {
  async discoverPaths() { return {paths: files}; }
  async openSnapshot(signal) {
    const segments = await super.openSnapshot(signal);
    for (const segment of segments) {
      const read = segment.opened.handle.read.bind(segment.opened.handle);
      segment.opened.handle.read = async (...args) => { const result = await read(...args); actualBytes += result.bytesRead; return result; };
    }
    return segments;
  }
}
const schema = 'acpx.session.journal.v1';
const anchor = (request_id = null, sequence = 0) => ({schema, type: 'segment', record_id: 'record-1', sequence, message_sequence: 0, request_id});
const start = request_id => ({schema, type: 'turn_started', request_id});
const done = request_id => ({schema, type: 'turn_result', request_id, result: {status: 'completed', stopReason: 'end_turn'}});
const msg = text => ({jsonrpc: '2.0', method: 'session/update', params: {sessionId: 'native-1', update: {sessionUpdate: 'agent_message_chunk', content: {type: 'text', text}}}});
const b = {state: {record_id: 'record-1'}, turn: {acpx_request_id: 'target', phase: 'queued'}, record: {event_log: {max_segments: 4}}};
const now = Date.now(), proof = {validated: true, record_id: 'record-1', request_id: 'target', owner_generation: 7, heartbeat_at: new Date(now - 100).toISOString()};
let tests = 0;
async function run(lines, expected, options = {}, binding = b) {
  const file = path.join(fixture, 'active.ndjson');
  await fs.writeFile(file, typeof lines === 'string' ? lines : lines.map(x => JSON.stringify(x)).join('\n') + '\n', {mode: 0o600});
  files = [file]; actualBytes = 0;
  const result = await boundedRequestSnapshot(binding, Reader, {nowMs: now, ...options});
  assert.equal(result.state, expected, JSON.stringify(result));
  assert.ok(result.scan_bytes_read <= 262144);
  assert.equal(result.scan_bytes_read, actualBytes);
  assert.ok(!JSON.stringify(result).includes('secret-content'));
  tests++;
  return result;
}
try {
  let r = await run([anchor(), start('target'), msg('secret-content')], 'unknown');
  assert.equal(r.journal_request_open, true); assert.equal(r.last_observed_event.type, 'message');
  r = await run([anchor(), start('target'), msg('secret-content')], 'active', {ownerProof: proof});
  assert.equal(r.owner_heartbeat.state, 'fresh');
  await run([anchor(), start('target')], 'unknown', {ownerProof: {...proof, request_id: 'other'}});
  r = await run([anchor(), start('target')], 'unknown', {ownerProof: {...proof, heartbeat_at: new Date(now - 60000).toISOString()}});
  assert.equal(r.owner_heartbeat.state, 'stale');
  r = await run([anchor(), start('target'), done('target')], 'terminal'); assert.equal(r.terminal_source, 'acpx_journal');
  r = await run([anchor('other'), done('other')], 'unknown', {ownerProof: proof}); assert.equal(r.last_observed_event, null);
  r = await run([anchor('target'), msg('secret-content')], 'active', {ownerProof: proof}); assert.equal(r.journal_request_open, true);
  r = await run([anchor(), start('target'), done('target'), start('target')], 'unknown', {ownerProof: proof});
  assert.equal(r.reason, 'REQUEST_SNAPSHOT_REQUEST_ID_REUSED');
  r = await run([], 'unknown'); assert.equal(r.journal_request_open, null);
  r = await run([anchor('target')], 'unknown', {ownerProof: {...proof, heartbeat_at: new Date(now + 1000).toISOString()}});
  assert.equal(r.owner_heartbeat.reason, 'heartbeat_clock_unverifiable');
  const huge = [anchor(), start('target'), msg('secret-content'.repeat(90000)), done('target')].map(x => JSON.stringify(x)).join('\n') + '\n';
  r = await run(huge, 'unknown', {ownerProof: proof}); assert.equal(r.journal_state, 'tail_unanchored'); assert.equal(actualBytes, 262144);
  r = await run(huge, 'terminal', {}, {...b, turn: {...b.turn, phase: 'terminal', terminal: {status: 'completed', stopReason: 'end_turn'}}});
  assert.equal(r.terminal_source, 'retained_turn_receipt');
  r = await run(JSON.stringify(anchor('target')) + '\n' + JSON.stringify(msg('secret-content')), 'unknown', {ownerProof: proof}); assert.equal(r.journal_state, 'partial_line');
  r = await run([anchor('other-record'), done('target')], 'unknown'); assert.equal(r.journal_state, 'unavailable');
  r = await run([anchor(), start('target'), done('target')], 'unknown', {}, {...b, turn: {...b.turn, phase: 'terminal', terminal: {status: 'failed'}}});
  assert.equal(r.reason, 'REQUEST_SNAPSHOT_TERMINAL_CONFLICT');
  // A newest small unrelated segment leaves room to inspect an older exact terminal.
  const old = path.join(fixture, 'old.ndjson'), active = path.join(fixture, 'new.ndjson');
  await fs.writeFile(old, [anchor(), start('target'), done('target')].map(x => JSON.stringify(x)).join('\n') + '\n');
  await fs.writeFile(active, [anchor(null, 2), start('other')].map(x => JSON.stringify(x)).join('\n') + '\n');
  files = [old, active]; actualBytes = 0;
  r = await boundedRequestSnapshot(b, Reader, {nowMs: now, ownerProof: proof});
  assert.equal(r.state, 'terminal'); assert.equal(r.journal_request_open, false); tests++;
  console.log(JSON.stringify({passed: tests, runtime_version: '0.21.0', runtime: 'official cached SessionJournalReader', redirected: 'synthetic path discovery only', maximum_actual_read_bytes: 262144, live_access: false}));
} finally { await fs.rm(fixture, {recursive: true, force: true}); }
