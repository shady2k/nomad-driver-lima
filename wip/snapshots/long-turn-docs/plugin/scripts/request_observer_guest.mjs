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
 if(state.config_hash!==hash(JSON.stringify(c))||!uuid(state.record_id)||state.record_id!==p.record_id)fail('TRANSCRIPT_SESSION_BINDING_CHANGED');
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
 const record=privateJSON(path.join(os.homedir(),'.acpx','sessions',state.record_id+'.json'),8388608);
 if(record.schema!=='acpx.session.v1'||record.acpx_record_id!==state.record_id||record.name!==p.session_id)fail('TRANSCRIPT_ACPX_RECORD_CHANGED');
 if(p.acpx_request_id!=null&&p.acpx_request_id!==turn.acpx_request_id)fail('TRANSCRIPT_REQUEST_BINDING_CHANGED');
 return {c,dir,state,turn,record};
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
 if(typeof readers[0].prototype.openPresentSegments!=='function')fail('TRANSCRIPT_ACPX_READER_UNAVAILABLE');
 class BoundedReader extends readers[0]{
  async openPresentSegments(segments,paths){
   const result=await super.openPresentSegments(segments,paths);let bytes=0;
   // Bound the real opened snapshot before parsing, including incomplete lines.
   // An event-only quota would miss a huge unterminated journal record.
   for(const segment of segments){const size=Number(segment.opened.stat.size);if(!Number.isSafeInteger(size)||size<0)fail('TRANSCRIPT_SCAN_BOUND_EXCEEDED');bytes+=size;if(bytes>8388608)fail('TRANSCRIPT_SCAN_BOUND_EXCEEDED');}
   return result;
  }
 }
 const reader=new BoundedReader({acpxRecordId:b.state.record_id,eventLog:{max_segments:maxSegments}});
 const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),5000);let after=-1,total=0,events=[],snapshot;
 try{do{
  snapshot=await reader.read(controller.signal,{after,maxBytes:262144});
  for(const e of snapshot.events){total+=Buffer.byteLength(JSON.stringify(e));if(total>8388608||events.length>=32768)fail('TRANSCRIPT_SCAN_BOUND_EXCEEDED');events.push(e);after=e.sequence;}
  if(snapshot.hasMore&&snapshot.events.length===0)fail('TRANSCRIPT_JOURNAL_UNAVAILABLE');
 }while(snapshot.hasMore)}finally{clearTimeout(timer)}
 const projection=project(events,b.turn.acpx_request_id);
 let terminalSource=projection.terminal?'acpx_journal':null;
 if(!projection.terminal&&b.turn.phase==='terminal'&&b.turn.terminal){projection.terminal=terminalSummary(b.turn.terminal);terminalSource='retained_turn_receipt';projection.history_complete=false;}
 const bound={session_id:p.session_id,request_id:p.request_id,record_id:b.state.record_id,acpx_request_id:b.turn.acpx_request_id,prompt_sha256:b.turn.prompt_sha256,config_hash:b.state.config_hash};
 emit({type:'request_transcript',...bound,...paginate(projection,bound,p.cursor,limit,maxBytes),terminal_source:terminalSource,journal_snapshot_partial:!!snapshot.activePartial,prompt_retry:false,read_only:true,next_action:projection.terminal?'inspect_response_or_result':'watch_existing_request_no_resubmit'});
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
export async function main(){
 const input={disconnected:false};process.stdin.once('end',()=>{input.disconnected=true});
 try{
  const p=await new Promise((resolve,reject)=>{let raw='';const onData=chunk=>{raw+=chunk;if(Buffer.byteLength(raw)>2097152){reject(Error('INPUT_BOUND'));return}const end=raw.indexOf('\n');if(end>=0){process.stdin.off('data',onData);try{resolve(JSON.parse(raw.slice(0,end)))}catch{reject(Error('INPUT_JSON'))}}};process.stdin.on('data',onData);process.stdin.once('end',()=>reject(Error('INPUT_CLOSED')))});
  const b=binding(p);
  if(p.action==='transcript')await transcript(p,b);
  else if(p.action==='watch')await watch(p,b,input);
  else fail('REQUEST_OBSERVER_ACTION_UNSUPPORTED');
 }catch(error){const code=typeof error.code==='string'&&/^[A-Z_]{1,100}$/.test(error.code)?error.code:'REQUEST_OBSERVER_UNAVAILABLE';emit({type:'error',code,prompt_retry:false});process.exitCode=1}
}
