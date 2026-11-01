"""Real finite subprocess/pipe/journal tests, synthetic Agent and client only."""
import pathlib,sys,os,json,uuid,tempfile,time,unittest,subprocess
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations,workerctl as w,secret_profiles
import test_operations as generic

MOCK_BAO=r'''
import sys,os,json,re,subprocess,time,signal,pathlib
h=pathlib.Path(next(x.split('=',1)[1]for x in sys.argv if x.startswith('-config='))).read_text()
command=json.loads(re.search(r'exec \{ command = (\[.*\])',h)[1])
if pathlib.Path(os.getcwd(),'render-fail').exists():sys.exit(12)
if pathlib.Path(os.getcwd(),'render-delay').exists():time.sleep(2)
for name in re.findall(r'env_template "([A-Z_]+)"',h):os.environ[name]='synthetic-process-secret'
# A supervisor diagnostic must never be merged into client output.
print('SUPERVISOR-DIAGNOSTIC synthetic-process-secret',flush=True)
child=subprocess.Popen(command)
def stop(*args):
 child.terminate()
 try:child.wait(timeout=2)
 except subprocess.TimeoutExpired:child.kill();child.wait()
 sys.exit(143)
signal.signal(signal.SIGTERM,stop)
token=pathlib.Path(json.loads(re.search(r'token_file_path = (".*?")',h)[1]));last=token.read_text()
while child.poll()is None:
 current=token.read_text()
 if current!=last:
  # Synthetic auth renewal/replacement observation, not a live Bao claim.
  pathlib.Path(os.getcwd(),'replacement-observed').write_text('observed')
  last=current
 time.sleep(.03)
sys.exit(child.returncode)
'''
class SecretOperationTests(unittest.TestCase):
 def fixture(self,d):
  base=generic.OperationsTests();a,s,gs,calls,code=base.fixture(d)
  root=pathlib.Path(d)/'tmpfs'/s;root.mkdir(parents=True,mode=0o700);(root/'vault_token').write_text('synthetic-task-token');(root/'vault_token').chmod(0o600)
  bao=pathlib.Path(d)/'mock-bao';bao.write_text('#!'+sys.executable+'\n'+MOCK_BAO);bao.chmod(0o700)
  c={'schema_version':1,'profiles':[{'id':'llm-test','version':'v1','description':'Synthetic model client test','scope':{'namespaces':[a.c['namespace']],'tasks':[a.c['task']]},'clients':{v:{'executable':sys.executable,'work_root':d}for v in ('prime','opencode')},'bindings':[{'record':'models/data/test','field':'API_KEY','env':'LLM_API_KEY'}]}]}
  machine={'machine_id':pathlib.Path('/etc/machine-id').read_text().strip(),'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}if sys.platform=='linux'else{'machine_id':'fixture-machine','boot_id':'fixture-boot'}
  state=a.load(s);binding={'session_id':s,'attempt':{k:state['attempt'][k]for k in ('allocation','task')},'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
  delivery={'catalogue':c,'catalogue_sha256':secret_profiles.digest(c),'binding':binding,'machine_identity':machine,'namespace':a.c['namespace'],'task':a.c['task'],'address':'https://secrets.example.invalid','bao_bin':str(bao)}
  w.atomic(root/'secret-profiles.json',delivery)
  override="\ndef secret_root(session):return pathlib.Path("+repr(str(root))+ ")\n"
  if sys.platform!='linux':override+="def secret_subreaper():pass\ndef secret_owned_descendants():return []\n"
  # Only Linux tmpfs/process identity locations are fixture substitutions.
  code=code.replace("if __name__=='__main__':",override+"\nif __name__=='__main__':")
  def nomad(args,**kw):
   calls.append(json.loads(kw['input'])['action']);return subprocess.run([sys.executable,'-c',code],input=kw['input'],text=True,capture_output=True,timeout=kw['timeout'])
  a.nomad=nomad
  return base,a,s,gs,calls,code,root,delivery
 def start(self,a,s,op,client,script,code,timeout=10):
  with patch.object(operations,'program',return_value=code):return operations.start(a,s,op,[sys.executable,'-c',script],'model-test',timeout=timeout,secret_profile='llm-test',secret_client=client)
 def test_two_independent_clients_cancel_one_preserves_other_and_token(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);one,two=str(uuid.uuid4()),str(uuid.uuid4())
   self.assertEqual(operations.profiles(a,s)[0]['clients'],['opencode','prime'])
   script='import os,time;print(os.environ["LLM_API_KEY"],flush=True);assert not any(k in os.environ for k in ("BAO_TOKEN","VAULT_TOKEN","NOMAD_TOKEN"));time.sleep(2)'
   self.start(a,s,one,'prime',script,code);self.start(a,s,two,'opencode',script,code)
   with self.assertRaisesRegex(RuntimeError,'VM_RETAINED'):operations.verify_terminal(a,s)
   operations.cancel(a,s,one);self.assertEqual(base.done(a,s,one)['phase'],'cancelled');self.assertEqual(base.done(a,s,two)['phase'],'completed')
   operations.verify_terminal(a,s)
   self.assertEqual((root/'vault_token').read_text(),'synthetic-task-token');self.assertEqual(list((root/'secret-operations').iterdir()),[])
   for op in (one,two):
    raw=json.dumps(operations.logs(a,s,op,0));self.assertNotIn('synthetic-process-secret',raw);self.assertNotIn('SUPERVISOR-DIAGNOSTIC',raw)
   self.assertNotIn('synthetic-process-secret',(gs/'lifecycle.jsonl').read_text())
 def test_unknown_response_reconnect_never_replays_client(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);op=str(uuid.uuid4());count=pathlib.Path(d)/'runs';script='import pathlib,time;p=pathlib.Path('+repr(str(count))+');p.write_text(p.read_text()+"x"if p.exists()else"x");time.sleep(.5)'
   original=a.nomad
   def lost(args,**kw):
    result=original(args,**kw)
    if json.loads(kw['input'])['action']=='start':raise subprocess.TimeoutExpired('fixture',1)
    return result
   a.nomad=lost
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_NO_REPLAY'):self.start(a,s,op,'prime',script,code)
   a.nomad=original;self.start(a,s,op,'prime',script,code);self.assertEqual(base.done(a,s,op)['phase'],'completed');self.assertEqual(count.read_text(),'x');self.assertEqual(calls.count('start'),1)
 def test_replacement_does_not_restart_or_change_running_child_environment(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);op=str(uuid.uuid4())
   script='import os,time;print("first="+os.environ["LLM_API_KEY"],flush=True);time.sleep(1);print("last="+os.environ["LLM_API_KEY"],flush=True)'
   self.start(a,s,op,'prime',script,code)
   while operations.status(a,s,op)['phase']=='launching':time.sleep(.03)
   time.sleep(.15);w.atomic(root/'vault_token','synthetic-replacement-token')
   self.assertEqual(base.done(a,s,op)['phase'],'completed');self.assertTrue((pathlib.Path(d)/'replacement-observed').exists());self.assertEqual(calls.count('start'),1)
   self.assertNotIn('synthetic-process-secret',json.dumps(operations.logs(a,s,op,0)))
 def test_render_failure_and_deadline_preserve_shared_token_and_clean_tmpfs(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);(pathlib.Path(d)/'render-fail').touch();op=str(uuid.uuid4());self.start(a,s,op,'prime','print("must-not-run")',code)
   self.assertEqual(base.done(a,s,op)['phase'],'failed');self.assertTrue((root/'vault_token').exists());self.assertEqual(list((root/'secret-operations').iterdir()),[])
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);op=str(uuid.uuid4());self.start(a,s,op,'prime','import time;time.sleep(10)',code,timeout=1);self.assertEqual(base.done(a,s,op)['phase'],'timed_out')
 def test_wrong_binding_and_finalization_seal_fail_closed_before_launch(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d)
   delivery['binding']['creator_generation']='wrong';w.atomic(root/'secret-profiles.json',delivery)
   with self.assertRaisesRegex(RuntimeError,'BINDING_CHANGED'):operations.profiles(a,s)
   self.assertNotIn('start',calls)
 def test_cancel_before_activation_never_executes_client(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);(pathlib.Path(d)/'render-delay').touch();marker=pathlib.Path(d)/'client-executed';op=str(uuid.uuid4())
   self.start(a,s,op,'prime','import pathlib;pathlib.Path('+repr(str(marker))+').write_text("unexpected")',code)
   operations.cancel(a,s,op);row=base.done(a,s,op);self.assertEqual(row['phase'],'cancelled');self.assertFalse(marker.exists());self.assertFalse(row['state']['effects_may_be_partial'])
 def test_workspace_symlink_denied_before_secret_client_launch(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);linked=pathlib.Path(d)/'linked';linked.symlink_to('/tmp',target_is_directory=True);op=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):operations.start(a,s,op,[sys.executable,'-c','print("must-not-run")'],'model-test',cwd=str(linked),secret_profile='llm-test',secret_client='prime')
   row=base.done(a,s,op);self.assertEqual(row['phase'],'failed');self.assertFalse(row['state']['effects_may_be_partial']);self.assertNotIn('start',json.dumps(row['state'].get('command_process')))
 def test_collect_excludes_secret_runtime_files(self):
  with tempfile.TemporaryDirectory()as d:
   base,a,s,gs,calls,code,root,delivery=self.fixture(d);op=str(uuid.uuid4());self.start(a,s,op,'opencode','import os;print("key="+os.environ["LLM_API_KEY"])',code);self.assertEqual(base.done(a,s,op)['phase'],'completed')
   out=pathlib.Path(d)/'export';out.mkdir();items=operations.capture(a,s,out)
   for path in out.rglob('*'):
    if path.is_file():self.assertNotIn(b'synthetic-process-secret',path.read_bytes());self.assertNotIn(b'synthetic-task-token',path.read_bytes())
   self.assertEqual(len(items),2)
if __name__=='__main__':unittest.main()
