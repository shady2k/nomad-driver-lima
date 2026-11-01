import copy,hashlib,json,os,pathlib,subprocess,sys,tempfile,unittest,uuid
from unittest import mock
from types import SimpleNamespace
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl,request_transcript,activity
from test_request_transcript import guest_fixture,write_private,NODE,HELPER
import test_request_transcript as base_fixture

@unittest.skipUnless(NODE,'Node required')
class LegacyTurnBindingTests(unittest.TestCase):
 def observe(self,root,payload):
  process=subprocess.Popen([NODE,'--input-type=module','-e',request_transcript.program()],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env={**os.environ,'HOME':str(root)})
  try:
   process.stdin.write(json.dumps({**payload,'action':'watch'})+'\n');process.stdin.flush();line=process.stdout.readline();process.stdin.close();process.stdin=None;process.wait(timeout=5)
   return process.returncode,json.loads(line)if line else None
  finally:
   if process.poll()is None:process.kill();process.wait(timeout=2)
   if process.stdin:process.stdin.close()
   process.stdout.close();process.stderr.close()
 def terminal(self,root,payload,state):
  path=state/('turn-'+payload['request_id']+'.json');turn=json.loads(path.read_text());turn.update(phase='terminal',cursor='retained-native-cursor',terminal={'status':'completed','stopReason':'end_turn'});write_private(path,turn);return path
 def test_first_and_second_legacy_native_ids_remain_opaque_and_reads_do_not_write(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root)
   for native in [str(uuid.uuid4()),str(uuid.uuid4()),r]:
    turn=state/('turn-'+r+'.json');v=json.loads(turn.read_text());v['acpx_request_id']=native;write_private(turn,v);p['acpx_request_id']=native;self.terminal(root,p,state);before=turn.read_bytes()
    for _ in range(2):
     code,row=self.observe(root,p);self.assertEqual(code,0,row);self.assertEqual(row['event']['requestId'],native);self.assertEqual(row['event']['type'],'turn_result')
    self.assertEqual(turn.read_bytes(),before)
 def test_legacy_context_and_mapping_conflicts_fail_closed(self):
  for kind in ['outer','native','prompt','session','config','record','schema','missing','intent','explicit-session','null-session']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as tmp:
    root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root);path=self.terminal(root,p,state);v=json.loads(path.read_text())
    if kind=='outer':v['request_id']=str(uuid.uuid4())
    if kind=='native':p['acpx_request_id']='different-native'
    if kind=='prompt':p['prompt_sha256']='a'*64
    if kind=='session':p['session_id']=str(uuid.uuid4())
    if kind=='config':p['config']={**c,'workspace':str(root/'other')}
    if kind=='record':p['record_id']=str(uuid.uuid4())
    if kind=='schema':v['unrecognized_binding']='unsupported'
    if kind=='missing':path.unlink()
    if kind=='intent':v['phase']='intent';v.pop('acpx_request_id')
    if kind=='explicit-session':v['session_id']=str(uuid.uuid4())
    if kind=='null-session':v['session_id']=None
    if kind!='missing':write_private(path,v)
    before=path.read_bytes()if path.exists()else None;code,row=self.observe(root,p);self.assertNotEqual(code,0);self.assertEqual(row['type'],'error');self.assertFalse(row['prompt_retry']);self.assertEqual(path.read_bytes()if path.exists()else None,before)
 def test_lost_submit_ack_can_observe_unique_durable_mapping_without_guess_or_replay(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root);path=self.terminal(root,p,state);p['acpx_request_id']=None;before=path.read_bytes();code,row=self.observe(root,p);self.assertEqual(code,0,row);self.assertEqual(row['event']['requestId'],'queue-request');self.assertEqual(path.read_bytes(),before)

