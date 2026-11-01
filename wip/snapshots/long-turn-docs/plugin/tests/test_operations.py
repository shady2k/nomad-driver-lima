import sys,pathlib,tempfile,json,uuid,unittest,subprocess,time,os,concurrent.futures,importlib.util
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations,workerctl as w
import test_guest_exec as legacy

def fixture_program(source):
 if sys.platform=='linux':return source
 # Cross-platform unit fixture substitutes only Linux kernel identity queries.
 # The runner, subprocess ownership, journal, locks, redaction and deadlines are real.
 override='''
def identity():return {'machine_id':'fixture-machine','boot_id':'fixture-boot'}
def process(pid):
 try:os.kill(pid,0)
 except (ProcessLookupError,PermissionError):return None
 return {'pid':pid,'ticks':str(pid),'uid':os.getuid(),'state':'R'}
'''
 return source.replace("if __name__=='__main__':",override+"\nif __name__=='__main__':")

class OperationsTests(unittest.TestCase):
 def fixture(self,d):
  a,s,row,owner=legacy.ExecTests().fixture(d);g=pathlib.Path(d)/'guest';g.mkdir(mode=0o700);gs=g/s;gs.mkdir(mode=0o700)
  a.c['guest'].update(state_dir=str(g),workspace=d,max_artifact_bytes=1048576)
  a.hash=w.digest(w.canonical(a.c));state=a.load(s) if False else {'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row)};w.atomic(a.local(s)/'session.json',state)
  w.atomic(gs/'session.json',{'phase':'ready','config':a.c['guest']})
  policy=json.loads(pathlib.Path(a.operations_policy_file).read_text());policy.update(foreground_timeout_seconds=5,default_timeout_seconds=10,max_timeout_seconds=30,heartbeat_seconds=1,max_log_bytes=1048576,native_output_bytes=2048);w.atomic(a.operations_policy_file,policy)
  source=operations.program()
  code=fixture_program(source);calls=[]
  def nomad(args,**kw):
   p=json.loads(kw['input']);calls.append(p['action']);return subprocess.run([sys.executable,'-c',code],input=kw['input'],text=True,capture_output=True,timeout=kw['timeout'])
  a.nomad=nomad
  return a,s,gs,calls,code
 def done(self,a,s,op,seconds=12):
  end=time.monotonic()+seconds
  while time.monotonic()<end:
   r=operations.status(a,s,op)
   if r['phase'] in operations.TERMINAL|{'unknown'}:return r
   time.sleep(.1)
  self.fail('Fixture did not finish')
 def test_start_is_running_logs_failure_exit_and_export(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4())
   cmd=[sys.executable,'-c','import time,sys;print("API_KEY=synthetic-secret",flush=True);time.sleep(.4);print("err",file=sys.stderr);sys.exit(7)']
   with patch.object(operations,'program',return_value=code):
    r=operations.start(a,s,op,cmd,'install-tools');self.assertNotEqual(r['phase'],'completed');r=self.done(a,s,op);self.assertEqual(r['phase'],'failed');self.assertEqual(r['state']['exit_code'],7)
    page=operations.logs(a,s,op,0);self.assertNotIn('synthetic-secret',json.dumps(page));self.assertIn('REDACTED',json.dumps(page));self.assertTrue(page['eof'])
    cursor=page['cursor'];self.assertEqual(operations.logs(a,s,op,cursor)['events'],[])
    out=pathlib.Path(d)/'export';out.mkdir();items=operations.capture(a,s,out);self.assertEqual(len(items),2)
    native=(gs/'lifecycle.jsonl').read_text();self.assertIn('install_tools',native);self.assertIn('exit_7',native);self.assertNotIn('synthetic-secret',native)
 def test_lost_launch_response_and_concurrent_same_id_never_relaunch(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4());counter=pathlib.Path(d)/'count'
   cmd=[sys.executable,'-c','import pathlib,time;p=pathlib.Path('+repr(str(counter))+');p.write_text(p.read_text()+"x" if p.exists() else "x");time.sleep(.6)']
   original=a.nomad
   def lost(args,**kw):
    result=original(args,**kw)
    if json.loads(kw['input'])['action']=='start':raise subprocess.TimeoutExpired('fixture',1)
    return result
   with patch.object(operations,'program',return_value=code):
    a.nomad=lost
    with self.assertRaisesRegex(RuntimeError,'UNKNOWN_NO_REPLAY'):operations.start(a,s,op,cmd,'prepare')
    a.nomad=original
    with concurrent.futures.ThreadPoolExecutor(2) as pool:list(pool.map(lambda _:operations.start(a,s,op,cmd,'prepare'),range(2)))
    self.assertEqual(self.done(a,s,op)['phase'],'completed');self.assertEqual(counter.read_text(),'x');self.assertEqual(calls.count('start'),1)
    with self.assertRaisesRegex(RuntimeError,'CONTENT_CHANGED'):operations.start(a,s,op,['id'],'prepare')
 def test_deadline_and_cancel_only_owned_fixture(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d)
   with patch.object(operations,'program',return_value=code):
    op=str(uuid.uuid4());operations.start(a,s,op,[sys.executable,'-c','import time;time.sleep(10)'],'slow-task',timeout=1);self.assertEqual(self.done(a,s,op)['phase'],'timed_out')
    sentinel=subprocess.Popen([sys.executable,'-c','import time;time.sleep(20)'])
    try:
     op=str(uuid.uuid4());operations.start(a,s,op,[sys.executable,'-c','import time;time.sleep(10)'],'own-task');operations.cancel(a,s,op);self.assertEqual(self.done(a,s,op)['phase'],'cancelled');self.assertIsNone(sentinel.poll())
    finally:sentinel.terminate();sentinel.wait()
 def test_wrong_owner_boot_and_running_finish_gate(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c','import time;time.sleep(1)'],'checking')
    with self.assertRaisesRegex(RuntimeError,'VM_RETAINED'):operations.verify_terminal(a,s)
    local=operations.path(a,s,op);record=json.loads(local.read_text());original_boot=record['machine_identity']['boot_id'];record['machine_identity']['boot_id']='other-boot';w.atomic(local,record)
    with self.assertRaisesRegex(RuntimeError,'BOOT_CHANGED'):operations.status(a,s,op)
    record['machine_identity']['boot_id']=original_boot;w.atomic(local,record);self.done(a,s,op)
 def test_unicode_log_chunks_keep_cursor_and_checksum(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c','print("Ж"*24000)'],'unicode-output');self.assertEqual(self.done(a,s,op)['phase'],'completed')
    cursor=0;text=''
    while True:
     page=operations.logs(a,s,op,cursor);self.assertGreater(page['cursor'],cursor);cursor=page['cursor'];text+=''.join(v['text'] for v in page['events'])
     if page['eof']:break
    self.assertEqual(text,'Ж'*24000+'\n')
 def test_native_forwarding_above_16k_preserves_unicode_and_redaction(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c','print("Ж"*24000);print("API_KEY=synthetic-secret")'],'install-go');r=self.done(a,s,op)
   rows=[json.loads(v) for v in (gs/'lifecycle.jsonl').read_text().splitlines()]
   text=''.join(v.get('text','') for v in rows if v.get('stream')=='stdout')
   self.assertIn('Ж'*24000,text);self.assertNotIn('synthetic-secret',text)
   self.assertGreater(r['state']['native_output_bytes'],16384)
   self.assertFalse(r['state']['native_output_truncated'])
 def test_native_limit_notice_once_and_guest_log_limit_explicit(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4());original=a.nomad
   def capped(args,**kw):
    payload=json.loads(kw['input'])
    if 'policy' in payload:payload['policy']['native_output_bytes']=17000
    kw['input']=json.dumps(payload);return original(args,**kw)
   a.nomad=capped
   with patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c','print("x"*24000);print("API_KEY=synthetic-secret")'],'download-tools');r=self.done(a,s,op)
   text=(gs/'lifecycle.jsonl').read_text()
   self.assertEqual(text.count('[NATIVE_OUTPUT_LIMIT]'),1);self.assertIn('operation-logs',text)
   self.assertNotIn('synthetic-secret',text);self.assertTrue(r['state']['native_output_truncated'])
   self.assertFalse(r['state']['output_truncated']);self.assertEqual(r['state']['native_output_bytes'],17000)
 def test_wrong_generation_and_symlink_rejected_without_launch(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4());record=json.loads(pathlib.Path(a.c['creator_record']).read_text());record['generation']=str(uuid.uuid4());w.atomic(a.c['creator_record'],record)
   with patch.object(operations,'program',return_value=code):
    with self.assertRaisesRegex(RuntimeError,'GENERATION_CHANGED'):operations.start(a,s,op,['id'],'inspect')
   self.assertEqual(calls,[])
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4());(gs/'operations').mkdir(mode=0o700);(gs/'operations'/op).symlink_to(pathlib.Path(d),target_is_directory=True)
   with patch.object(operations,'program',return_value=code):
    with self.assertRaisesRegex(RuntimeError,'UNSAFE_OPERATION_DIRECTORY'):operations.start(a,s,op,['id'],'inspect')
 def test_cancel_forged_process_identity_no_signal(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=self.fixture(d);op=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c','import time;time.sleep(10)'],'wait-task')
    statefile=gs/'operations'/op/'state.json';state=json.loads(statefile.read_text());ticks=state['command_process']['ticks'];state['command_process']['ticks']='forged';w.atomic(statefile,state)
    with self.assertRaisesRegex(RuntimeError,'IDENTITY_UNCONFIRMED'):operations.cancel(a,s,op)
    state['command_process']['ticks']=ticks;w.atomic(statefile,state);operations.cancel(a,s,op);self.assertEqual(self.done(a,s,op)['phase'],'cancelled')
 def test_v2_native_log_metadata_redaction(self):
  spec=importlib.util.spec_from_file_location('task_main_ops',pathlib.Path(__file__).parents[1]/'guest/task-main.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  event={'component':'agent-operation','operation_id':str(uuid.uuid4()),'label':'install-tools','state':'running','stage':'operation_install_tools_running','stream':'stdout','text':'API_KEY=synthetic-secret','env':{'secret':'never-copy'}}
  out=m.lifecycle_event(event);self.assertNotIn('synthetic-secret',json.dumps(out));self.assertNotIn('env',out);self.assertEqual(out['label'],'install-tools')
if __name__=='__main__':unittest.main()
