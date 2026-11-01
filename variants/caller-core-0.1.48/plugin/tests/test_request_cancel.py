import contextlib,json,os,pathlib,shutil,subprocess,sys,tempfile,unittest,uuid
from types import SimpleNamespace
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import request_cancel as c
import request_transcript as observer
NODE=shutil.which('node');HELPER=pathlib.Path(__file__).parents[1]/'scripts/request_observer_guest.mjs'
IDENTITY={'machine_id':'a'*32,'boot_id':'00000000-0000-4000-8000-000000000001'}
def write(p,v):p.parent.mkdir(parents=True,exist_ok=True,mode=0o700);p.write_text(json.dumps(v));p.chmod(0o600)

class CallerTests(unittest.TestCase):
 def fixture(self,root):
  s,r=str(uuid.uuid4()),str(uuid.uuid4());folder=root/s;folder.mkdir(mode=0o700)
  state={'phase':'ready','record_id':str(uuid.uuid4()),'attempt':{'allocation':'allocation','started_at':'start'}}
  cfg={'creator_record':'native','namespace':'workers','allocation':'allocation','task':'worker','guest_node':NODE,'guest':{},'agent_instance_id':'owner','creator_session_id':'native-session','creator_generation':'generation'}
  target={'ClientStatus':'running','TaskStates':{'worker':{'State':'running'}},'Job':{'Meta':{'creator_agent_id':'owner','creator_session_id':'native-session','creator_generation':'generation','worker_session_id':s}}}
  turn={'session_id':s,'request_id':r,'phase':'watch_unknown','prompt_sha256':'b'*64,'acpx_request_id':'native-target'};write(folder/(r+'.json'),turn)
  # Unrelated unknown activity must not block observation or exact-request cancel.
  write(folder/'exec-other.json',{'execution_id':str(uuid.uuid4()),'phase':'exec_unknown'})
  calls=[];guest={'phase':'not_requested'}
  def nomad(args,**kw):
   p=json.loads(kw['input']);calls.append(p['action'])
   if p['action']=='cancel':guest['phase']='cancel_accepted'
   result={'type':'request_cancel_status'if p['action']=='cancel-status'else'request_cancel','session_id':s,'request_id':r,'record_id':state['record_id'],'acpx_request_id':'native-target','identity':IDENTITY,'phase':guest['phase'],'terminal':None}
   return SimpleNamespace(returncode=0,stdout=json.dumps(result))
  a=SimpleNamespace(c=cfg,load=lambda _:state,local=lambda _:folder,target=lambda:target,attempt=lambda _:state['attempt'],require_initiator=lambda _:None,lock=lambda _:contextlib.nullcontext(),nomad=nomad)
  return a,s,r,state,calls,guest,target
 def test_exact_cancel_ack_is_not_terminal_and_repeat_only_reads(self):
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f));before=(a.local(s)/(r+'.json')).read_bytes()
   one=c.cancel(a,s,r);two=c.cancel(a,s,r);status=c.status(a,s,r)
   self.assertEqual(one['phase'],'cancel_accepted');self.assertFalse(one['cancel_is_terminal_proof']);self.assertIsNone(one['terminal']);self.assertEqual(calls.count('cancel'),1);self.assertEqual(two['phase'],one['phase']);self.assertEqual(status['phase'],one['phase']);self.assertEqual((a.local(s)/(r+'.json')).read_bytes(),before)
 def test_lost_response_never_replays_and_status_recovers_guest_ack(self):
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f));real=a.nomad
   def lost(args,**kw):
    value=real(args,**kw)
    if json.loads(kw['input'])['action']=='cancel':raise subprocess.TimeoutExpired('native transport',15)
    return value
   a.nomad=lost
   first=c.cancel(a,s,r);self.assertTrue(first['cancel_outcome_unknown']);again=c.cancel(a,s,r);self.assertEqual(again['phase'],'cancel_accepted');self.assertEqual(calls.count('cancel'),1)
 def test_terminal_already_observed_no_cancel_dispatch(self):
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f));real=a.nomad
   def terminal(args,**kw):
    frame=real(args,**kw);v=json.loads(frame.stdout);v.update(phase='terminal_observed',terminal={'status':'completed','stopReason':'end_turn'});frame.stdout=json.dumps(v);return frame
   a.nomad=terminal;value=c.cancel(a,s,r);self.assertEqual(value['terminal']['status'],'completed');self.assertEqual(calls,['cancel-status']);self.assertFalse((a.local(s)/'request-cancellations'/(r+'.json')).exists())
 def test_invalid_terminal_does_not_claim_completion_or_dispatch(self):
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f));file=a.local(s)/(r+'.json');turn=json.loads(file.read_text());turn.update(phase='terminal',terminal={'status':'unknown'});write(file,turn)
   with self.assertRaisesRegex(RuntimeError,'TERMINAL_PROOF_INVALID'):c.cancel(a,s,r)
   self.assertEqual(calls,['cancel-status'])
 def test_owner_attempt_and_unknown_request_rejected_before_transport(self):
  for kind in ['generation','attempt','request','no-native']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as f:
    a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f))
    if kind=='generation':target['Job']['Meta']['creator_generation']='other'
    elif kind=='attempt':a.attempt=lambda _:{'different':'attempt'}
    elif kind=='request':r=str(uuid.uuid4())
    else:a.c.pop('creator_record')
    with self.assertRaises((RuntimeError,FileNotFoundError)):c.cancel(a,s,r)
    self.assertFalse(calls)
 def test_status_has_no_new_state_and_machine_change_denies_dispatch(self):
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f));c.status(a,s,r);self.assertFalse((a.local(s)/'request-cancellations').exists())
   c.cancel(a,s,r);real=a.nomad
   def changed(args,**kw):
    frame=real(args,**kw);v=json.loads(frame.stdout);v['identity']={**IDENTITY,'machine_id':'f'*32};frame.stdout=json.dumps(v);return frame
   a.nomad=changed
   with self.assertRaisesRegex(RuntimeError,'MACHINE_CHANGED'):c.cancel(a,s,r)
   self.assertEqual(calls.count('cancel'),1)

 def test_concurrent_caller_cancel_and_readonly_status_do_not_replay(self):
  import concurrent.futures,fcntl
  with tempfile.TemporaryDirectory()as f:
   a,s,r,state,calls,guest,target=self.fixture(pathlib.Path(f))
   @contextlib.contextmanager
   def locked(_):
    with (a.local(s)/'caller.lock').open('a')as lock:
     fcntl.flock(lock,fcntl.LOCK_EX);yield
   a.lock=locked
   with concurrent.futures.ThreadPoolExecutor(max_workers=2)as pool:results=list(pool.map(lambda _:c.cancel(a,s,r),range(2)))
   self.assertEqual(calls.count('cancel'),1);self.assertTrue(all(x['phase']=='cancel_accepted'for x in results))
   self.assertEqual(c.status(a,s,r)['phase'],'cancel_accepted');self.assertEqual(calls.count('cancel'),1)
 def test_cancel_unknown_cli_nonzero_and_status_cli_independent(self):
  import workerctl
  for command,phase,exitcode in [('request-cancel','cancel_unknown',3),('request-cancel','cancel_accepted',None),('request-cancel-status','cancel_unknown',None)]:
   with self.subTest(command=command,phase=phase),mock.patch.object(sys,'argv',['workerctl','--config','/fixture',command,'--session',str(uuid.uuid4()),'--request',str(uuid.uuid4())]),mock.patch.object(workerctl,'Adapter'),mock.patch.object(c,'cancel',return_value={'phase':phase}),mock.patch.object(c,'status',return_value={'phase':phase}),contextlib.redirect_stdout(__import__('io').StringIO()):
    if exitcode:
     with self.assertRaises(SystemExit)as result:workerctl.main()
     self.assertEqual(result.exception.code,exitcode)
    else:workerctl.main()

