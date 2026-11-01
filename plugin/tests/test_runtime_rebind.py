import json,os,pathlib,subprocess,tempfile,unittest
ROOT=pathlib.Path(__file__).resolve().parents[1]
SCRIPT=ROOT/'scripts/runtime_rebind_guest.mjs'
class RuntimeRebindTests(unittest.TestCase):
 def runfixture(self,mode):
  with tempfile.TemporaryDirectory()as td:
   code=r'''
import fs from 'node:fs';import path from 'node:path';import crypto from 'node:crypto';import{rebind,preserveBindings}from MODULE;
const home=process.env.HOME,sid='11111111-1111-4111-8111-111111111111',old='22222222-2222-4222-8222-222222222222',next='33333333-3333-4333-8333-333333333333',op='44444444-4444-4444-8444-444444444444',req='55555555-5555-4555-8555-555555555555',dir=home+'/state/'+sid;
fs.mkdirSync(dir,{recursive:true,mode:0o700});fs.mkdirSync(dir+'/operations',{mode:0o700});fs.writeFileSync(dir+'/operations/creation.lock','',{mode:0o600});if(MODE==='public-lock')fs.chmodSync(dir+'/operations/creation.lock',0o644);if(MODE==='link-lock'){fs.unlinkSync(dir+'/operations/creation.lock');fs.symlinkSync(dir+'/session.json',dir+'/operations/creation.lock')}fs.chmodSync(dir,0o700);fs.mkdirSync(home+'/.acpx/sessions',{recursive:true});
const write=(p,v)=>fs.writeFileSync(p,JSON.stringify(v),{mode:0o600}),hash=x=>crypto.createHash('sha256').update(x).digest('hex');
const c={state_dir:home+'/state',workspace:home+'/repo',node:'/usr/local/bin/node',launcher:'/opt/worker/launcher.mjs',runtime_module:'/opt/acpx/dist/runtime.js'},quote=x=>"'"+x+"'";
if(MODE.startsWith('native')){c.node=process.execPath;c.runtime_module=process.env.NOMAD_WORKERS_TEST_ACPX_PACKAGE+'/dist/runtime.js';c.launcher=home+'/fixture.mjs';fs.writeFileSync(c.launcher,`import fs from 'node:fs';import readline from 'node:readline';const dir=process.argv[2];fs.renameSync(dir+'/launch-permit.json',dir+'/launch-used.json');for await(const line of readline.createInterface({input:process.stdin})){const m=JSON.parse(line);fs.appendFileSync(dir+'/methods',m.method+'\\n');if(m.id!==undefined)console.log(JSON.stringify({jsonrpc:'2.0',id:m.id,result:m.method==='initialize'?{protocolVersion:1,agentCapabilities:{loadSession:false,sessionCapabilities:{close:{}}}}:m.method==='session/new'?{sessionId:'native-fixture-session'}:m.method==='session/prompt'?{stopReason:'end_turn'}:{}}))}`)}
c.protocol_version=1;c.acpx_version='0.21.0';c.max_artifact_bytes=1048576;c.agent_argv=[c.node,c.launcher];if(MODE.startsWith('native'))c.acpx_bin=process.env.NOMAD_WORKERS_TEST_ACPX_PACKAGE+'/dist/cli.js';fs.mkdirSync(c.workspace,{recursive:true});const command=[c.node,c.launcher,dir].map(quote).join(' ');
write(dir+'/session.json',{phase:'ready',record_id:old,config_hash:hash(JSON.stringify(c)),config:c,bootstrap_used:true});write(dir+'/turn-'+req+'.json',{request_id:req,phase:MODE==='active'?'queued':'terminal',acpx_request_id:req,prompt_sha256:hash('fixture prompt'),terminal:{status:'failed'}});
write(dir+'/cleanup.json',{phase:'session_cleanup_confirmed',cleanup_confirmed:true,session_id:sid,record_id:old,acp_session_id:'old-acp',owner_pid:MODE==='live-owner'?process.pid:2147483000});
if(MODE==='wrong-cleanup'){const p=dir+'/cleanup.json',v=JSON.parse(fs.readFileSync(p));v.record_id=next;write(p,v)}
if(MODE==='live-owner'){fs.mkdirSync(home+'/.acpx/queues',{recursive:true});write(home+'/.acpx/queues/'+hash(old).slice(0,24)+'.lock',{pid:process.pid})}
const oldPath=home+'/.acpx/sessions/'+old+'.json';write(oldPath,{schema:'acpx.session.v1',acpx_record_id:old,acp_session_id:'old-acp',name:sid,cwd:c.workspace,agent_command:command,event_log:{max_segments:5},messages:[{text:'old history'}]});const before=fs.readFileSync(oldPath,'utf8');let calls=0;
const deps={runtime:()=>{},createSession:async options=>{calls++;if(MODE==='unknown')throw Error('transport lost');if(options.resumeSessionId||options.name!==sid)throw Error('unexpected');write(home+'/.acpx/sessions/'+next+'.json',{acpx_record_id:next,acp_session_id:'new-acp',name:sid,cwd:c.workspace,agent_command:command});return {acpxRecordId:next,acpSessionId:'new-acp',name:sid,cwd:c.workspace}}};
if(MODE.startsWith('native'))delete deps.createSession;
if(MODE==='crash'||MODE==='native-flow'){let once=true;deps.afterPointer=()=>{if(once){once=false;throw Error('simulated interruption')}}}
const p={session_id:sid,operation_id:op,record_id:old,config:c,cleanup_receipt:'cleanup.json',...(MODE==='retry-pre-effect'?{lookup_only:true,retry_pre_effect:true}:{})};let a,b,error;

const flow={};const {spawnSync}=await import('node:child_process');const adapter=ADAPTER,observer=OBSERVER,newreq='77777777-7777-4777-8777-777777777777';
function submit(){const r=spawnSync(process.execPath,[adapter],{input:JSON.stringify({config:c,session_id:sid,action:'submit',request_id:newreq,prompt:'synthetic local test only'}),encoding:'utf8',timeout:15000});return {code:r.status,frames:r.stdout.trim().split('\n').filter(Boolean).map(x=>JSON.parse(x))}}
async function beforeReconcile(){flow.blocked=submit();try{await rebind({...p,operation_id:'66666666-6666-4666-8666-666666666666'},deps)}catch(e){flow.other=e.message}}
async function afterReconcile(){flow.allowed=submit();let native;for(let i=0;i<100;i++){const text=fs.readFileSync(home+'/.acpx/sessions/'+b.new_record_id+'.stream.ndjson','utf8');if(text.includes('turn_result')){native=text;break}await new Promise(r=>setTimeout(r,50))}flow.synthetic_terminal=!!native;
 const dist=path.dirname(c.runtime_module),ipc=await import('file://'+path.join(dist,fs.readdirSync(dist).find(x=>/^ipc-.*\.js$/.test(x))));const close=Object.values(ipc).find(x=>typeof x==='function'&&x.name==='tryCloseSessionOnRunningOwner');await close({sessionId:b.new_record_id,timeoutMs:3000});
 const events=[{schema:'acpx.session.journal.v1',type:'segment',record_id:old,sequence:0,message_sequence:0,request_id:null},{schema:'acpx.session.journal.v1',type:'turn_started',request_id:req},{schema:'acpx.session.journal.v1',type:'turn_result',request_id:req,result:{status:'failed',error:{code:'FIXTURE_FAILED',message:'synthetic historical failure'}}}];fs.writeFileSync(home+'/.acpx/sessions/'+old+'.stream.ndjson',events.map(x=>JSON.stringify(x)+'\n').join(''),{mode:0o600});
 const h=spawnSync(process.execPath,['--input-type=module','-e',fs.readFileSync(observer,'utf8')+'\nawait main();'],{input:JSON.stringify({config:c,session_id:sid,request_id:req,record_id:old,acpx_request_id:req,prompt_sha256:hash('fixture prompt'),action:'transcript',limit:10,max_bytes:4096})+'\n',encoding:'utf8',timeout:10000});flow.history=JSON.parse(h.stdout);
}
let otherError;try{a=await rebind(p,deps);if(MODE==='native-flow')await beforeReconcile();b=await rebind(p,deps);if(MODE==='native-flow')await afterReconcile();if(MODE==='unknown'){await rebind({...p,lookup_only:true,retry_pre_effect:true},deps);try{await rebind({...p,operation_id:'66666666-6666-4666-8666-666666666666'},deps)}catch(e){otherError=e.message}}}catch(e){error=e.message}
console.log(JSON.stringify({a,b,error,otherError,flow,calls,old_unchanged:fs.readFileSync(oldPath,'utf8')===before,methods:fs.existsSync(dir+'/methods')?fs.readFileSync(dir+'/methods','utf8'):null,state:JSON.parse(fs.readFileSync(dir+'/session.json'))}));
'''.replace('MODULE',json.dumps(SCRIPT.as_uri())).replace('MODE',json.dumps(mode)).replace('ADAPTER',json.dumps(str(ROOT/'guest/adapter.mjs'))).replace('OBSERVER',json.dumps(str(ROOT/'scripts/request_observer_guest.mjs')))
   p=subprocess.run(['node','--input-type=module','-e',code],env={**os.environ,'HOME':td},capture_output=True,text=True,timeout=35)
   self.assertEqual(p.returncode,0,p.stderr);return json.loads(p.stdout)
 @unittest.skipUnless(os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE'),'Official pinned acpx package required')
 def test_official_native_session_new_no_prompt(self):
  r=self.runfixture('native');self.assertEqual(r['a']['phase'],'ready',r);self.assertTrue(r['old_unchanged']);self.assertIn('initialize',r['methods']);self.assertIn('session/new',r['methods']);self.assertNotIn('session/prompt',r['methods']);self.assertTrue(r['b']['lookup_only'])
 @unittest.skipUnless(os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE'),'Official pinned acpx required')
 def test_real_guest_first_submit_fenced_until_pointer_reconciles(self):
  r=self.runfixture('native-flow');self.assertNotIn('error',r,r);f=r['flow'];self.assertEqual(f['blocked']['frames'][-1].get('code'),'SESSION_NOT_READY');self.assertIn(f['other'],['RECOVERY_CURRENT_BINDING_CHANGED','RECOVERY_RECORD_ALREADY_FENCED']);self.assertEqual(f['allowed']['code'],0,r);self.assertTrue(f['synthetic_terminal'],r);self.assertEqual(f['history']['terminal']['status'],'failed');self.assertTrue(r['old_unchanged'])
 def test_success_preserves_history_once(self):
  r=self.runfixture('success');self.assertEqual(r['calls'],1);self.assertTrue(r['old_unchanged']);self.assertEqual(r['a']['phase'],'ready');self.assertTrue(r['b']['lookup_only']);self.assertEqual(r['state']['record_bindings']['55555555-5555-4555-8555-555555555555'],'22222222-2222-4222-8222-222222222222');self.assertFalse(r['state']['bootstrap_used'])
 def test_unknown_never_retries(self):
  r=self.runfixture('unknown');self.assertEqual(r['calls'],1);self.assertEqual(r['a']['phase'],'rebind_unknown');self.assertEqual(r['state']['record_id'],'22222222-2222-4222-8222-222222222222');self.assertTrue(r['old_unchanged']);self.assertEqual(r['state']['phase'],'recovering');self.assertIn(r['otherError'],['RECOVERY_CURRENT_BINDING_CHANGED','RECOVERY_RECORD_ALREADY_FENCED'])
 def test_pointer_crash_reconciles_without_session_new(self):
  r=self.runfixture('crash');self.assertEqual(r['calls'],1);self.assertEqual(r['a']['phase'],'binding_pending');self.assertEqual(r['b']['phase'],'ready');self.assertTrue(r['b']['lookup_only']);self.assertTrue(r['old_unchanged'])
 def test_explicit_pre_effect_retry_keeps_single_native_dispatch(self):
  r=self.runfixture('retry-pre-effect');self.assertEqual(r['calls'],1);self.assertEqual(r['a']['phase'],'ready');self.assertTrue(r['b']['lookup_only'])
 def test_operation_creation_lock_must_be_private_regular_file(self):
  for mode in ['public-lock','link-lock']:
   r=self.runfixture(mode);self.assertEqual(r['calls'],0);self.assertTrue('RECOVERY_OPERATION_PATH_CHANGED'in r['error']or'ELOOP'in r['error'])
 def test_wrong_cleanup_record_rejected(self):
  r=self.runfixture('wrong-cleanup');self.assertEqual(r['calls'],0);self.assertEqual(r['error'],'RECOVERY_CONFIRMED_CLEANUP_REQUIRED')
 def test_live_old_owner_rejected(self):
  r=self.runfixture('live-owner');self.assertEqual(r['calls'],0);self.assertEqual(r['error'],'RECOVERY_OLD_OWNER_STILL_PRESENT')
 def test_active_work_rejected_before_effect(self):
  r=self.runfixture('active');self.assertEqual(r['calls'],0);self.assertEqual(r['error'],'RECOVERY_ACTIVE_OR_UNKNOWN_TURN')
if __name__=='__main__':unittest.main()

class CallerRebindTests(unittest.TestCase):
 def test_caller_commits_binding_and_repeat_is_lookup_only(self):
  import sys,types,contextlib
  from unittest import mock
  sys.path.insert(0,str(ROOT/'scripts'));import runtime_rebind as rb
  with tempfile.TemporaryDirectory()as td:
   root=pathlib.Path(td);s='11111111-1111-4111-8111-111111111111';old='22222222-2222-4222-8222-222222222222';new='33333333-3333-4333-8333-333333333333';op='44444444-4444-4444-8444-444444444444';req='55555555-5555-4555-8555-555555555555'
   state={'phase':'ready','record_id':old,'attempt':{'allocation':'same','started_at':'same'}};(root/'session.json').write_text(json.dumps(state));(root/'session.json').chmod(0o600);calls=[]
   a=types.SimpleNamespace(local=lambda _:root,load=lambda _:json.loads((root/'session.json').read_text()),lock=lambda _:contextlib.nullcontext(),require_initiator=lambda _:None,c={'namespace':'workers','task':'worker','allocation':'same','guest_node':'/node','guest':{}})
   def rpc(args,**kw):
    payload=json.loads(kw['input']);calls.append(payload['lookup_only']);return types.SimpleNamespace(returncode=0,stdout=json.dumps({'type':'runtime_rebind','phase':'ready','session_ready':True,'session_id':s,'operation_id':op,'old_record_id':old,'new_record_id':new,'record_bindings':{req:old},'prompt_replayed':False}))
   a.nomad=rpc
   with mock.patch.object(rb.observer,'checked',side_effect=lambda a,s:a.load(s)):
    rb.execute(a,s,op,'cleanup.json');rb.execute(a,s,op,'cleanup.json')
   self.assertEqual(calls,[False,True]);self.assertEqual(a.load(s)['record_id'],new);self.assertEqual(a.load(s)['record_bindings'][req],old)
 def test_every_unconfirmed_phase_blocks_actual_caller_submit(self):
  import sys
  from unittest import mock
  sys.path.insert(0,str(ROOT/'scripts'));import workerctl
  for phase,ready in [('not_dispatched_or_unknown',False),('ready',False),('future_unknown_phase',None),('binding_pending',False)]:
   with self.subTest(phase=phase),tempfile.TemporaryDirectory()as td:
    a=object.__new__(workerctl.Adapter);a.root=pathlib.Path(td);a.hash='pinned';a.c={};s='11111111-1111-4111-8111-111111111111';op='44444444-4444-4444-8444-444444444444';req='77777777-7777-4777-8777-777777777777'
    workerctl.atomic(a.local(s)/'session.json',{'phase':'ready','config_hash':'pinned'});workerctl.atomic(a.local(s)/('runtime-rebind-'+op+'.json'),{'phase':phase,'session_ready':ready,'operation_id':op})
    with mock.patch.object(a,'remote')as remote:
     with self.assertRaisesRegex(RuntimeError,'RUNTIME_RECOVERY_UNCONFIRMED'):a.submit(s,req,'synthetic never dispatched')
     remote.assert_not_called();self.assertFalse((a.local(s)/(req+'.json')).exists())