@unittest.skipUnless(NODE,'Node required')
class ActualProducerContractTests(unittest.TestCase):
 observe=LegacyTurnBindingTests.observe
 terminal=LegacyTurnBindingTests.terminal
 def test_actual_deployed_adapter_first_and_second_turn_schema(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root);native_first=str(uuid.uuid4());second=str(uuid.uuid4());c.update(agent_argv=['/bin/true'],max_artifact_bytes=4096)
   runtime=root/'runtime.mjs';runtime.write_text('export function createSharedAcpRuntime(){return {startTurn(o){return {promptStarted:Promise.resolve()}},async shutdown(){}}}\n');c['runtime_module']=str(runtime)
   write_private(state/'session.json',{'phase':'ready','record_id':record,'config_hash':hashlib.sha256(json.dumps(c,separators=(',',':')).encode()).hexdigest(),'bootstrap_used':False})
   script='#!'+sys.executable+'\nimport sys,json\na=sys.argv[1:]\nif a==["--version"]:print("0.21.0")\nelif "status" in a:print(json.dumps({"status":"alive"}))\nelif "prompt" in a:print(json.dumps({"action":"prompt_queued","requestId":'+repr(native_first)+'}))\nelif "watch" in a:\n for q in '+repr([native_first,second])+': print(json.dumps({"type":"turn_result","requestId":q,"cursor":"native-cursor-"+q,"result":{"status":"completed","stopReason":"end_turn"}}),flush=True)\nelse:raise SystemExit(44)\n';pathlib.Path(c['acpx_bin']).write_text(script);pathlib.Path(c['acpx_bin']).chmod(0o700)
   adapter=pathlib.Path(__file__).parents[1]/'guest/adapter.mjs'
   (state/('turn-'+r+'.json')).unlink()
   for outer,expected in [(r,native_first),(second,second)]:
    prompt='synthetic turn '+outer;run=subprocess.run([NODE,str(adapter)],input=json.dumps({'config':c,'session_id':s,'request_id':outer,'action':'submit','prompt':prompt}),capture_output=True,text=True,timeout=8);self.assertEqual(run.returncode,0,run.stdout+run.stderr);ack=json.loads(run.stdout.splitlines()[-1]);turn=state/('turn-'+outer+'.json');v=json.loads(turn.read_text());self.assertNotIn('session_id',v);self.assertEqual(v['acpx_request_id'],expected);request_transcript.validate_submit_ack(ack,s,outer,hashlib.sha256(prompt.encode()).hexdigest())
    payload={'config':c,'session_id':s,'request_id':outer,'record_id':record,'acpx_request_id':expected,'prompt_sha256':v['prompt_sha256']};self.assertEqual(v['phase'],'queued');code,row=self.observe(root,payload);self.assertEqual(code,0,row);self.assertEqual(row['event']['requestId'],expected);after=json.loads(turn.read_text());self.assertEqual(after['phase'],'terminal');self.assertEqual(after['terminal']['status'],'completed');self.assertEqual({k:after[k] for k in v if k!='phase'},{k:v[k] for k in v if k!='phase'});before=turn.read_bytes();self.assertEqual(self.observe(root,payload)[0],0);self.assertEqual(turn.read_bytes(),before)

