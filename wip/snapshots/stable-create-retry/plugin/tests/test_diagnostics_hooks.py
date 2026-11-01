import base64,importlib.util,json,pathlib,subprocess,sys,tempfile,time,unittest,uuid
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import diagnostics,session_hook,storage,exchange
from secure_logs import Redactor
class DiagnosticTests(unittest.TestCase):
 def frame(self,file,raw,end=None):return {'File':'alloc/logs/worker.stdout.'+str(file),'Offset':len(raw) if end is None else end,'Data':base64.b64encode(raw).decode()}
 def test_complete_segments_secret_split_and_redaction(self):
  with tempfile.TemporaryDirectory() as d:
   a=b'API_KEY="synthetic';b=b'-secret"\nhttps://example.invalid/file?X-Amz-Signature=synthetic-signature\nnext\n';frames=[self.frame(0,a),self.frame(0,b,len(a)+len(b)),self.frame(1,b'last\n')]
   item,coverage=diagnostics.stream_snapshot(frames,'worker','stdout',pathlib.Path(d),1048576,Redactor());raw=(pathlib.Path(d)/item['local_file']).read_text();self.assertNotIn('synthetic-secret',raw);self.assertNotIn('synthetic-signature',raw);self.assertIn('[REDACTED]',raw);self.assertEqual(len(coverage),2);self.assertTrue(raw.endswith('last\n'))
 def test_missing_rotated_gap_wrong_file_or_error_never_commit(self):
  bad=[[self.frame(1,b'x')],[self.frame(0,b'x',10)],[{'File':'other.stdout.0','Offset':1,'Data':'eA=='}],[{'error':'upstream failure'}],[{'FileEvent':'file deleted'}]]
  for frames in bad:
   with tempfile.TemporaryDirectory() as d:
    with self.assertRaises(RuntimeError):diagnostics.stream_snapshot(frames,'worker','stdout',pathlib.Path(d),1048576,Redactor())
    self.assertFalse((pathlib.Path(d)/'native-stdout.log').exists())
 def test_guest_main_streams_and_requires_receipt(self):
  with tempfile.TemporaryDirectory() as root:
   s=str(uuid.uuid4());d=pathlib.Path(root)/s;d.mkdir(mode=0o700);(d/'lifecycle.jsonl').write_text(json.dumps({'stage':'request_running','session_id':s,'api_key':'must-not-emit'})+'\n');(d/'finish.json').write_text(json.dumps({'session_id':s,'export_verified':True,'manifest_sha256':'a'*64}))
   p=subprocess.run([sys.executable,str(pathlib.Path(__file__).parents[1]/'guest/task-main.py'),'--state-dir',root,'--session',s,'--runtime-check','/usr/bin/true'],capture_output=True,text=True,timeout=5);self.assertEqual(p.returncode,0,p.stderr);self.assertIn('request_running',p.stdout);self.assertNotIn('must-not-emit',p.stdout)
   (d/'finish.json').write_text('{}');p=subprocess.run([sys.executable,str(pathlib.Path(__file__).parents[1]/'guest/task-main.py'),'--state-dir',root,'--session',s,'--runtime-check','/usr/bin/true'],capture_output=True,text=True,timeout=5);self.assertNotEqual(p.returncode,0)
class HookTests(unittest.TestCase):
 def test_native_hook_records_no_prompts_and_new_generation_on_resume(self):
  with tempfile.TemporaryDirectory() as d:
   event={'hook_event_name':'SessionStart','session_id':'native-session-1','source':'startup','transcript_path':'never-read','user_prompt':'not-stored'};p,first=session_hook.record(d,event,now=10);raw=pathlib.Path(p).read_text();self.assertNotIn('not-stored',raw);self.assertNotIn('never-read',raw)
   _,compact=session_hook.record(d,{**event,'source':'compact'},now=20);self.assertEqual(first['generation'],compact['generation'])
   _,resumed=session_hook.record(d,{**event,'source':'resume'},now=30);self.assertNotEqual(first['generation'],resumed['generation']);self.assertEqual(resumed['native_session_id'],'native-session-1')
   _,ended=session_hook.record(d,{'hook_event_name':'SessionEnd','session_id':'native-session-1'},now=40);self.assertTrue(ended['ended']);self.assertIn('not_authenticated',ended['evidence'])
if __name__=='__main__':unittest.main()

