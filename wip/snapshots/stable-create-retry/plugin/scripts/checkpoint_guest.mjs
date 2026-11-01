// Ephemeral owned management checkpoint. No model request or session creation.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {spawnSync} from 'node:child_process';
const hash=b=>crypto.createHash('sha256').update(b).digest('hex');
const canon=v=>JSON.stringify(v&&typeof v==='object'?Array.isArray(v)?v.map(x=>JSON.parse(canon(x))):Object.fromEntries(Object.keys(v).sort().map(k=>[k,JSON.parse(canon(v[k]))])):v);
function fail(code){throw Object.assign(Error(code),{code})}
function privatePath(p,dir=false){const s=fs.lstatSync(p);if(s.isSymbolicLink()||s.uid!==process.getuid()||(s.mode&0o077)||!(dir?s.isDirectory():s.isFile()))fail('CHECKPOINT_PRIVATE_PATH_REQUIRED');return s}
function read(p){const s=privatePath(p);if(s.size>2097152)fail('CHECKPOINT_RECORD_BOUND');return JSON.parse(fs.readFileSync(p,'utf8'))}
function atomic(p,value){const tmp=p+'.'+crypto.randomUUID();const f=fs.openSync(tmp,'wx',0o600);try{fs.writeFileSync(f,JSON.stringify(value));fs.fsyncSync(f)}finally{fs.closeSync(f)}fs.renameSync(tmp,p);const d=fs.openSync(path.dirname(p),'r');try{fs.fsyncSync(d)}finally{fs.closeSync(d)}}
function identity(){return {machine_id:fs.readFileSync('/etc/machine-id','utf8').trim(),boot_id:fs.readFileSync('/proc/sys/kernel/random/boot_id','utf8').trim()}}
function quote(x){return "'"+x.replaceAll("'","'\"'\"'")+"'"}
export function run(p,readIdentity=identity){
 const c=p.config;if(!/^[a-f0-9-]{36}$/.test(p.session_id)||!path.isAbsolute(c.state_dir)||!path.isAbsolute(c.workspace))fail('CHECKPOINT_CONFIG_INVALID');
 const base=c.state_dir,dir=path.join(base,p.session_id);privatePath(base,true);if(!fs.existsSync(dir))fs.mkdirSync(dir,{mode:0o700});privatePath(dir,true);
 const file=path.join(dir,'session.json'),state=fs.existsSync(file)?read(file):null;
 if(state&&(canon(state.config)!==canon(c)||state.config_hash!==hash(JSON.stringify(c))))fail('CHECKPOINT_GUEST_CONFIG_CHANGED');
 const machine=readIdentity();if(p.machine_identity&&canon(machine)!==canon(p.machine_identity))fail('CHECKPOINT_MACHINE_BOOT_CHANGED');
 function idle(){
  const names=fs.readdirSync(dir);if(names.length>2048)fail('CHECKPOINT_RECORD_BOUND');
  if(names.some(n=>/^turn-.*\.json$/.test(n)))fail('CHECKPOINT_AGENT_TURN_PRESENT_USE_TURN_FINISH');
  const seen=[];const ops=path.join(dir,'operations');if(fs.existsSync(ops)){
   privatePath(ops,true);const entries=fs.readdirSync(ops,{withFileTypes:true});if(entries.length>1024)fail('CHECKPOINT_OPERATION_BOUND');
   for(const e of entries){if(e.name==='creation.lock')continue;if(!e.isDirectory()||e.isSymbolicLink())fail('CHECKPOINT_OPERATION_PATH_CHANGED');seen.push(e.name);const o=read(path.join(ops,e.name,'state.json'));if(!['completed','failed','cancelled','timed_out'].includes(o.phase)||o.output_truncated)fail('CHECKPOINT_RUNNING_OR_UNKNOWN_OPERATION');if(canon(o.binding)!==canon(p.binding))fail('CHECKPOINT_OPERATION_OWNER_CHANGED')}
  }
  if(canon(seen.sort())!==canon((p.known_operation_ids??[]).slice().sort()))fail('CHECKPOINT_OPERATION_LEDGER_INCOMPLETE');
  const uploads=path.join(dir,'uploads'),inputIds=[];if(fs.existsSync(uploads)){privatePath(uploads,true);const names=fs.readdirSync(uploads);if(names.length>2048)fail('CHECKPOINT_RECORD_BOUND');for(const name of names){if(!/^[a-f0-9-]{36}\.json$/.test(name))continue;inputIds.push(name.slice(0,-5));const u=read(path.join(uploads,name));if(!['completed','aborted'].includes(u.phase)||(u.phase==='completed'&&u.binding.extract_to&&!u.extract_verified))fail('CHECKPOINT_INPUT_TRANSFER_UNFINISHED');if(canon(u.binding.binding)!==canon(p.binding))fail('CHECKPOINT_UPLOAD_OWNER_CHANGED')}}
  if(canon(inputIds.sort())!==canon((p.known_upload_ids??[]).slice().sort()))fail('CHECKPOINT_UPLOAD_LEDGER_INCOMPLETE');
 }
 function artifact(rel){if(typeof rel!=='string'||path.isAbsolute(rel)||rel.split('/').some(x=>!x||x==='..'||x.startsWith('.')||/^(id_rsa|id_ed25519|credentials|token)(\.|$)/i.test(x)))fail('PATH_NOT_ALLOWED');const root=fs.realpathSync(c.workspace),target=fs.realpathSync(path.join(root,rel));if(!target.startsWith(root+path.sep))fail('PATH_ESCAPE');const s=fs.statSync(target);if(!s.isFile()||!Number.isSafeInteger(c.max_artifact_bytes)||c.max_artifact_bytes<1||c.max_artifact_bytes>67108864||s.size>c.max_artifact_bytes)fail('ARTIFACT_TOO_LARGE');const b=fs.readFileSync(target);return {path:rel,size:b.length,sha256:hash(b),data:b.toString('base64')}}
 if(p.action==='lifecycle'){if(typeof p.stage!=='string'||!/^[a-z_]{1,64}$/.test(p.stage))fail('INVALID_LIFECYCLE_STAGE');const f=fs.openSync(path.join(dir,'lifecycle.jsonl'),'a',0o600);try{fs.writeSync(f,JSON.stringify({component:'agent-plugin',stage:p.stage,session_id:p.session_id,time:Date.now()/1000})+'\n');fs.fsyncSync(f)}finally{fs.closeSync(f)}return {type:'lifecycle_recorded'}}
 if(p.action==='probe'){if(state&&!['ready'].includes(state.phase))fail('CHECKPOINT_GUEST_NOT_READY');idle();return {machine_identity:machine,agent_turns:0}}
 if(p.action==='artifact'){if(state&&state.phase!=='ready')fail('CHECKPOINT_GUEST_NOT_READY');idle();return {...artifact(p.path),machine_identity:machine}}
 if(p.action==='prepare_finish'){
  const lock=path.join(dir,'submission.lock');let f;try{f=fs.openSync(lock,'wx',0o600)}catch{fail('CHECKPOINT_SUBMISSION_BUSY')}
  try{idle();if(!/^[a-f0-9]{64}$/.test(p.manifest_sha256)||!Array.isArray(p.files)||p.files.length>64)fail('CHECKPOINT_MANIFEST_INVALID');if(state&&state.phase!=='ready'&&!(state.phase==='finishing'&&state.finish_manifest_sha256===p.manifest_sha256))fail('CHECKPOINT_GUEST_NOT_READY');for(const f of p.files){const a=artifact(f.path);if(a.sha256!==f.sha256||a.size!==f.size)fail('RESULT_CHANGED')}
   atomic(file,{...(state??{config:c,config_hash:hash(JSON.stringify(c)),record_id:null,closed:true}),phase:'finishing',finish_manifest_sha256:p.manifest_sha256,checkpoint_kind:'exec_only'});return {type:'finish_ready',machine_identity:machine};
  }finally{fs.closeSync(f);fs.unlinkSync(lock)}
 }
 if(p.action==='close'){
  if(!state||state.phase!=='finishing')fail('FINISH_NOT_PREPARED');
  if(!state.closed){const base=[c.acpx_bin,'--agent',[c.node,c.launcher,dir].map(quote).join(' '),'--cwd',c.workspace,'--ttl','0','--format','json','--prompt-retries','0','--deny-all','--suppress-reads'];if(c.model)base.push('--model',c.model);const r=spawnSync(base[0],[...base.slice(1),'sessions','close',p.session_id],{encoding:'utf8',timeout:30000,maxBuffer:2097152});if(r.status!==0)fail('CHECKPOINT_CLOSE_UNCONFIRMED');atomic(file,{...state,closed:true})}
  return {type:'closed',machine_identity:machine};
 }
 if(p.action==='finish_ack'){if(!state||state.phase!=='finishing'||!state.closed||state.finish_manifest_sha256!==p.manifest_sha256)fail('FINISH_RECEIPT_STATE_MISMATCH');atomic(path.join(dir,'finish.json'),{session_id:p.session_id,manifest_sha256:p.manifest_sha256,export_verified:true});return {type:'finish_acknowledged',machine_identity:machine}}
 fail('CHECKPOINT_ACTION_INVALID');
}
