import base64,copy,hashlib,io,json,os,pathlib,shutil,subprocess,sys,tempfile,time,unittest,uuid
from types import SimpleNamespace
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import request_transcript as transcript
import workerctl
NODE=shutil.which('node')
HELPER=pathlib.Path(__file__).parents[1]/'scripts/request_observer_guest.mjs'

def write_private(path,value):
 path.parent.mkdir(parents=True,exist_ok=True,mode=0o700);path.write_text(json.dumps(value));path.chmod(0o600)

def guest_fixture(root):
 s,r,record=map(str,[uuid.uuid4(),uuid.uuid4(),uuid.uuid4()]);state=root/'state'/s;state.mkdir(parents=True,mode=0o700)
 c={'protocol_version':1,'state_dir':str(root/'state'),'workspace':str(root/'work'),'node':NODE or '/node','runtime_module':str(root/'package/dist/runtime.js'),'acpx_bin':str(root/'acpx'),'launcher':str(root/'launcher'),'acpx_version':'0.21.0'}
 config_hash=hashlib.sha256(json.dumps(c,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()
 write_private(state/'session.json',{'phase':'ready','record_id':record,'config_hash':config_hash})
 prompt_hash=hashlib.sha256(b'must not be replayed').hexdigest()
 write_private(state/('turn-'+r+'.json'),{'request_id':r,'acpx_request_id':'queue-request','phase':'queued','prompt_sha256':prompt_hash})
 write_private(root/'.acpx/sessions'/(record+'.json'),{'schema':'acpx.session.v1','acpx_record_id':record,'name':s,'event_log':{'max_segments':5}})
 payload={'config':c,'session_id':s,'request_id':r,'record_id':record,'acpx_request_id':'queue-request','prompt_sha256':prompt_hash}
 return s,r,record,c,state,payload

@unittest.skipUnless(NODE,'Node required for actual ephemeral observer tests')
class ProjectionTests(unittest.TestCase):
 def project(self,events,limit=1,cursor=None):
  source='import {project,paginate} from '+json.dumps(HELPER.as_uri())+';let raw="";for await(const b of process.stdin)raw+=b;const p=JSON.parse(raw);const v=project(p.events,"request");console.log(JSON.stringify(paginate(v,{session_id:"session",request_id:"request",record_id:"record"},p.cursor,p.limit,4096)));'
  result=subprocess.run([NODE,'--input-type=module','-e',source],input=json.dumps({'events':events,'limit':limit,'cursor':cursor}),capture_output=True,text=True,timeout=10)
  if result.returncode:raise RuntimeError(result.stderr)
  return json.loads(result.stdout)
 def fixture(self):
  events=[{'type':'turn_started','sequence':1,'requestId':'request'}]
  for seq,text in [(2,'{"Author'),(3,'ization":"Bearer synthetic-transcript-secret"}\nanswer')]:
   events.append({'type':'message','sequence':seq,'requestId':'request','message':{'method':'session/update','params':{'update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':text}}}}})
  events.extend([{'type':'message','sequence':4,'requestId':'request','message':{'method':'session/update','params':{'update':{'sessionUpdate':'tool_call','toolCallId':'call-a','kind':'execute','status':'in_progress','rawInput':{'API_KEY':'must-not-render'}}}}},{'type':'turn_result','sequence':5,'requestId':'request','result':{'status':'completed','stopReason':'end_turn','_meta':{'API_KEY':'must-not-render'}}}])
  return events
 def test_page_is_deterministic_and_redacts_before_chunk_pagination(self):
  events=self.fixture();first=self.project(events);again=self.project(events)
  self.assertEqual(first,again);self.assertNotIn('synthetic-transcript-secret',json.dumps(first));self.assertIn('[REDACTED]',first['entries'][0]['text'])
  second=self.project(events,cursor=first['cursor']);self.assertEqual(second['entries'][0]['kind'],'tool_start');self.assertFalse(second['has_more'])
  self.assertTrue(second['history_complete']);self.assertEqual(second['terminal'],{'status':'completed','stop_reason':'end_turn'});self.assertNotIn('must-not-render',json.dumps(second))
 def test_escaped_quoted_credential_text_fails_closed_before_pagination(self):
  events=self.fixture();events[1]['message']['params']['update']['content']['text']=r'{\"Authorization\":\"Bearer synthetic-escaped-secret\"}'
  events[2]['message']['params']['update']['content']['text']=''
  row=self.project(events)
  self.assertNotIn('synthetic-escaped-secret',json.dumps(row));self.assertIn('REDACTED_ESCAPED',row['entries'][0]['text'])
 def test_unclosed_credentials_redacted_at_terminal_and_tool_boundaries(self):
  for key in ['Authorization','Proxy-Authorization','API_KEY','LLM_API_KEY','VAULT_TOKEN','PASSWORD']:
   for quote in ['"',"'"]:
    for boundary in ['terminal','tool']:
     with self.subTest(key=key,quote=quote,boundary=boundary):
      events=self.fixture();value=quote+key+quote+':'+quote+'Bearer synthetic-unclosed-secret'
      cut=len(value)//2
      events[1]['message']['params']['update']['content']['text']=value[:cut]
      events[2]['message']['params']['update']['content']['text']=value[cut:]
      if boundary=='terminal':events.pop(3)
      result=self.project(events,limit=100)
      self.assertNotIn('synthetic-unclosed-secret',json.dumps(result))
      self.assertIn('[REDACTED]',result['entries'][0]['text'])
      # Projection/redaction is safe even if a consumer filters the result twice.
      script='import {redact} from '+json.dumps(HELPER.as_uri())+';let s="";for await(const b of process.stdin)s+=b;console.log(JSON.stringify(redact(redact(JSON.parse(s)))));'
      twice=subprocess.run([NODE,'--input-type=module','-e',script],input=json.dumps(value),text=True,capture_output=True,timeout=10)
      self.assertEqual(twice.returncode,0,twice.stderr);self.assertNotIn('synthetic-unclosed-secret',twice.stdout)
 def test_open_text_is_not_released_until_logical_boundary(self):
  events=self.fixture()[:3];pending=self.project(events);self.assertTrue(pending['tail_buffered']);self.assertFalse(pending['entries'])
  final=self.project(self.fixture(),cursor=pending['cursor']);self.assertTrue(final['entries']);self.assertNotIn('synthetic-transcript-secret',json.dumps(final))
 def test_cursor_rejects_other_request_or_changed_retained_prefix(self):
  first=self.project(self.fixture());raw=json.loads(base64.urlsafe_b64decode(first['cursor']+'='*(-len(first['cursor'])%4)));raw['request_id']='another'
  cursor=base64.urlsafe_b64encode(json.dumps(raw).encode()).decode().rstrip('=')
  with self.assertRaises(RuntimeError):self.project(self.fixture(),cursor=cursor)
  changed=self.fixture();changed[2]['message']['params']['update']['content']['text']='changed response'
  with self.assertRaises(RuntimeError):self.project(changed,cursor=first['cursor'])