class CallerBindingTests(unittest.TestCase):
 def adapter(self,root):
  s=str(uuid.uuid4());c={'version':1,'allocation':str(uuid.uuid4()),'owner_id':str(uuid.uuid4()),'receipt_root':str(root/'receipts'),'namespace':'workers','task':'worker','job':'owned'};p=root/'config.json';p.write_text(json.dumps(c));a=workerctl.Adapter(p);row={'ID':c['allocation'],'ClientStatus':'running','Job':{'Version':1},'TaskStates':{'worker':{'State':'running','StartedAt':'original','Restarts':0}}};a.target=lambda:row;workerctl.atomic(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row),'record_id':str(uuid.uuid4())});return a,s
 def test_submit_ack_validation_first_second_and_no_native_id_assumption(self):
  for native_mode in ['distinct','same']:
   with tempfile.TemporaryDirectory()as tmp:
    a,s=self.adapter(pathlib.Path(tmp));q=str(uuid.uuid4());native=str(uuid.uuid4())if native_mode=='distinct'else q;calls=[]
    def remote(session,action,**kw):
     calls.append(action);yield {'type':'submitted','request_id':q,'phase':'queued','prompt_sha256':workerctl.digest(kw['prompt'].encode()),'acpx_request_id':native}
    a.remote=remote;row=a.submit(s,q,'fixture');self.assertEqual(row['acpx_request_id'],native);self.assertEqual(a.submit(s,q,'fixture'),row);self.assertEqual(calls,['submit'])
 def test_bad_or_lost_ack_persists_unknown_and_repeat_never_dispatches(self):
  for kind in ['lost','outer','prompt','session','native','type']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as tmp:
    a,s=self.adapter(pathlib.Path(tmp));q=str(uuid.uuid4());calls=[]
    def remote(session,action,**kw):
     calls.append(action)
     if kind=='lost':raise RuntimeError('transport unknown')
     frame={'type':'submitted','request_id':q,'phase':'queued','prompt_sha256':workerctl.digest(kw['prompt'].encode()),'acpx_request_id':str(uuid.uuid4())}
     if kind=='outer':frame['request_id']=str(uuid.uuid4())
     if kind=='prompt':frame['prompt_sha256']='f'*64
     if kind=='session':frame['session_id']=str(uuid.uuid4())
     if kind=='native':frame.pop('acpx_request_id')
     if kind=='type':frame['type']='other'
     yield frame
    a.remote=remote
    with self.assertRaises(RuntimeError):a.submit(s,q,'fixture')
    row=a.submit(s,q,'fixture');self.assertEqual(row['phase'],'submit_unknown');self.assertEqual(row['request_id'],q);self.assertNotIn('acpx_request_id',row);self.assertEqual(calls,['submit'])
    with self.assertRaisesRegex(RuntimeError,'UNFINISHED_OR_UNKNOWN'):activity.pending(a,s)
 def test_read_missing_or_conflicting_local_receipt_fails_before_transport(self):
  for kind in ['hash','outer','session','native']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as tmp:
    root=pathlib.Path(tmp);a,s,q,turn,calls,target=base_fixture.CallerTests().fixture(root)
    if kind=='hash':turn.pop('prompt_sha256')
    if kind=='outer':turn['request_id']=str(uuid.uuid4())
    if kind=='session':turn['session_id']=str(uuid.uuid4())
    if kind=='native':turn['acpx_request_id']=False
    write_private(root/s/(q+'.json'),turn)
    with self.assertRaises(RuntimeError):request_transcript.read(a,s,q)
    self.assertFalse(calls)
 def test_read_only_terminal_summary_does_not_clear_unknown_gate(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);a,s,q,turn,calls,target=base_fixture.CallerTests().fixture(root);turn['phase']='watch_unknown';write_private(root/s/(q+'.json'),turn);before=(root/s/(q+'.json')).read_bytes();original=a.nomad
   def reply(*args,**kw):
    result=original(*args,**kw);v=json.loads(result.stdout);v['terminal']={'status':'completed','stop_reason':'end_turn'};return SimpleNamespace(returncode=0,stdout=json.dumps(v))
   a.nomad=reply;self.assertEqual(request_transcript.read(a,s,q)['terminal']['status'],'completed');self.assertEqual((root/s/(q+'.json')).read_bytes(),before)
   with self.assertRaisesRegex(RuntimeError,'UNFINISHED_OR_UNKNOWN'):activity.pending(a,s)
 def test_verified_watch_terminal_alone_clears_unknown_gate(self):
  with tempfile.TemporaryDirectory()as tmp:
   a,s=self.adapter(pathlib.Path(tmp));q=str(uuid.uuid4());native=str(uuid.uuid4());p=a.local(s)/(q+'.json');workerctl.atomic(p,{'session_id':s,'request_id':q,'phase':'watch_unknown','acpx_request_id':native,'cursor':None})
   with self.assertRaisesRegex(RuntimeError,'UNFINISHED_OR_UNKNOWN'):activity.pending(a,s)
   a.remote=lambda *args,**kw:(x for x in [{'type':'watch','request_id':q,'event':{'requestId':native,'cursor':'verified-native-cursor','type':'turn_result','result':{'status':'completed','stopReason':'end_turn'}}}])
   self.assertEqual(list(a.watch(s,q))[-1]['event']['type'],'turn_result');row=json.loads(p.read_text());self.assertEqual(row['phase'],'terminal');self.assertEqual(row['acpx_request_id'],native);self.assertEqual(row['cursor'],'verified-native-cursor');activity.pending(a,s)

 def test_watch_wrong_native_frame_never_clears_unknown_gate(self):
  with tempfile.TemporaryDirectory()as tmp:
   a,s=self.adapter(pathlib.Path(tmp));q=str(uuid.uuid4());native=str(uuid.uuid4());p=a.local(s)/(q+'.json');workerctl.atomic(p,{'session_id':s,'request_id':q,'phase':'queued','acpx_request_id':native,'cursor':None})
   a.remote=lambda *args,**kw:(x for x in [{'type':'watch','request_id':q,'event':{'requestId':str(uuid.uuid4()),'cursor':'untrusted','type':'turn_result','result':{'status':'completed'}}}])
   with self.assertRaisesRegex(RuntimeError,'WATCH_REQUEST_BINDING_CHANGED'):list(a.watch(s,q))
   row=json.loads(p.read_text());self.assertEqual(row['phase'],'watch_unknown');self.assertEqual(row['acpx_request_id'],native);self.assertIsNone(row['cursor'])
   with self.assertRaisesRegex(RuntimeError,'UNFINISHED_OR_UNKNOWN'):activity.pending(a,s)