@unittest.skipUnless(NODE,'Node required')
class GuestIntentTests(unittest.TestCase):
 def test_guest_concurrent_crash_unknown_and_terminal_without_journal_scan(self):
  script=r'''
import fs from 'node:fs';import path from 'node:path';import {cancelRequest} from HELPER;
const dir=process.argv[1],id={machine_id:'a'.repeat(32),boot_id:'00000000-0000-4000-8000-000000000001'};
const p={session_id:'session',request_id:'target',record_id:'record',prompt_sha256:'b'.repeat(64),action:'cancel',expected_identity:{boot_id:id.boot_id,machine_id:id.machine_id}};
const b={dir,c:{},state:{phase:'ready',record_id:'record',config_hash:'config'},turn:{phase:'queued',acpx_request_id:'native-target'}};
let count=0;const deps={identity:()=>id,dispatch:async(c,record,target)=>{if(record!=='record'||target!=='native-target')throw Error('wrong target');count++;await new Promise(r=>setTimeout(r,30));return true}};
const both=await Promise.all([cancelRequest(p,b,deps),cancelRequest(p,b,deps)]);if(count!==1||!both.some(x=>x.phase==='cancel_accepted'))throw Error('duplicate dispatch');
const again=await cancelRequest(p,b,deps);if(count!==1||again.phase!=='cancel_accepted'||again.cancel_is_terminal_proof)throw Error('replay or false terminal');
p.action='cancel-status';b.turn.phase='terminal';b.turn.terminal={status:'cancelled'};const done=await cancelRequest(p,b,deps);if(done.phase!=='terminal_observed'||done.terminal.status!=='cancelled'||count!==1)throw Error('terminal lookup');
// A second request suffers lost response. Its durable unknown is never resent.
p.action='cancel';p.request_id='lost';b.turn={phase:'queued',acpx_request_id:'lost-native'};let lost=0;const fail={identity:()=>id,dispatch:async()=>{lost++;throw Error('synthetic disconnect')}};
const first=await cancelRequest(p,b,fail),second=await cancelRequest(p,b,fail);if(first.phase!=='cancel_unknown'||second.phase!=='cancel_unknown'||lost!==1)throw Error('unknown replay');
// Crash immediately after durable intent, before any dispatch: still lookup-only.
p.request_id='crash';b.turn.acpx_request_id='crash-native';const saved={session_id:p.session_id,request_id:p.request_id,record_id:b.state.record_id,acpx_request_id:b.turn.acpx_request_id,prompt_sha256:p.prompt_sha256,config_hash:b.state.config_hash,identity:id,phase:'cancel_unknown'};fs.writeFileSync(path.join(dir,'request-cancel-crash.json'),JSON.stringify(saved),{mode:0o600});const crash=await cancelRequest(p,b,deps);if(crash.phase!=='cancel_unknown'||count!==1)throw Error('crash replay');
console.log(JSON.stringify({count,lost,terminal:done.terminal.status,concurrent:true}));
'''.replace('HELPER',json.dumps(HELPER.as_uri()))
  with tempfile.TemporaryDirectory()as f:
   result=subprocess.run([NODE,'--input-type=module','-e',script,f],capture_output=True,text=True,timeout=10);self.assertEqual(result.returncode,0,result.stdout+result.stderr);self.assertEqual(json.loads(result.stdout)['count'],1)

