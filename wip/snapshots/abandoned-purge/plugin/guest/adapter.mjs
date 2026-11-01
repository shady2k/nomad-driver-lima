// Ephemeral CLI of the Claude plugin, invoked through native Nomad exec.
// Upstream acpx owns the live ACP connection; this is not a companion service.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {spawn,spawnSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';
import readline from 'node:readline';
const sha=b=>crypto.createHash('sha256').update(b).digest('hex');
const uuid=x=>typeof x==='string'&&/^[a-f0-9-]{36}$/.test(x);
let child=null,shared=null;
process.stdout.on('error',()=>{child?.kill('SIGTERM');shared?.shutdown().finally(()=>process.exit(0));if(!shared)process.exit(0)});
process.on('SIGTERM',()=>{child?.kill('SIGTERM');shared?.shutdown().finally(()=>process.exit(0));if(!shared)process.exit(0)});
const emit=x=>process.stdout.write(JSON.stringify(x)+'\n');
function fail(code){throw Object.assign(Error(code),{code})}
function atomic(file,v){const tmp=file+'.'+crypto.randomUUID();const fd=fs.openSync(tmp,'wx',0o600);try{fs.writeFileSync(fd,JSON.stringify(v));fs.fsyncSync(fd)}finally{fs.closeSync(fd)}fs.renameSync(tmp,file);const d=fs.openSync(path.dirname(file),'r');try{fs.fsyncSync(d)}finally{fs.closeSync(d)}}
function read(file){return JSON.parse(fs.readFileSync(file,'utf8'))}
function exclusive(file,v){const fd=fs.openSync(file,'wx',0o600);try{fs.writeFileSync(fd,JSON.stringify(v));fs.fsyncSync(fd)}finally{fs.closeSync(fd)}}
function quote(x){return "'"+x.replaceAll("'","'\"'\"'")+"'"}
function runtimeGuard(c,dir,configHash){
 if(!c.prime_vault)return null;
 const ready=JSON.parse(fs.readFileSync(path.join(dir,'runtime-ready.json'),'utf8'));
 if(ready.phase!=='ready'||ready.config_hash!==configHash)throw Error('runtime ownership');
 if(ready.models_sha256&&crypto.createHash('sha256').update(fs.readFileSync(path.join(dir,'prime-config','models.json'))).digest('hex')!==ready.models_sha256)throw Error('native model catalog changed');
 for(const row of [...ready.processes,ready.controller]){const fields=fs.readFileSync('/proc/'+row.pid+'/stat','utf8').split(') ').slice(1).join(') ').trim().split(/\s+/);if(fields[0]==='Z'||fields[19]!==row.ticks)throw Error('runtime owner died')}
 const ss=fs.lstatSync(ready.socket);if(!ss.isSocket()||ss.uid!==process.getuid()||ss.dev!==ready.socket_dev||ss.ino!==ready.socket_ino)throw Error('runtime socket changed');
 return ready;
}
let raw='';for await(const b of process.stdin){raw+=b;if(Buffer.byteLength(raw)>2097152)fail('INPUT_TOO_LARGE')}
try {
 const p=JSON.parse(raw),c=p.config;
 if(!uuid(p.session_id)||c.protocol_version!==1||!path.isAbsolute(c.state_dir)||!path.isAbsolute(c.workspace)||!Array.isArray(c.agent_argv)||!path.isAbsolute(c.agent_argv[0]))fail('INVALID_CONFIG');
 for(const k of ['node','launcher','acpx_bin','runtime_module'])if(!path.isAbsolute(c[k]))fail('INVALID_CONFIG');
 if(!Number.isSafeInteger(c.max_artifact_bytes)||c.max_artifact_bytes<1||c.max_artifact_bytes>67108864)fail('INVALID_ARTIFACT_BOUND');
 if(c.acpx_version!=='0.21.0')fail('UNSUPPORTED_ACPX_CONTRACT');
 const version=spawnSync(c.acpx_bin,['--version'],{encoding:'utf8'});if(version.status!==0||version.stdout.trim()!==c.acpx_version)fail('ACPX_VERSION_MISMATCH');
 const dir=path.join(c.state_dir,p.session_id);fs.mkdirSync(dir,{recursive:true,mode:0o700});
 if((fs.statSync(dir).mode&0o077)!==0)fail('STATE_NOT_PRIVATE');
 const stateFile=path.join(dir,'session.json'),configHash=sha(JSON.stringify(c));
 const turnFile=()=>{if(!uuid(p.request_id))fail('INVALID_REQUEST');return path.join(dir,'turn-'+p.request_id+'.json')};
 const base=[c.acpx_bin,'--agent',[c.node,c.launcher,dir].map(quote).join(' '),'--cwd',c.workspace,'--ttl','0','--format','json','--prompt-retries','0','--deny-all','--suppress-reads'];
 if(c.model)base.push('--model',c.model);
 function cli(args,input=''){const r=spawnSync(base[0],[...base.slice(1),...args],{input,encoding:'utf8',maxBuffer:2097152,timeout:args[0]==='status'?5000:30000});if(r.status!==0)fail('ACPX_COMMAND_FAILED');return r.stdout.split('\n').filter(Boolean).map(x=>JSON.parse(x))}
 function status(){runtimeGuard(c,dir,configHash);return cli(['status','-s',p.session_id])[0]}
 function permit(purpose){exclusive(path.join(dir,'launch-permit.json'),{purpose,nonce:crypto.randomUUID()})}
 function state(){const s=read(stateFile);if(s.config_hash!==configHash)fail('CONFIG_CHANGED');return s}
 function artifact(rel){if(typeof rel!=='string'||path.isAbsolute(rel)||rel.split('/').some(x=>!x||x==='..'||x.startsWith('.')||/^(id_rsa|id_ed25519|credentials|token)(\.|$)/i.test(x)))fail('PATH_NOT_ALLOWED');const root=fs.realpathSync(c.workspace),target=fs.realpathSync(path.join(root,rel));if(!target.startsWith(root+path.sep))fail('PATH_ESCAPE');const st=fs.statSync(target);if(!st.isFile()||st.size>c.max_artifact_bytes)fail('ARTIFACT_TOO_LARGE');const bytes=fs.readFileSync(target);return {path:rel,size:bytes.length,sha256:sha(bytes),data:bytes.toString('base64')}}
 function lifecycle(stage,fields={}){
  if(typeof stage!=='string'||!/^[a-z_]{1,64}$/.test(stage))fail('INVALID_LIFECYCLE_STAGE');
  const event={component:'agent-plugin',stage,session_id:p.session_id,time:Date.now()/1000};
  if(uuid(fields.request_id))event.request_id=fields.request_id;
  for(const k of ['bytes','total_bytes','duration_ms'])if(Number.isSafeInteger(fields[k])&&fields[k]>=0)event[k]=fields[k];
  const fd=fs.openSync(path.join(dir,'lifecycle.jsonl'),'a',0o600);try{fs.writeSync(fd,JSON.stringify(event)+'\n');fs.fsyncSync(fd)}finally{fs.closeSync(fd)}
 }
 if(p.action==='lifecycle'){lifecycle(p.stage,p);emit({type:'lifecycle_recorded'})}
 else if(p.action==='finish_ack'){
  const s=state();if(s.phase!=='finishing'||!s.closed||s.finish_manifest_sha256!==p.manifest_sha256)fail('FINISH_RECEIPT_STATE_MISMATCH');if(typeof p.manifest_sha256!=='string'||!/^[a-f0-9]{64}$/.test(p.manifest_sha256))fail('INVALID_FINISH_RECEIPT');
  lifecycle('receipt_verified');atomic(path.join(dir,'finish.json'),{session_id:p.session_id,manifest_sha256:p.manifest_sha256,export_verified:true});emit({type:'finish_acknowledged'})
 }
 else if(p.action==='create'){
  if(fs.existsSync(stateFile)){const s=state();try{runtimeGuard(c,dir,configHash)}catch{fail('OWNED_PRIME_RUNTIME_UNAVAILABLE')};emit({type:'session',phase:s.phase,session_id:p.session_id,record_id:s.record_id});process.exit(0)}
  exclusive(stateFile,{config:c,config_hash:configHash,phase:'creating',bootstrap_used:false});
  if(c.prime_vault){
   atomic(path.join(dir,'runtime-request.json'),{config:c,config_hash:configHash});
   const deadline=Date.now()+35000;
   while(!fs.existsSync(path.join(dir,'runtime-ready.json'))){if(fs.existsSync(path.join(dir,'runtime-failed.json')))fail('PRIME_RUNTIME_FAILED');if(Date.now()>deadline)fail('PRIME_RUNTIME_READY_TIMEOUT');await new Promise(r=>setTimeout(r,100))}
   try{runtimeGuard(c,dir,configHash)}catch{fail('OWNED_PRIME_RUNTIME_UNAVAILABLE')}
  }
  permit('create');
  const result=cli(['sessions','new','--name',p.session_id]);const record=result.find(x=>x.acpxRecordId);if(!record)fail('CREATE_OUTCOME_UNKNOWN');
  const s=state();s.record_id=record.acpxRecordId;s.phase='ready';atomic(stateFile,s);lifecycle('acp_ready');emit({type:'session',phase:s.phase,session_id:p.session_id,record_id:s.record_id});
 } else if(p.action==='status') {const s=state();emit({type:'status',phase:s.phase,owner:status(),runtime_contract:c.runtime_contract??null,effective_provider_config_verified:false})
 } else if(p.action==='select_model') {
  const lock=path.join(dir,'submission.lock');exclusive(lock,{pid:process.pid,action:'select_model'});
  try {
   const s=state();if(s.phase!=='ready')fail('SESSION_FINISHING_NO_MODEL_CHANGE');
   const turns=fs.readdirSync(dir).filter(x=>/^turn-.*\.json$/.test(x)).map(x=>read(path.join(dir,x)));if(turns.some(t=>t.phase!=='terminal'))fail('TURN_ACTIVE_OR_UNKNOWN');
   runtimeGuard(c,dir,configHash);if(status().status!=='alive')fail('OWNER_DIED_NO_MODEL_RESTART');
   const models=read(path.join(dir,'prime-config','models.json'));const matches=[];
   for(const [provider,value] of Object.entries(models.providers??{}))for(const model of value.models??[])if(model.id===p.model_id)matches.push([provider,model.id]);
   if(matches.length!==1)fail('MODEL_NOT_IN_NATIVE_CATALOG');const value=JSON.stringify(matches[0]);if(value!==p.coordinator_value)fail('MODEL_SELECTOR_MISMATCH');
   // Official acpx owner-routed control. No launch permit is granted: owner loss
   // cannot create another ACP transport/Prime session through our launcher.
   const result=cli(['set','model',value,'-s',p.session_id]);const ack=result.find(x=>x.action==='model_set');
   if(!ack||ack.modelId!==value||ack.resumed!==false||ack.acpxRecordId!==s.record_id)fail('MODEL_SELECTION_OUTCOME_UNKNOWN');
   s.coordinator_model={id:p.model_id,acp_value:value};atomic(stateFile,s);lifecycle('coordinator_model_selected');emit({type:'model_selected',model_id:p.model_id,coordinator_value:value,worker_models_changed:false});
  } finally {fs.unlinkSync(lock)}
 } else if(p.action==='submit'){
  const s=state(),file=turnFile();if(fs.existsSync(file)){const t=read(file);if(t.prompt_sha256!==sha(p.prompt??''))fail('REQUEST_CONTENT_CHANGED');emit({type:'submitted',...t,replayed_submission:false});process.exit(0)}
  if(s.phase!=='ready')fail('SESSION_NOT_READY');try{runtimeGuard(c,dir,configHash)}catch{fail('OWNED_PRIME_RUNTIME_UNAVAILABLE')};if(typeof p.prompt!=='string'||Buffer.byteLength(p.prompt)>1048576)fail('INVALID_PROMPT');
  const lock=path.join(dir,'submission.lock');exclusive(lock,{pid:process.pid,request_id:p.request_id});
  try {
   Object.assign(s,state());if(s.phase!=='ready')fail('SESSION_FINISHING_NO_NEW_TURN');
   const t={request_id:p.request_id,phase:'intent',prompt_sha256:sha(p.prompt)};exclusive(file,t);
   if(!s.bootstrap_used){
    s.bootstrap_used=true;atomic(stateFile,s);permit('initial-submit');
    const result=cli(['prompt','-s',p.session_id,'--no-wait','--file','-'],p.prompt);const queued=result.find(x=>x.action==='prompt_queued');if(!queued)fail('SUBMIT_OUTCOME_UNKNOWN');
    t.acpx_request_id=queued.requestId;t.phase='queued';atomic(file,t);lifecycle('request_running',{request_id:p.request_id});emit({type:'submitted',...t});
   } else {
    if(status().status!=='alive')fail('OWNER_DIED');
    const api=await import(pathToFileURL(c.runtime_module).href);shared=api.createSharedAcpRuntime({cwd:c.workspace,ttlMs:0,permissionMode:'deny-all',nonInteractivePermissions:'deny',promptRetries:0});
    const handle={backend:'acpx-shared',acpxRecordId:s.record_id,runtimeSessionName:s.record_id,sessionKey:p.session_id,cwd:c.workspace};
    const turn=shared.startTurn({handle,requestId:p.request_id,mode:'prompt',text:p.prompt});
    try{await turn.promptStarted}catch{fail('OWNER_UNAVAILABLE_NO_RETRY')}
    t.acpx_request_id=p.request_id;t.phase='queued';atomic(file,t);lifecycle('request_running',{request_id:p.request_id});emit({type:'submitted',...t});await shared.shutdown();shared=null;
   }
  }finally{fs.unlinkSync(lock)}
 } else if(p.action==='watch'){
  state();const file=turnFile(),t=read(file);if(!t.acpx_request_id)fail('SUBMIT_OUTCOME_UNKNOWN');
  const args=['sessions','watch','--name',p.session_id];if(p.cursor)args.push('--cursor',p.cursor);
  child=spawn(base[0],[...base.slice(1),...args],{stdio:['ignore','pipe','pipe']});let settled=false;
  const lines=readline.createInterface({input:child.stdout});
  const health=setInterval(()=>{if(!settled){try{if(status().status!=='alive')child.kill('SIGTERM')}catch{child.kill('SIGTERM')}}},1000);
  try {for await(const line of lines){const e=JSON.parse(line);if(e.requestId!==t.acpx_request_id)continue;emit({type:'watch',request_id:p.request_id,event:e});if(e.type==='turn_result'){lifecycle('request_terminal',{request_id:p.request_id});t.phase='terminal';t.terminal=e.result;t.cursor=e.cursor;atomic(file,t);settled=true;child.kill('SIGTERM');break}}}
  finally{clearInterval(health)}
  if(!settled)fail('OWNER_UNAVAILABLE_OUTCOME_UNKNOWN');
 } else if(p.action==='artifact') {state();const t=read(turnFile());if(t.phase!=='terminal')fail('TURN_NOT_TERMINAL');emit({type:'artifact',request_id:p.request_id,...artifact(p.path)})
 } else if(p.action==='prepare_finish') {
  const lock=path.join(dir,'submission.lock');exclusive(lock,{pid:process.pid,action:'prepare_finish'});
  try {
   const s=state();if(!Array.isArray(p.files)||!p.files.length)fail('EMPTY_MANIFEST');
   if(typeof p.manifest_sha256!=='string'||!/^[a-f0-9]{64}$/.test(p.manifest_sha256))fail('INVALID_FINISH_RECEIPT');
   if(s.phase!=='ready'&&!(s.phase==='finishing'&&s.finish_manifest_sha256===p.manifest_sha256))fail('FINISH_STATE_MISMATCH');
   const turns=fs.readdirSync(dir).filter(x=>/^turn-.*\.json$/.test(x)).map(x=>read(path.join(dir,x)));if(turns.some(t=>t.phase!=='terminal'))fail('UNFINISHED_OR_UNKNOWN_TURN');
   for(const f of p.files){const a=artifact(f.path);if(a.sha256!==f.sha256||a.size!==f.size)fail('RESULT_CHANGED')}
   s.phase='finishing';s.finish_manifest_sha256=p.manifest_sha256;atomic(stateFile,s);lifecycle('finish_prepared');emit({type:'finish_ready',record_id:s.record_id});
  } finally {fs.unlinkSync(lock)}
 } else if(p.action==='close') {const s=state();if(s.phase!=='finishing')fail('FINISH_NOT_PREPARED');if(!s.closed){cli(['sessions','close',p.session_id]);s.closed=true;atomic(stateFile,s)}emit({type:'closed'})
 } else fail('UNKNOWN_ACTION');
} catch(e){emit({type:'error',code:/^[A-Z_]+$/.test(e.code??'')?e.code:'ADAPTER_ERROR',outcome:'unknown',retry_prompt:false});process.exitCode=1}