class DiagnosticReceiptTests(unittest.TestCase):
 def test_full_s3_diagnostic_binding_and_readback_without_vm(self):
  from test_catalog_lifecycle import StorageTests,MemoryStore
  fixture=StorageTests()
  with tempfile.TemporaryDirectory() as d:
   a,s,r=fixture.fixture(d,{'backend':'s3'});a.c['diagnostics_required']=True;a.c['namespace']='agent-workers'
   # Config was updated only for this synthetic unit fixture; refresh its binding.
   import workerctl as w
   a.hash=w.digest(w.canonical(a.c));target={'ID':a.c['allocation'],'Job':{'Version':1},'TaskStates':{'worker':{'Restarts':0,'StartedAt':'synthetic-start','Events':[{'Type':'Started','DisplayMessage':'API_KEY="synthetic-secret"'}]}}};a.target=lambda:target;state={'config_hash':a.hash,'phase':'ready','attempt':a.attempt(target)};w.atomic(a.local(s)/'session.json',state)
   def frames(*args,**kw):
    typ='stderr' if 'type=stderr' in args[1] else 'stdout';raw=b'API_KEY="synthetic-secret"\n'
    return iter([{'File':'alloc/logs/worker.'+typ+'.0','Offset':len(raw),'Data':base64.b64encode(raw).decode()}])
   store=MemoryStore();backend=storage.binding(store.cfg)
   with mock.patch.object(storage,'store',return_value=(store,backend)),mock.patch.object(diagnostics,'frames',side_effect=frames),mock.patch.object(diagnostics,'plain_bytes',return_value=b'API_KEY="synthetic-secret"\n'):
    m=a.collect(s,r,['result.txt']);self.assertTrue(m['export_binding']['diagnostics_required']);paths=[x['path'] for x in m['files']];self.assertIn('.nomad-logs/diagnostics-index.json',paths)
    for value in store.objects.values():self.assertNotIn(b'synthetic-secret',value)
    local=a.local(s)/json.loads((a.local(s)/(r+'.json')).read_text())['manifest_dir']/'0.bin';local.unlink();a.target=lambda:(_ for _ in ()).throw(AssertionError('result must not access stopped VM'));a.result(s,r);self.assertEqual(local.read_bytes(),b'output')
 def test_missing_diagnostics_blocks_export_and_finish_on_unknown_turn(self):
  from test_catalog_lifecycle import StorageTests
  import workerctl as w
  fixture=StorageTests()
  with tempfile.TemporaryDirectory() as d:
   a,s,r=fixture.fixture(d,{'backend':'s3'});a.c['diagnostics_required']=True;a.hash=w.digest(w.canonical(a.c));state=json.loads((a.local(s)/'session.json').read_text());state['config_hash']=a.hash;w.atomic(a.local(s)/'session.json',state)
   with mock.patch.object(diagnostics,'capture',side_effect=RuntimeError('logs ACL unavailable')):
    with self.assertRaisesRegex(RuntimeError,'DIAGNOSTIC_EXPORT'):a.collect(s,r,['result.txt'])
   rec=json.loads((a.local(s)/(r+'.json')).read_text());self.assertFalse(rec['export_verified']);self.assertEqual(rec['phase'],'export_failed_retryable')
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_LOCAL_TURN|NOT_EXPORTED'):a.finish(s,r)
   rec['phase']='watch_unknown';w.atomic(a.local(s)/(r+'.json'),rec)
   with self.assertRaisesRegex(RuntimeError,'NOT_TERMINAL'):a.collect(s,r,['result.txt'])
 def test_cancelled_terminal_preserved_not_reported_as_completed_task(self):
  from test_catalog_lifecycle import StorageTests
  import workerctl as w
  with tempfile.TemporaryDirectory() as d:
   a,s,r=StorageTests().fixture(d);p=a.local(s)/(r+'.json');v=json.loads(p.read_text());v['terminal']={'status':'cancelled','stopReason':'cancelled'};w.atomic(p,v);m=a.collect(s,r,['result.txt']);self.assertEqual(m['terminal']['status'],'cancelled');self.assertNotIn('task_completed',m)

class FinishUnknownTests(unittest.TestCase):
 def test_other_unknown_request_blocks_even_previously_exported_result(self):
  from test_catalog_lifecycle import StorageTests
  import workerctl as w
  with tempfile.TemporaryDirectory() as d:
   a,s,r=StorageTests().fixture(d);a.collect(s,r,['result.txt']);unknown=str(uuid.uuid4());w.atomic(a.local(s)/(unknown+'.json'),{'request_id':unknown,'phase':'submit_unknown'})
   a.target=lambda:(_ for _ in ()).throw(AssertionError('must retain before remote close'))
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_LOCAL_TURN'):a.finish(s,r)

class ReviewRegressionTests(unittest.TestCase):
 def test_finish_requires_every_terminal_export_not_only_selected_request(self):
  from test_catalog_lifecycle import StorageTests
  import workerctl as w
  with tempfile.TemporaryDirectory() as d:
   a,s,r=StorageTests().fixture(d);a.collect(s,r,['result.txt']);other=str(uuid.uuid4());w.atomic(a.local(s)/(other+'.json'),{'request_id':other,'phase':'terminal','terminal':{'status':'completed'},'export_verified':False})
   a.target=lambda:(_ for _ in ()).throw(AssertionError('must not close remote'))
   with self.assertRaisesRegex(RuntimeError,'OTHER_TERMINAL_TURN_NOT_EXPORTED'):a.finish(s,r)
 def test_create_pending_allocation_does_not_bind_prestart_attempt(self):
  from test_catalog_lifecycle import StorageTests
  with tempfile.TemporaryDirectory() as d:
   a,s,r=StorageTests().fixture(d);s2=str(uuid.uuid4());a.target=lambda:{'ID':a.c['allocation'],'ClientStatus':'pending','TaskStates':{'worker':{'State':'pending','StartedAt':'0001-01-01T00:00:00Z'}}}
   with self.assertRaisesRegex(RuntimeError,'TASK_NOT_RUNNING'):a.create(s2)
   self.assertFalse((a.local(s2)/'session.json').exists())
 def test_collect_is_immutable_and_repeat_does_not_reexport(self):
  from test_catalog_lifecycle import StorageTests
  with tempfile.TemporaryDirectory() as d:
   a,s,r=StorageTests().fixture(d);m=a.collect(s,r,['result.txt']);a.remote=lambda *args,**kw:(_ for _ in ()).throw(AssertionError('repeat export must use receipt'))
   self.assertEqual(a.collect(s,r,['result.txt']),m)
   with self.assertRaisesRegex(RuntimeError,'DECLARATION_CHANGED'):a.collect(s,r,['other.txt'])