@unittest.skipUnless(NODE and os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE'),'Official cached acpx package required')
class OfficialOwnerTests(unittest.TestCase):
 def test_official_owner_exact_target_not_other_active_no_launch(self):
  script=r'''
import fs from 'node:fs';import path from 'node:path';import crypto from 'node:crypto';import {pathToFileURL} from 'node:url';import {cancelRequest,nativeTargetedCancel} from HELPER;
const root=process.argv[1],pkg=process.argv[2],file=fs.readdirSync(path.join(pkg,'dist')).find(x=>/^ipc-.*\.js$/.test(x)),mod=await import(pathToFileURL(path.join(pkg,'dist',file)).href);
const Owner=Object.values(mod).find(x=>typeof x==='function'&&x.name==='SessionQueueOwner');if(!Owner)throw Error('missing official owner');
const record='11111111-1111-4111-8111-111111111111',key=crypto.createHash('sha256').update(record).digest('hex').slice(0,24),socket='/tmp/nw-cancel-'+process.pid+'-'+crypto.randomUUID().slice(0,8)+'.sock';
const now=new Date().toISOString(),lease={sessionId:record,pid:process.pid,socketPath:socket,createdAt:now,heartbeatAt:now,ownerGeneration:123,queueDepth:0,sharedRuntime:true};
const qdir=path.join(root,'.acpx','queues');fs.mkdirSync(qdir,{recursive:true,mode:0o700});fs.writeFileSync(path.join(qdir,key+'.lock'),JSON.stringify(lease),{mode:0o600});
let calls=0;const owner=await Owner.start(lease,{cancelPrompt:async()=>{calls++;return true}},{maxQueueDepth:16});
const dir=path.join(root,'session');fs.mkdirSync(dir,{mode:0o700});const identity={machine_id:'a'.repeat(32),boot_id:'00000000-0000-4000-8000-000000000001'};
const b={dir,c:{runtime_module:path.join(pkg,'dist/runtime.js')},state:{phase:'ready',record_id:record,config_hash:'config'},turn:{phase:'queued',acpx_request_id:'own-target'}};
const p={session_id:'outer-session',request_id:'outer-target',prompt_sha256:'b'.repeat(64),action:'cancel',expected_identity:{boot_id:identity.boot_id,machine_id:identity.machine_id}};const deps={identity:()=>identity,dispatch:nativeTargetedCancel};
try{
 owner.activeTask={requestId:'other-active'};
 const other=await cancelRequest(p,b,deps);if(other.phase!=='target_not_cancelled'||calls!==0||owner.activeTask.requestId!=='other-active')throw Error('cancelled other active request');
 // New exact target: native API acknowledges only that owned active request.
 p.request_id='second';b.turn.acpx_request_id='second-native';owner.activeTask={requestId:'second-native'};
 const yes=await cancelRequest(p,b,deps);if(yes.phase!=='cancel_accepted'||yes.cancel_is_terminal_proof||yes.terminal||calls!==1)throw Error('bad native cancel ack');
 await cancelRequest(p,b,deps);if(calls!==1)throw Error('duplicate native cancel');
 // Existing request IDs can be observed even with unrelated unfinished work;
 // no journals or CLI executable exist in this fixture.
 p.action='cancel-status';const observed=await cancelRequest(p,b,deps);if(observed.phase!=='cancel_accepted'||calls!==1)throw Error('status dispatched');
 // Exact queued target: remove only it, preserving a different active request.
 owner.activeTask={requestId:'other-active'};let pendingClosed=0;
 owner.pending.push({requestId:'queued-native',waitForCompletion:true,send:()=>{},close:()=>{pendingClosed++}});
 p.action='cancel';p.request_id='queued-outer';b.turn.acpx_request_id='queued-native';
 const queued=await cancelRequest(p,b,deps);if(queued.phase!=='cancel_accepted'||pendingClosed!==1||calls!==1||owner.activeTask.requestId!=='other-active')throw Error('wrong queued cancel');
 // A lost response after the real upstream accepted is not a retry permission.
 p.request_id='lost-outer';b.turn.acpx_request_id='lost-native';owner.activeTask={requestId:'lost-native'};
 const lostDeps={identity:()=>identity,dispatch:async(...args)=>{await nativeTargetedCancel(...args);throw Error('synthetic lost reply')}};
 const lost=await cancelRequest(p,b,lostDeps);await cancelRequest(p,b,lostDeps);if(lost.phase!=='cancel_unknown'||calls!==2)throw Error('native lost-response replay');
 // Missing owner is distinct from completed/cancelled and does not spawn one.
 fs.unlinkSync(path.join(qdir,key+'.lock'));p.request_id='absent';b.turn.acpx_request_id='absent-native';
 const absent=await cancelRequest(p,b,deps);if(absent.phase!=='owner_unavailable'||calls!==2)throw Error('absent owner fabricated outcome');
 console.log(JSON.stringify({official_owner:true,other_untouched:true,calls,terminal_proof:false}));
}finally{owner.activeTask=undefined;await owner.close();try{fs.unlinkSync(socket)}catch{}}
'''.replace('HELPER',json.dumps(HELPER.as_uri()))
  with tempfile.TemporaryDirectory(prefix='cancel-owner-')as f:
   result=subprocess.run([NODE,'--input-type=module','-e',script,f,os.environ['NOMAD_WORKERS_TEST_ACPX_PACKAGE']],capture_output=True,text=True,env={**os.environ,'HOME':f},timeout=10);self.assertEqual(result.returncode,0,result.stdout+result.stderr);self.assertEqual(json.loads(result.stdout)['calls'],2)

@unittest.skipUnless(NODE and os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE'),'Official cached acpx package required')
class NativeProducerConsumerTests(unittest.TestCase):
 def test_actual_caller_guest_main_and_official_owner_with_persisted_identity(self):
  import hashlib,selectors
  sys.path.insert(0,str(pathlib.Path(__file__).parent));from test_request_transcript import guest_fixture
  owner_source=r'''
import fs from 'node:fs';import path from 'node:path';import crypto from 'node:crypto';import {pathToFileURL} from 'node:url';
const p=JSON.parse(process.argv[1]),dir=path.join(p.package,'dist'),mod=await import(pathToFileURL(path.join(dir,fs.readdirSync(dir).find(x=>/^ipc-.*\.js$/.test(x)))).href),Owner=Object.values(mod).find(x=>typeof x==='function'&&x.name==='SessionQueueOwner');
const q=path.join(p.root,'.acpx','queues');fs.mkdirSync(q,{recursive:true,mode:0o700});const now=new Date().toISOString(),lease={sessionId:p.record,pid:process.pid,socketPath:p.socket,createdAt:now,heartbeatAt:now,ownerGeneration:123,queueDepth:0,sharedRuntime:true};fs.writeFileSync(path.join(q,crypto.createHash('sha256').update(p.record).digest('hex').slice(0,24)+'.lock'),JSON.stringify(lease),{mode:0o600});
let count=0;const owner=await Owner.start(lease,{cancelPrompt:async()=>{fs.writeFileSync(p.count,String(++count));return true}},{maxQueueDepth:16});owner.activeTask={requestId:p.target};console.log('ready');
process.on('SIGTERM',()=>{owner.activeTask=undefined;owner.close().finally(()=>{try{fs.unlinkSync(p.socket)}catch{}process.exit(0)})});
'''
  with tempfile.TemporaryDirectory(prefix='caller-cancel-e2e-')as folder:
   root=pathlib.Path(folder);s,r,record,cfg,guest,p=guest_fixture(root);pkg=os.environ['NOMAD_WORKERS_TEST_ACPX_PACKAGE'];cfg['runtime_module']=str(pathlib.Path(pkg)/'dist/runtime.js');config_hash=hashlib.sha256(json.dumps(cfg,separators=(',',':'),ensure_ascii=False).encode()).hexdigest();write(guest/'session.json',{'phase':'ready','record_id':record,'config_hash':config_hash})
   native=json.loads((guest/('turn-'+r+'.json')).read_text())['acpx_request_id']
   journal=[{'schema':'acpx.session.journal.v1','type':'segment','record_id':record,'sequence':0,'message_sequence':0,'request_id':None},{'schema':'acpx.session.journal.v1','type':'turn_started','request_id':native},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'fixture','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'existing visible progress'}}}},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'fixture','update':{'sessionUpdate':'tool_call','toolCallId':'fixture-tool','kind':'read','status':'in_progress'}}}]
   stream=root/'.acpx/sessions'/(record+'.stream.ndjson');stream.write_text(''.join(json.dumps(v)+'\n'for v in journal));stream.chmod(0o600)
   socket='/tmp/nw-cancel-e2e-'+str(uuid.uuid4())[:12]+'.sock';count=root/'count'
   owner=subprocess.Popen([NODE,'--input-type=module','-e',owner_source,json.dumps({'root':str(root),'package':pkg,'record':record,'target':native,'socket':socket,'count':str(count)})],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env={**os.environ,'HOME':str(root)})
   try:
    with selectors.DefaultSelector()as ready:ready.register(owner.stdout,selectors.EVENT_READ);self.assertTrue(ready.select(5),'synthetic owner readiness timeout')
    self.assertEqual(owner.stdout.readline().strip(),'ready')
    local=root/'caller'/s;local.mkdir(parents=True,mode=0o700);turn=json.loads((guest/('turn-'+r+'.json')).read_text());write(local/(r+'.json'),{**turn,'session_id':s,'phase':'watch_unknown'})
    state={'phase':'ready','record_id':record,'attempt':{'allocation':'allocation','started_at':'fixture'}}
    config={'creator_record':'native','namespace':'workers','allocation':'allocation','task':'worker','guest_node':NODE,'guest':cfg,'agent_instance_id':'agent','creator_session_id':'native-session','creator_generation':'generation'}
    target={'ClientStatus':'running','TaskStates':{'worker':{'State':'running'}},'Job':{'Meta':{'creator_agent_id':'agent','creator_session_id':'native-session','creator_generation':'generation','worker_session_id':s}}}
    commands=[]
    # Test-only host identity fixture; production helper accepts no override.
    patch="import fsNative from 'node:fs';const nativeRead=fsNative.readFileSync;fsNative.readFileSync=(file,...rest)=>file==='/etc/machine-id'?'"+IDENTITY['machine_id']+"':file==='/proc/sys/kernel/random/boot_id'?'"+IDENTITY['boot_id']+"':nativeRead(file,...rest);\n"
    def execute(argv,**kw):
     commands.append(json.loads(kw['input'])['action']);source=patch+HELPER.read_text()+'\nawait main();\n'
     return subprocess.run([NODE,'--input-type=module','-e',source],capture_output=True,env={**os.environ,'HOME':str(root)},**kw)
    a=SimpleNamespace(c=config,load=lambda _:state,local=lambda _:local,target=lambda:target,attempt=lambda _:state['attempt'],require_initiator=lambda _:None,lock=lambda _:contextlib.nullcontext(),nomad=execute)
    registry=root/'.acpx/sessions'/(record+'.json');registry_bytes=registry.read_bytes();registry.unlink()
    first=c.cancel(a,s,r);self.assertEqual(first['phase'],'cancel_accepted');self.assertFalse(first['cancel_is_terminal_proof']);self.assertEqual(count.read_text(),'1')
    repeated=c.cancel(a,s,r);status=c.status(a,s,r);self.assertEqual(repeated['phase'],'cancel_accepted');self.assertEqual(status['phase'],'cancel_accepted');self.assertEqual(commands.count('cancel'),1);self.assertEqual(count.read_text(),'1')
    registry.write_bytes(registry_bytes);registry.chmod(0o600)
    write(local/'exec-unknown.json',{'execution_id':str(uuid.uuid4()),'phase':'exec_unknown'})
    history=observer.read(a,s,r);self.assertEqual(history['entries'][0]['text'],'existing visible progress');self.assertIsNone(history['terminal']);self.assertEqual(commands.count('cancel'),1)
    # Independently observed terminal closes the story; ACK never did.
    turn.update(phase='terminal',terminal={'status':'cancelled'});write(guest/('turn-'+r+'.json'),turn)
    terminal=c.status(a,s,r);self.assertEqual(terminal['phase'],'terminal_observed');self.assertEqual(terminal['terminal']['status'],'cancelled');self.assertEqual(count.read_text(),'1')
   finally:
    if owner.poll()is None:owner.terminate()
    try:owner.wait(timeout=3)
    except subprocess.TimeoutExpired:owner.kill();owner.wait(timeout=2)
    owner.stdout.close();owner.stderr.close()
    try:os.unlink(socket)
    except FileNotFoundError:pass