class CallerTests(unittest.TestCase):
 def fixture(self,root):
  s,r,record=map(str,[uuid.uuid4(),uuid.uuid4(),uuid.uuid4()]);folder=root/s;folder.mkdir(mode=0o700)
  turn={'session_id':s,'request_id':r,'phase':'queued','cursor':'independent-live-cursor','acpx_request_id':'queue-request','prompt_sha256':hashlib.sha256(b'fixture').hexdigest()}
  write_private(folder/(r+'.json'),turn)
  attempt={'allocation':'allocation','started_at':'original-start'}
  c={'namespace':'workers','allocation':'allocation','task':'worker','guest_node':'/node','guest':{},'creator_record':'native','agent_instance_id':'agent','creator_session_id':'native-session','creator_generation':'generation'}
  state={'phase':'ready','record_id':record,'attempt':attempt}
  target={'ClientStatus':'running','TaskStates':{'worker':{'State':'running'}},'Job':{'Meta':{'creator_agent_id':'agent','creator_session_id':'native-session','creator_generation':'generation','worker_session_id':s}}}
  calls=[]
  a=SimpleNamespace(c=c,load=lambda _:state,local=lambda _:folder,require_initiator=lambda _:None,target=lambda:target,attempt=lambda _:attempt)
  response={'type':'request_transcript','session_id':s,'request_id':r,'record_id':record,'acpx_request_id':'queue-request','entries':[],'cursor':'transcript-cursor','read_only':True,'prompt_sha256':turn['prompt_sha256']}
  a.nomad=lambda args,**kw:(calls.append((args,kw))or SimpleNamespace(returncode=0,stdout=json.dumps(response)))
  return a,s,r,turn,calls,target
 def test_read_twice_never_advances_live_cursor_or_records_activity(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);a,s,r,turn,calls,target=self.fixture(root);path=root/s/(r+'.json');before=path.read_bytes()
   first=transcript.read(a,s,r);second=transcript.read(a,s,r,cursor=first['cursor'])
   self.assertEqual(path.read_bytes(),before);self.assertEqual(json.loads(path.read_text()),turn);self.assertEqual(len(calls),2)
   self.assertEqual(json.loads(calls[1][1]['input'])['cursor'],'transcript-cursor');self.assertNotIn('prompt',json.loads(calls[0][1]['input']))
 def test_wrong_owner_generation_and_attempt_fail_before_native_exec(self):
  for kind in ('generation','attempt'):
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as tmp:
    a,s,r,turn,calls,target=self.fixture(pathlib.Path(tmp))
    if kind=='generation':target['Job']['Meta']['creator_generation']='wrong'
    else:a.attempt=lambda _: {'allocation':'allocation','started_at':'different-start'}
    with self.assertRaisesRegex(RuntimeError,'OWNER_GENERATION|ATTEMPT_CHANGED'):transcript.read(a,s,r)
    self.assertFalse(calls)
 def test_observer_transport_eof_without_terminal_is_unknown_not_success(self):
  with tempfile.TemporaryDirectory()as tmp:
   a,s,r,turn,calls,target=self.fixture(pathlib.Path(tmp))
   process=SimpleNamespace(stdin=io.StringIO(),stdout=io.StringIO(''),poll=lambda:0,wait=lambda **kw:0)
   with mock.patch.object(transcript.subprocess,'Popen',return_value=process):
    with self.assertRaisesRegex(RuntimeError,'WATCH_OBSERVER_CLOSED_OUTCOME_UNKNOWN'):list(transcript.watch(a,s,r))
   self.assertTrue(process.stdin.closed);self.assertTrue(process.stdout.closed)

 def test_terminal_watch_returns_cached_frame_and_next_action_without_remote(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r=str(uuid.uuid4()),str(uuid.uuid4());cfg={'version':1,'allocation':str(uuid.uuid4()),'owner_id':str(uuid.uuid4()),'receipt_root':str(root/'receipts')};p=root/'config';p.write_text(json.dumps(cfg));a=workerctl.Adapter(p)
   write_private(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready'})
   write_private(a.local(s)/(r+'.json'),{'phase':'terminal','cursor':'final-live-cursor','acpx_request_id':'queue-request','terminal':{'status':'completed','stopReason':'end_turn'}})
   a.remote=lambda *args,**kw:(_ for _ in ()).throw(AssertionError('terminal watch must not open another observer'))
   row=list(a.watch(s,r))[0];self.assertTrue(row['cached_local_terminal']);self.assertEqual(row['event']['type'],'turn_result');self.assertIn('request-transcript',row['next_action'])
   write_private(a.local(s)/(r+'.json'),{'phase':'terminal','terminal':{'status':'failed','error':{'message':'Authorization: Bearer synthetic-hidden-error'}}})
   self.assertNotIn('synthetic-hidden-error',json.dumps(list(a.watch(s,r))))

@unittest.skipUnless(NODE and os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE'),'Explicit cached official acpx package required; never downloads')
class OfficialJournalTests(unittest.TestCase):
 def test_legacy_first_and_second_native_mapping_with_guest_queued_journal_terminal(self):
  for kind in ['first-distinct-native','second-equal-native']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory()as tmp:
    root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root);native=str(uuid.uuid4())if kind=='first-distinct-native'else r
    c['runtime_module']=str(pathlib.Path(os.environ['NOMAD_WORKERS_TEST_ACPX_PACKAGE'])/'dist/runtime.js');write_private(state/'session.json',{'phase':'ready','record_id':record,'config_hash':hashlib.sha256(json.dumps(c,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()})
    turn=state/('turn-'+r+'.json');row=json.loads(turn.read_text());row['acpx_request_id']=native;write_private(turn,row);p.update(config=c,acpx_request_id=native)
    journal=[{'schema':'acpx.session.journal.v1','type':'segment','record_id':record,'sequence':0,'message_sequence':0,'request_id':None},{'schema':'acpx.session.journal.v1','type':'turn_started','request_id':native},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'synthetic','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'synthetic retained response'}}}},{'schema':'acpx.session.journal.v1','type':'turn_result','request_id':native,'result':{'status':'completed','stopReason':'end_turn'}}]
    stream=root/'.acpx/sessions'/(record+'.stream.ndjson');stream.write_text(''.join(json.dumps(x)+'\n'for x in journal));stream.chmod(0o600);before={str(x.relative_to(root)):x.read_bytes()for x in root.rglob('*')if x.is_file()}
    for _ in range(2):
     result=subprocess.run([NODE,'--input-type=module','-e',transcript.program()],input=json.dumps({**p,'action':'transcript'})+'\n',capture_output=True,text=True,env={**os.environ,'HOME':str(root)},timeout=10);self.assertEqual(result.returncode,0,result.stdout+result.stderr);value=json.loads(result.stdout);self.assertEqual(value['acpx_request_id'],native);self.assertTrue(value['history_complete']);self.assertEqual(value['terminal_source'],'acpx_journal')
    self.assertEqual(before,{str(x.relative_to(root)):x.read_bytes()for x in root.rglob('*')if x.is_file()});self.assertEqual(json.loads(turn.read_text())['phase'],'queued')

 def test_real_official_reader_pages_existing_journal_without_any_write_or_cli(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root)
   package=pathlib.Path(os.environ['NOMAD_WORKERS_TEST_ACPX_PACKAGE'])
   c['runtime_module']=str(package/'dist/runtime.js')
   write_private(state/'session.json',{'phase':'ready','record_id':record,'config_hash':hashlib.sha256(json.dumps(c,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()})
   sessions=root/'.acpx/sessions';sessions.mkdir(parents=True,mode=0o700,exist_ok=True)
   write_private(sessions/(record+'.json'),{'schema':'acpx.session.v1','acpx_record_id':record,'name':s,'event_log':{'max_segments':5}})
   journal=[{'schema':'acpx.session.journal.v1','type':'segment','record_id':record,'sequence':0,'message_sequence':0,'request_id':None},{'schema':'acpx.session.journal.v1','type':'turn_started','request_id':'queue-request'}]
   journal.extend([{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'acp-fixture','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'retained worker response'}}}},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'acp-fixture','update':{'sessionUpdate':'tool_call','toolCallId':'call-a','kind':'read','status':'in_progress'}}},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'acp-fixture','update':{'sessionUpdate':'tool_call_update','toolCallId':'call-a','status':'completed'}}},{'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'acp-fixture','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'final response'}}}},{'schema':'acpx.session.journal.v1','type':'turn_result','request_id':'queue-request','result':{'status':'completed','stopReason':'end_turn'}}])
   stream=sessions/(record+'.stream.ndjson');stream.write_text(''.join(json.dumps(x)+'\n'for x in journal));stream.chmod(0o600)
   before={str(f.relative_to(root)):f.read_bytes()for f in root.rglob('*')if f.is_file()}
   env={**os.environ,'HOME':str(root)}
   def read(cursor=None):
    payload={**p,'action':'transcript','limit':1,'max_bytes':4096,'cursor':cursor}
    result=subprocess.run([NODE,'--input-type=module','-e',transcript.program()],input=json.dumps(payload)+'\n',capture_output=True,text=True,env=env,timeout=10)
    self.assertEqual(result.returncode,0,result.stdout+result.stderr);return json.loads(result.stdout)
   first=read();again=read();self.assertEqual(first,again);self.assertEqual(first['entries'][0]['text'],'retained worker response');self.assertTrue(first['history_complete'])
   self.assertTrue(first['has_more']);second=read(first['cursor']);third=read(second['cursor']);last=read(third['cursor'])
   self.assertEqual(second['entries'][0]['kind'],'tool_start');self.assertEqual(third['entries'][0]['kind'],'tool_end');self.assertEqual(last['entries'][0]['text'],'final response');self.assertFalse(last['has_more'])
   self.assertEqual(before,{str(f.relative_to(root)):f.read_bytes()for f in root.rglob('*')if f.is_file()})
   original=stream.read_bytes();stream.write_bytes(b'x'*8388609)
   result=subprocess.run([NODE,'--input-type=module','-e',transcript.program()],input=json.dumps({**p,'action':'transcript'})+'\n',capture_output=True,text=True,env=env,timeout=10)
   self.assertNotEqual(result.returncode,0);self.assertEqual(json.loads(result.stdout)['code'],'TRANSCRIPT_SCAN_BOUND_EXCEEDED')
   stream.write_bytes(original)
   session_file=state/'session.json';session_file.unlink();session_file.symlink_to(sessions/(record+'.json'))
   result=subprocess.run([NODE,'--input-type=module','-e',transcript.program()],input=json.dumps({**p,'action':'transcript'})+'\n',capture_output=True,text=True,env=env,timeout=10)
   self.assertNotEqual(result.returncode,0);self.assertEqual(json.loads(result.stdout)['type'],'error')


@unittest.skipUnless(NODE,'Node required')
class DisconnectTests(unittest.TestCase):
 def test_eof_stops_only_own_quiet_watch_child_not_primary(self):
  with tempfile.TemporaryDirectory()as tmp:
   root=pathlib.Path(tmp);s,r,record,c,state,p=guest_fixture(root);trace=root/'watch.pid';ended=root/'watch.ended'
   script='#!'+sys.executable+'\nimport os,sys,json,time,signal,pathlib\nif sys.argv[1:]==["--version"]:print("0.21.0");sys.exit(0)\npathlib.Path('+repr(str(trace))+').write_text(str(os.getpid()))\ndef stop(*args):\n pathlib.Path('+repr(str(ended))+').write_text("stopped");raise SystemExit(0)\nsignal.signal(signal.SIGTERM,stop)\nprint(json.dumps({"type":"message","requestId":"queue-request","cursor":"watch-one","message":{}}),flush=True)\nwhile True:time.sleep(.05)\n'
   pathlib.Path(c['acpx_bin']).write_text(script);pathlib.Path(c['acpx_bin']).chmod(0o700)
   primary=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)'])
   process=subprocess.Popen([NODE,'--input-type=module','-e',transcript.program()],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env={**os.environ,'HOME':str(root)})
   try:
    process.stdin.write(json.dumps({**p,'action':'watch'})+'\n');process.stdin.flush()
    first=json.loads(process.stdout.readline());self.assertEqual(first['event']['cursor'],'watch-one')
    process.stdin.close();process.wait(timeout=5)
    self.assertTrue(ended.exists());self.assertIsNone(primary.poll());self.assertEqual(json.loads((state/('turn-'+r+'.json')).read_text())['phase'],'queued')
   finally:
    if process.poll()is None:process.kill();process.wait(timeout=2)
    primary.terminate();primary.wait(timeout=2)
    process.stdout.close();process.stderr.close()
