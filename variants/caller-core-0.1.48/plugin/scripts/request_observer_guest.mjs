// Ephemeral read-only journal reader / finite watch client. No ACP agent spawn.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import {pathToFileURL} from 'node:url';
import {spawn,spawnSync} from 'node:child_process';
import readline from 'node:readline';
const hash=x=>crypto.createHash('sha256').update(x).digest('hex');
const uuid=x=>typeof x==='string'&&/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(x);
const fail=code=>{throw Object.assign(new Error(code),{code})};
const emit=x=>process.stdout.write(JSON.stringify(x)+'\n');
const enumValue=x=>typeof x==='string'&&/^[a-zA-Z0-9_.-]{1,80}$/.test(x)?x:'unknown';
export function redact(text){
 // Complete logical text is joined BEFORE filtering or pagination.
 const key='(?:authorization|proxy-authorization|x-vault-token|x-nomad-token|api[-_]?key|[A-Z_][A-Z0-9_-]*(?:TOKEN|KEY|PASSWORD|PASSWD|SECRET|SEED)|TOKEN|KEY|PASSWORD|PASSWD|SECRET|SEED)';
 const unescaped=text.replace(/\\+(["'])/g,'$1');
 if(unescaped!==text&&new RegExp('["\']?\\b'+key+'["\']?\\s*[:=]','i').test(unescaped))return '[REDACTED_ESCAPED_CREDENTIAL_TEXT]';
 // Same unterminated-quoted-value rule as reviewed ACP logging (faf2079).
 const quoted=new RegExp('(["\']?\\b'+key+'["\']?\\s*[:=]\\s*)'+String.raw`("(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$))`,'gi');
 text=text.replace(quoted,(_,prefix)=>prefix+'"[REDACTED]"');
 text=text.replace(new RegExp('(\\b'+key+'\\s*[:=]\\s*)(?:Bearer\\s+|Basic\\s+)?[^\\s,;"\\\'}]+','gi'),'$1[REDACTED]');
 text=text.replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b/g,'[REDACTED_JWT]');
 text=text.replace(/\b(?:hvs|hvb|s)\.[A-Za-z0-9_-]{16,}\b/g,'[REDACTED_TOKEN]');
 text=text.replace(/https?:\/\/[^\s"'<>]+/g,value=>{try{const u=new URL(value);if(u.username||u.password){u.username='REDACTED';u.password='';}if(u.search)u.search='REDACTED';return u.toString()}catch{return '[REDACTED_URL]'}});
 return text;
}
export function terminalSummary(result){
 if(!result||typeof result!=='object')return null;
 const out={status:enumValue(result.status??result.kind),stop_reason:enumValue(result.stopReason??result.stop_reason)};
 if(Number.isSafeInteger(result.exitCode))out.exit_code=result.exitCode;
 return out;
}
function privateJSON(file,max=2097152){
 const fd=fs.openSync(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);
 try{const st=fs.fstatSync(fd);if(!st.isFile()||st.uid!==process.getuid()||(st.mode&0o777)!==0o600)fail('TRANSCRIPT_PRIVATE_RECORD_REQUIRED');if(st.size>max)fail('TRANSCRIPT_RECORD_BOUND_EXCEEDED');const bytes=Buffer.alloc(st.size+1),n=fs.readSync(fd,bytes,0,bytes.length,0);if(n>st.size||fs.fstatSync(fd).size>max)fail('TRANSCRIPT_RECORD_CHANGED');return JSON.parse(bytes.subarray(0,n).toString('utf8'))}finally{fs.closeSync(fd)}
}
function binding(p){
 const c=p.config;
 if(!uuid(p.session_id)||!uuid(p.request_id)||!c||c.protocol_version!==1||c.acpx_version!=='0.21.0')fail('TRANSCRIPT_CONFIG_UNSUPPORTED');
 for(const key of ['state_dir','workspace','node','runtime_module','acpx_bin','launcher'])if(!path.isAbsolute(c[key]??''))fail('TRANSCRIPT_CONFIG_UNSUPPORTED');
 const dir=path.join(c.state_dir,p.session_id),st=fs.lstatSync(dir);
 if(!st.isDirectory()||st.uid!==process.getuid()||(st.mode&0o077)!==0)fail('TRANSCRIPT_PRIVATE_SESSION_REQUIRED');
 const state=privateJSON(path.join(dir,'session.json')),turn=privateJSON(path.join(dir,'turn-'+p.request_id+'.json'));
 const boundRecord=state.record_bindings?.[p.request_id]??state.record_id;
 if(state.config_hash!==hash(JSON.stringify(c))||!uuid(boundRecord)||boundRecord!==p.record_id)fail('TRANSCRIPT_SESSION_BINDING_CHANGED');
 if(turn.request_id!==p.request_id||typeof turn.acpx_request_id!=='string'||!turn.acpx_request_id||turn.acpx_request_id.length>256)fail('TRANSCRIPT_REQUEST_BINDING_CHANGED');
 if(typeof p.prompt_sha256!=='string'||!/^[a-f0-9]{64}$/.test(p.prompt_sha256)||turn.prompt_sha256!==p.prompt_sha256)fail('TRANSCRIPT_PROMPT_BINDING_CHANGED');
 if(Object.hasOwn(turn,'session_id')){
  if(turn.session_id!==p.session_id)fail('TRANSCRIPT_REQUEST_BINDING_CHANGED');
 }else{
  // Deployed adapter v1 writes no session_id. Recognize only that exact schema;
  // directory + private session/config + acpx record + outer/prompt bindings
  // supply the missing field. This is not an unbound or guessed-ID fallback.
  const keys=new Set(['request_id','phase','prompt_sha256','acpx_request_id','cursor','terminal']);
  if(Object.keys(turn).some(k=>!keys.has(k))||!['queued','terminal'].includes(turn.phase))fail('TRANSCRIPT_LEGACY_TURN_SCHEMA_UNSUPPORTED');
 }
 // Control uses the already bound private create/turn receipts and upstream
 // ownerGeneration + targetRequestId. Never make cancellation depend on a
 // potentially large or unavailable session-history registry/journal read.
 const control=p.action==='cancel'||p.action==='cancel-status';
 const record=control?null:privateJSON(path.join(os.homedir(),'.acpx','sessions',boundRecord+'.json'),8388608);
 if(record&&(record.schema!=='acpx.session.v1'||record.acpx_record_id!==boundRecord||record.name!==p.session_id))fail('TRANSCRIPT_ACPX_RECORD_CHANGED');
 if(p.acpx_request_id!=null&&p.acpx_request_id!==turn.acpx_request_id)fail('TRANSCRIPT_REQUEST_BINDING_CHANGED');
 return {c,dir,state:{...state,record_id:boundRecord},turn,record};
}
function textParts(text){
 const parts=[];let part='',bytes=0;
 for(const char of text){const n=Buffer.byteLength(char);if(bytes+n>1024){parts.push(part);part='';bytes=0}part+=char;bytes+=n;}
 if(part)parts.push(part);return parts;
}
export function project(events,requestId){
 const entries=[];let open=null,started=0,terminal=null,tools=0,found=false;
 const flush=()=>{if(open){const clean=redact(open.text);textParts(clean).forEach((text,part)=>entries.push({kind:'text',role:open.role,sequence_start:open.first,sequence_end:open.last,part,text}));open=null;}};
 for(const e of events){
  if(e.requestId!==requestId)continue;found=true;
  if(e.type==='turn_started'){flush();if(++started>1)fail('TRANSCRIPT_REQUEST_ID_REUSED');continue}
  if(e.type==='turn_result'){flush();if(terminal)fail('TRANSCRIPT_REQUEST_ID_REUSED');terminal=terminalSummary(e.result);continue}
  const m=e.message,u=m?.params?.update,type=u?.sessionUpdate;
  if(e.type==='message'&&m?.method==='session/update'&&['agent_message_chunk','user_message_chunk'].includes(type)&&u.content?.type==='text'&&typeof u.content.text==='string'){
   const role=type==='agent_message_chunk'?'assistant':'user';
   if(open&&open.role!==role)flush();
   if(!open)open={role,first:e.sequence,last:e.sequence,text:''};
   open.text+=u.content.text;open.last=e.sequence;continue;
  }
  // Thoughts and raw tool input/output/config/environment are not transcript fields.
  if(type==='agent_thought_chunk')continue;
  flush();
  if(type==='tool_call'||type==='tool_call_update'){
   if(type==='tool_call')tools++;
   entries.push({kind:type==='tool_call'?'tool_start':(['completed','failed','cancelled'].includes(u.status)?'tool_end':'tool_update'),sequence:e.sequence,call_id:enumValue(u.toolCallId),tool:enumValue(u.kind??'tool'),status:enumValue(u.status??'pending')});
  }
 }
 // An in-flight logical text block is held until a real boundary or turn result:
 // streaming a prefix could expose a credential split over adjacent ACP chunks.
 return {entries,terminal,retained_prefix_complete:started===1,history_complete:started===1&&terminal!==null,tail_buffered:open!==null,journal_request_found:found,observed_tool_calls:tools};
}
export function paginate(projection,bound,cursor,limit,maxBytes){
 let index=0;
 if(cursor!=null){
  if(typeof cursor!=='string'||cursor.length>4096)fail('TRANSCRIPT_CURSOR_INVALID');
  let parsed;try{const raw=Buffer.from(cursor,'base64url');if(raw.toString('base64url')!==cursor)fail('TRANSCRIPT_CURSOR_INVALID');parsed=JSON.parse(raw.toString('utf8'))}catch{fail('TRANSCRIPT_CURSOR_INVALID')}
  if(parsed.version!==1||Object.entries(bound).some(([k,v])=>parsed[k]!==v)||!Number.isSafeInteger(parsed.index)||parsed.index<0)fail('TRANSCRIPT_CURSOR_BINDING_CHANGED');
  index=parsed.index;if(index>projection.entries.length)fail('TRANSCRIPT_CURSOR_HISTORY_CHANGED');if(parsed.prefix_sha256!==hash(JSON.stringify(projection.entries.slice(0,index))))fail('TRANSCRIPT_CURSOR_HISTORY_CHANGED');
 }
 const start=index,page=[];let bytes=0;
 while(index<projection.entries.length&&page.length<limit){const entry=projection.entries[index],size=Buffer.byteLength(JSON.stringify(entry));if(bytes+size>maxBytes)break;page.push(entry);bytes+=size;index++;}
 const next=Buffer.from(JSON.stringify({version:1,...bound,index,prefix_sha256:hash(JSON.stringify(projection.entries.slice(0,index)))})).toString('base64url');
 return {...projection,entries:page,cursor:next,has_more:index<projection.entries.length,page_start:start,page_end:index,content_bytes:bytes};
}
// Observational checkpoints are separate from turn/watch state. The client cursor
// contains only a content hash, never raw parser buffers or unredacted text.
function transcriptCache(b){
 const dir=path.join(b.dir,'transcript-pages');
 try{fs.mkdirSync(dir,{mode:0o700})}catch(e){if(e.code!=='EEXIST')throw e}
 const st=fs.lstatSync(dir);if(!st.isDirectory()||st.isSymbolicLink()||st.uid!==process.getuid()||(st.mode&0o077)!==0)fail('TRANSCRIPT_CACHE_UNSAFE');
 return dir;
}
function scanBinding(p,b){return {session_id:p.session_id,request_id:p.request_id,record_id:b.state.record_id,acpx_request_id:b.turn.acpx_request_id,prompt_sha256:b.turn.prompt_sha256,config_hash:b.state.config_hash}}
function loadScan(p,b,dir){
 const bound=scanBinding(p,b);
 if(p.cursor==null)return {version:2,bound,segments:{},global_sequence:null,queue:[],open:null,discard:false,started:0,terminal:null,tools:0,found:false,index:0};
 let cursor;try{cursor=JSON.parse(Buffer.from(p.cursor,'base64url').toString('utf8'))}catch{fail('TRANSCRIPT_CURSOR_INVALID')}
 if(cursor.version!==2||typeof cursor.checkpoint!=='string'||!/^[a-f0-9]{64}$/.test(cursor.checkpoint)||JSON.stringify(cursor.bound)!==JSON.stringify(bound))fail('TRANSCRIPT_CURSOR_BINDING_CHANGED');
 let saved;try{saved=privateJSON(path.join(dir,cursor.checkpoint+'.json'),2097152)}catch(e){if(e.code==='ENOENT')fail('TRANSCRIPT_CURSOR_EXPIRED_RESTART_READ');throw e}
 if(hash(JSON.stringify(saved))!==cursor.checkpoint||JSON.stringify(saved.bound)!==JSON.stringify(bound))fail('TRANSCRIPT_CURSOR_CHECKPOINT_CHANGED');
 return saved;
}
function saveScan(state,dir){
 const raw=JSON.stringify(state);if(Buffer.byteLength(raw)>2097152)fail('TRANSCRIPT_CHECKPOINT_BOUND_EXCEEDED');
 const digest=hash(raw),file=path.join(dir,digest+'.json');
 try{const fd=fs.openSync(file,'wx',0o600);try{fs.writeSync(fd,raw);fs.fsyncSync(fd)}finally{fs.closeSync(fd)}}catch(e){if(e.code!=='EEXIST')throw e;const old=privateJSON(file,2097152);if(hash(JSON.stringify(old))!==digest)fail('TRANSCRIPT_CACHE_CHANGED')}
 // Bounded disposable metadata only; old cursors expire explicitly. Never alter
 // journals, session/turn receipts or the live observer cursor.
 const files=fs.readdirSync(dir).filter(x=>/^[a-f0-9]{64}\.json$/.test(x)).map(name=>{const st=fs.lstatSync(path.join(dir,name));if(!st.isFile()||st.isSymbolicLink()||st.uid!==process.getuid()||(st.mode&0o077)!==0)fail('TRANSCRIPT_CACHE_UNSAFE');return {name,size:st.size,time:st.mtimeMs}}).sort((a,b)=>a.time-b.time||a.name.localeCompare(b.name));
 let bytes=files.reduce((n,f)=>n+f.size,0),count=files.length;
 for(const f of files){if(count<=64&&bytes<=16777216)break;if(f.name===digest+'.json')continue;try{fs.unlinkSync(path.join(dir,f.name));count--;bytes-=f.size}catch(e){if(e.code!=='ENOENT')throw e}}
 return Buffer.from(JSON.stringify({version:2,bound:state.bound,checkpoint:digest})).toString('base64url');
}
function acceptScanEvent(s,e,requestId){
 if(e.requestId!==requestId)return;s.found=true;
 const flush=()=>{if(s.open){if(s.discard)s.queue.push({kind:'text_omitted',role:s.open.role,sequence_start:s.open.first,sequence_end:s.open.last,reason:'logical_segment_bound',text:'Oversized logical text segment omitted before redaction; no partial credential text emitted.'});else textParts(redact(s.open.text)).forEach((text,part)=>s.queue.push({kind:'text',role:s.open.role,sequence_start:s.open.first,sequence_end:s.open.last,part,text}));s.open=null;s.discard=false}};
 if(e.type==='turn_started'){flush();if(++s.started>1)fail('TRANSCRIPT_REQUEST_ID_REUSED');return}
 if(e.type==='turn_result'){flush();if(s.terminal)fail('TRANSCRIPT_REQUEST_ID_REUSED');s.terminal=terminalSummary(e.result);return}
 const m=e.message,u=m?.params?.update,type=u?.sessionUpdate;
 if(e.type==='message'&&m?.method==='session/update'&&['agent_message_chunk','user_message_chunk'].includes(type)&&u.content?.type==='text'&&typeof u.content.text==='string'){
  const role=type==='agent_message_chunk'?'assistant':'user';if(s.open&&s.open.role!==role)flush();if(!s.open)s.open={role,first:e.sequence,last:e.sequence,text:''};s.open.last=e.sequence;
  if(!s.discard){if(Buffer.byteLength(s.open.text)+Buffer.byteLength(u.content.text)>65536){s.open.text='';s.discard=true}else s.open.text+=u.content.text}return;
 }
 if(type==='agent_thought_chunk')return;flush();
 if(type==='tool_call'||type==='tool_call_update'){if(type==='tool_call')s.tools++;s.queue.push({kind:type==='tool_call'?'tool_start':(['completed','failed','cancelled'].includes(u.status)?'tool_end':'tool_update'),sequence:e.sequence,call_id:enumValue(u.toolCallId),tool:enumValue(u.kind??'tool'),status:enumValue(u.status??'pending')})}
}
async function sampleAt(handle,offset,length){const buffer=Buffer.alloc(length);const {bytesRead}=await handle.read(buffer,0,length,offset);if(bytesRead!==length)fail('TRANSCRIPT_JOURNAL_TRUNCATED');return hash(buffer)}
async function incrementalTranscript(p,b,Reader,record,limit,maxBytes){
 const dir=transcriptCache(b),state=loadScan(p,b,dir),reader=new Reader({acpxRecordId:b.state.record_id,eventLog:{max_segments:record.event_log.max_segments}});
 const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),5000),page=[],start=state.index;
 let contentBytes=0,scanBytes=0,scanEvents=0,scanLines=0,unread=false,partial=false,snapshot;
 const drain=()=>{while(state.queue.length&&page.length<limit){const size=Buffer.byteLength(JSON.stringify(state.queue[0]));if(contentBytes+size>maxBytes)break;page.push(state.queue.shift());contentBytes+=size;state.index++}};
 try{
  snapshot=await reader.openSnapshot(controller.signal);
  const ids=new Set(snapshot.map(x=>x.identity));
  for(const [id,old]of Object.entries(state.segments))if(!ids.has(id)){if(old.offset<old.seen_size||old.pending)fail('TRANSCRIPT_ROTATION_GAP_RESTART_READ');delete state.segments[id]}
  drain();
  for(const segment of snapshot){
   controller.signal.throwIfAborted();const size=Number(segment.opened.stat.size);if(!Number.isSafeInteger(size)||size<0)fail('TRANSCRIPT_JOURNAL_UNAVAILABLE');
   let st=state.segments[segment.identity];
   if(!st)st=state.segments[segment.identity]={offset:0,pending:'',sequence:0,messageSequence:0,requestId:null,seen_size:size};
   if(size<st.offset||size<st.seen_size)fail('TRANSCRIPT_JOURNAL_TRUNCATED');
   if(st.head_hash&&await sampleAt(segment.opened.handle,0,st.head_length)!==st.head_hash)fail('TRANSCRIPT_JOURNAL_CHANGED');
   if(st.tail_hash&&await sampleAt(segment.opened.handle,st.tail_offset,st.tail_length)!==st.tail_hash)fail('TRANSCRIPT_JOURNAL_CHANGED');
   st.seen_size=size;
   let pending=Buffer.from(st.pending,'base64');delete st.pending;
   let stop=false;
   while(!stop){
    drain();if(page.length>=limit||contentBytes>=maxBytes||scanEvents>=2048||scanLines>=4096||scanBytes>=1048576){unread=pending.length>0||st.offset<size;stop=true;break}
    const end=pending.indexOf(10);
    if(end>=0){
     if(end>262144)fail('TRANSCRIPT_LINE_BOUND_EXCEEDED');const line=pending.subarray(0,end).toString('utf8');pending=pending.subarray(end+1);scanLines++;
     const eventPage={events:[],after:-1,bytes:0,maxBytes:Infinity},before=st.firstSequence;
     reader.consumeLine(line,st,eventPage,end+1);
     if(before===undefined&&st.firstSequence!==undefined&&state.global_sequence!==null&&st.firstSequence!==state.global_sequence)fail('TRANSCRIPT_JOURNAL_SEQUENCE_GAP');
     if(st.firstSequence!==undefined)state.global_sequence=st.sequence;
     for(const event of eventPage.events){scanEvents++;acceptScanEvent(state,event,b.turn.acpx_request_id)}
     continue;
    }
    if(pending.length>262144)fail('TRANSCRIPT_LINE_BOUND_EXCEEDED');
    if(st.offset>=size){partial=pending.length>0;break}
    const buffer=Buffer.alloc(Math.min(65536,size-st.offset,1048576-scanBytes));const {bytesRead}=await segment.opened.handle.read(buffer,0,buffer.length,st.offset);
    if(bytesRead===0)fail('TRANSCRIPT_JOURNAL_CHANGED');st.offset+=bytesRead;scanBytes+=bytesRead;pending=Buffer.concat([pending,buffer.subarray(0,bytesRead)]);
   }
   if(pending.length&&!stop&&st.offset>=size&&snapshot.slice(snapshot.indexOf(segment)+1).some(x=>Number(x.opened.stat.size)>0))fail('TRANSCRIPT_ROTATED_PARTIAL_LINE');
   st.pending=pending.toString('base64');
   st.head_length=Math.min(64,size);st.head_hash=await sampleAt(segment.opened.handle,0,st.head_length);
   st.tail_length=Math.min(64,st.offset);st.tail_offset=st.offset-st.tail_length;st.tail_hash=await sampleAt(segment.opened.handle,st.tail_offset,st.tail_length);
   if(stop){const idx=snapshot.indexOf(segment);unread ||= snapshot.slice(idx+1).some(x=>Number(x.opened.stat.size)>0);break}
  }
  drain();
  const scan=await reader.discoverPaths();if(!await reader.matchesSnapshot(scan.paths,snapshot,scan.holes))fail('TRANSCRIPT_ROTATION_CHANGED_RETRY_SAME_CURSOR');
 }finally{clearTimeout(timer);if(snapshot)await reader.closeSnapshot(snapshot)}
 const terminal=state.terminal??(b.turn.phase==='terminal'&&b.turn.terminal?terminalSummary(b.turn.terminal):null);
 const cursor=saveScan(state,dir),hasMore=unread||state.queue.length>0;
 emit({type:'request_transcript',...state.bound,entries:page,cursor,has_more:hasMore,page_start:start,page_end:state.index,content_bytes:contentBytes,terminal,terminal_source:state.terminal?'acpx_journal':terminal?'retained_turn_receipt':null,history_complete:state.started===1&&state.terminal!==null&&!hasMore,retained_prefix_complete:state.started===1,tail_buffered:state.open!==null,journal_request_found:state.found,observed_tool_calls:state.tools,scan_in_progress:hasMore,scan_bytes_read:scanBytes,scan_events:scanEvents,journal_snapshot_partial:partial,prompt_retry:false,read_only:true,observer_checkpoint_only:true,next_action:hasMore?'continue_transcript_cursor':terminal?'inspect_response_or_result':'watch_existing_request_no_resubmit'});
}

async function transcript(p,b){
 const limit=p.limit??50,maxBytes=p.max_bytes??65536;
 if(!Number.isSafeInteger(limit)||limit<1||limit>100||!Number.isSafeInteger(maxBytes)||maxBytes<2048||maxBytes>262144)fail('TRANSCRIPT_PAGE_BOUND_INVALID');
 const pkg=JSON.parse(fs.readFileSync(path.join(path.dirname(b.c.runtime_module),'..','package.json'),'utf8'));
 if(pkg.version!=='0.21.0')fail('TRANSCRIPT_ACPX_VERSION_UNSUPPORTED');
 const ipc=fs.readdirSync(path.dirname(b.c.runtime_module)).filter(x=>/^ipc-.*\.js$/.test(x));if(ipc.length!==1)fail('TRANSCRIPT_ACPX_READER_UNAVAILABLE');
 const exports=await import(pathToFileURL(path.join(path.dirname(b.c.runtime_module),ipc[0])).href);
 const readers=Object.values(exports).filter(x=>typeof x==='function'&&x.name==='SessionJournalReader'&&typeof x.prototype.read==='function'&&typeof x.prototype.readTail==='function');
 if(readers.length!==1)fail('TRANSCRIPT_ACPX_READER_UNAVAILABLE');
 const record=b.record;
 const maxSegments=record.event_log?.max_segments;if(!Number.isSafeInteger(maxSegments)||maxSegments<1||maxSegments>64)fail('TRANSCRIPT_RETENTION_BOUND_UNSUPPORTED');
 for(const method of ['openSnapshot','closeSnapshot','consumeLine','discoverPaths','matchesSnapshot'])if(typeof readers[0].prototype[method]!=='function')fail('TRANSCRIPT_ACPX_READER_UNAVAILABLE');
 await incrementalTranscript(p,b,readers[0],record,limit,maxBytes);
}
function atomic(file,value){const tmp=file+'.'+crypto.randomUUID();const fd=fs.openSync(tmp,'wx',0o600);try{fs.writeFileSync(fd,JSON.stringify(value));fs.fsyncSync(fd)}finally{fs.closeSync(fd)}fs.renameSync(tmp,file);const parent=fs.openSync(path.dirname(file),'r');try{fs.fsyncSync(parent)}finally{fs.closeSync(parent)}}
const quote=x=>"'"+x.replaceAll("'","'\"'\"'")+"'";
async function watch(p,b,input){
 let child=null,closed=false,exit=null,closing=null;
 const stop=()=>closing??=(async()=>{
  if(child&&!closed){child.kill('SIGTERM');await Promise.race([exit,new Promise(r=>setTimeout(r,300))]);if(!closed){child.kill('SIGKILL');await exit;}}
 })();
 const onEnd=()=>{input.disconnected=true;void stop()};
 process.stdin.on('end',onEnd);process.stdout.on('error',onEnd);process.once('SIGTERM',onEnd);
 try{
  if(input.disconnected)return;
  if(b.turn.phase==='terminal'&&b.turn.terminal&&b.turn.cursor){emit({type:'watch',request_id:p.request_id,event:{type:'turn_result',requestId:b.turn.acpx_request_id,cursor:b.turn.cursor,result:b.turn.terminal}});return;}
  const version=spawnSync(b.c.acpx_bin,['--version'],{encoding:'utf8',timeout:2000,maxBuffer:65536});
  if(version.status!==0||version.stdout.trim()!=='0.21.0')fail('WATCH_ACPX_VERSION_MISMATCH');
  if(input.disconnected)return;
  const args=['--agent',[b.c.node,b.c.launcher,b.dir].map(quote).join(' '),'--cwd',b.c.workspace,'--ttl','0','--format','json','--prompt-retries','0','--deny-all','--suppress-reads'];
  if(b.c.model)args.push('--model',b.c.model);
  args.push('sessions','watch','--name',p.session_id);if(p.cursor)args.push('--cursor',p.cursor);
  child=spawn(b.c.acpx_bin,args,{stdio:['ignore','pipe','ignore']});
  exit=new Promise(resolve=>{child.once('close',()=>{closed=true;resolve()});child.once('error',()=>{closed=true;resolve()})});
  let terminal=false;
  const lines=readline.createInterface({input:child.stdout});
  for await(const line of lines){
   if(input.disconnected)break;
   if(Buffer.byteLength(line)>2097152)fail('WATCH_FRAME_BOUND_OUTCOME_UNKNOWN');
   const e=JSON.parse(line);if(e.requestId!==b.turn.acpx_request_id)continue;
   if(e.type==='turn_result'){
    if(b.turn.phase==='terminal'&&JSON.stringify(b.turn.terminal)!==JSON.stringify(e.result))fail('WATCH_TERMINAL_METADATA_CHANGED');
    b.turn.phase='terminal';b.turn.terminal=e.result;b.turn.cursor=e.cursor;
    atomic(path.join(b.dir,'turn-'+p.request_id+'.json'),b.turn);
    const fd=fs.openSync(path.join(b.dir,'lifecycle.jsonl'),'a',0o600);try{fs.writeSync(fd,JSON.stringify({component:'agent-plugin',stage:'request_terminal',session_id:p.session_id,request_id:p.request_id,time:Date.now()/1000})+'\n');fs.fsyncSync(fd)}finally{fs.closeSync(fd)}
    terminal=true;
   }
   emit({type:'watch',request_id:p.request_id,event:e});if(terminal)break;
  }
  if(!terminal&&!input.disconnected)fail('OWNER_UNAVAILABLE_OUTCOME_UNKNOWN');
 }finally{await stop();process.stdin.off('end',onEnd);process.stdout.off('error',onEnd);process.off('SIGTERM',onEnd);process.stdin.pause();process.stdin.destroy();}
}
function cancelIdentity(){
 const machine=fs.readFileSync('/etc/machine-id','utf8').trim(),boot=fs.readFileSync('/proc/sys/kernel/random/boot_id','utf8').trim();
 if(!/^[a-f0-9]{32}$/.test(machine)||!uuid(boot))fail('REQUEST_CANCEL_MACHINE_IDENTITY_UNAVAILABLE');return {machine_id:machine,boot_id:boot};
}
export async function nativeTargetedCancel(c,record,target){
 const pkg=JSON.parse(fs.readFileSync(path.join(path.dirname(c.runtime_module),'..','package.json'),'utf8'));if(pkg.version!=='0.21.0')fail('REQUEST_CANCEL_ACPX_VERSION_UNSUPPORTED');
 const files=fs.readdirSync(path.dirname(c.runtime_module)).filter(x=>/^ipc-.*\.js$/.test(x));if(files.length!==1)fail('REQUEST_CANCEL_NATIVE_API_UNAVAILABLE');
 const mod=await import(pathToFileURL(path.join(path.dirname(c.runtime_module),files[0])).href),matches=Object.values(mod).filter(x=>typeof x==='function'&&x.name==='tryCancelOnRunningOwner');
 if(matches.length!==1)fail('REQUEST_CANCEL_NATIVE_API_UNAVAILABLE');
 // Native ownerGeneration/incarnation checks and targetRequestId fencing remain
 // upstream. This path never calls ensure/start/prompt or session-wide cancel.
 return await matches[0]({sessionId:record,targetRequestId:target,verbose:false});
}
export async function cancelRequest(p,b,deps={identity:cancelIdentity,dispatch:nativeTargetedCancel}){
 const identity=deps.identity(),file=path.join(b.dir,'request-cancel-'+p.request_id+'.json');
 const bound={session_id:p.session_id,request_id:p.request_id,record_id:b.state.record_id,acpx_request_id:b.turn.acpx_request_id,prompt_sha256:p.prompt_sha256,config_hash:b.state.config_hash,identity};
 const sameIdentity=x=>x&&Object.keys(x).sort().join(',')==='boot_id,machine_id'&&x.machine_id===identity.machine_id&&x.boot_id===identity.boot_id;
 const same=x=>['session_id','request_id','record_id','acpx_request_id','prompt_sha256','config_hash'].every(k=>x[k]===bound[k])&&sameIdentity(x.identity);
 let saved;try{saved=privateJSON(file,65536);if(!same(saved))fail('REQUEST_CANCEL_SAVED_BINDING_CHANGED')}catch(e){if(e.code!=='ENOENT')throw e}
 const terminal=b.turn.phase==='terminal'?terminalSummary(b.turn.terminal):null;
 if(b.turn.phase==='terminal'&&(!terminal||!['completed','cancelled','failed'].includes(terminal.status)))fail('REQUEST_CANCEL_TERMINAL_PROOF_INVALID');
 const answer=(phase,error_code=null)=>({type:p.action==='cancel-status'?'request_cancel_status':'request_cancel',...bound,phase,terminal,cancel_is_terminal_proof:false,prompt_retry:false,...error_code?{error_code}:{}});
 if(p.expected_identity&&!sameIdentity(p.expected_identity))fail('REQUEST_CANCEL_MACHINE_CHANGED');
 if(p.action==='cancel-status')return answer(terminal?'terminal_observed':saved?.phase??'not_requested',saved?.error_code);
 if(p.action!=='cancel'||!p.expected_identity)fail('REQUEST_CANCEL_PREFLIGHT_REQUIRED');
 if(terminal)return answer('terminal_observed');
 if(saved)return answer(saved.phase,saved.error_code); // crash/timeout: lookup only, NEVER dispatch.
 if(b.state.phase!=='ready')fail('REQUEST_CANCEL_SESSION_FINISHING');
 if(b.turn.phase!=='queued')fail('REQUEST_CANCEL_TARGET_STATE_UNCONFIRMED');
 const unknown={...bound,phase:'cancel_unknown'};
 try{const fd=fs.openSync(file,'wx',0o600);try{fs.writeSync(fd,JSON.stringify(unknown));fs.fsyncSync(fd)}finally{fs.closeSync(fd)}const d=fs.openSync(b.dir,'r');try{fs.fsyncSync(d)}finally{fs.closeSync(d)}}catch(e){if(e.code==='EEXIST'){saved=privateJSON(file,65536);if(!same(saved))fail('REQUEST_CANCEL_SAVED_BINDING_CHANGED');return answer(saved.phase,saved.error_code)}throw e}
 try{
  const result=await deps.dispatch(b.c,b.state.record_id,b.turn.acpx_request_id);
  const phase=result===true?'cancel_accepted':result===false?'target_not_cancelled':'owner_unavailable';
  atomic(file,{...bound,phase});return answer(phase);
 }catch(error){const raw=error.detailCode??error.code,code=typeof raw==='string'&&/^[A-Z_]{1,100}$/.test(raw)?raw:'REQUEST_CANCEL_NATIVE_OUTCOME_UNKNOWN';atomic(file,{...bound,phase:'cancel_unknown',error_code:code});return answer('cancel_unknown',code)}
}

export async function main(){
 const input={disconnected:false};process.stdin.once('end',()=>{input.disconnected=true});
 try{
  const p=await new Promise((resolve,reject)=>{let raw='';const onData=chunk=>{raw+=chunk;if(Buffer.byteLength(raw)>2097152){reject(Error('INPUT_BOUND'));return}const end=raw.indexOf('\n');if(end>=0){process.stdin.off('data',onData);try{resolve(JSON.parse(raw.slice(0,end)))}catch{reject(Error('INPUT_JSON'))}}};process.stdin.on('data',onData);process.stdin.once('end',()=>reject(Error('INPUT_CLOSED')))});
  const b=binding(p);
  if(p.action==='transcript')await transcript(p,b);
  else if(p.action==='watch')await watch(p,b,input);
  else if(p.action==='cancel'||p.action==='cancel-status')emit(await cancelRequest(p,b));
  else fail('REQUEST_OBSERVER_ACTION_UNSUPPORTED');
 }catch(error){const code=typeof error.code==='string'&&/^[A-Z_]{1,100}$/.test(error.code)?error.code:'REQUEST_OBSERVER_UNAVAILABLE';emit({type:'error',code,prompt_retry:false});process.exitCode=1}
}
