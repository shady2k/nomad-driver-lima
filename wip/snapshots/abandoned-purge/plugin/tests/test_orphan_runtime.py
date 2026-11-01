import importlib.util,json,os,pathlib,signal,subprocess,sys,tempfile,unittest,ast,time
from unittest import mock
ROOT=pathlib.Path(__file__).parents[2]
spec=importlib.util.spec_from_file_location('stop_runtime_fix',ROOT/'plugin/guest/task-main.py');runtime=importlib.util.module_from_spec(spec);spec.loader.exec_module(runtime)
source=(ROOT/'internal/lima/vault_guest.py').read_text();tree=ast.parse(source);function=next(n for n in tree.body if isinstance(n,ast.FunctionDef)and n.name=='stop_owned_runtime');scope={'os':os,'pathlib':pathlib,'time':time,'signal':signal,'json':json};exec(compile(ast.Module(body=[function],type_ignores=[]),'driver-owned-cleanup','exec'),scope);stop_owned_runtime=scope['stop_owned_runtime']
class OrphanRuntimeTests(unittest.TestCase):
 def test_term_resistant_exact_owned_child_killed_unrelated_untouched(self):
  owned=subprocess.Popen([sys.executable,'-c','import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);print("ready",flush=True);time.sleep(60)'],stdout=subprocess.PIPE,text=True);self.assertEqual(owned.stdout.readline().strip(),'ready')
  unrelated=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)'])
  try:
   with tempfile.TemporaryDirectory()as d:
    root=pathlib.Path(d);row={'pid':owned.pid,'ticks':'fixed-owned'};(root/'runtime-owner.json').write_text(json.dumps({'processes':[row]}));clock=[0]
    def monotonic():clock[0]+=4;return clock[0]
    with mock.patch.object(runtime,'descendants',return_value=[row]),mock.patch.object(runtime,'alive',side_effect=lambda v:owned.poll()is None),mock.patch.object(runtime,'listening_socket_inodes',return_value=set()),mock.patch.object(runtime.time,'monotonic',side_effect=monotonic):self.assertTrue(stop_owned_runtime(runtime,str(root)))
    self.assertEqual(owned.wait(timeout=2),-signal.SIGKILL);self.assertIsNone(unrelated.poll());self.assertTrue(json.loads((root/'runtime-stopped.json').read_text())['stopped'])
  finally:
   if owned.poll()is None:owned.kill()
   owned.wait();owned.stdout.close();unrelated.terminate();unrelated.wait()
 def test_reused_pid_never_signalled(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);row={'pid':99999,'ticks':'old'};(root/'runtime-owner.json').write_text(json.dumps({'processes':[row]}))
   with mock.patch.object(runtime,'descendants',return_value=[row]),mock.patch.object(runtime,'alive',return_value=False),mock.patch.object(runtime,'listening_socket_inodes',return_value=set()),mock.patch.object(runtime.os,'kill')as kill:self.assertTrue(stop_owned_runtime(runtime,str(root)));kill.assert_not_called()
 def test_revoke_403_is_not_success_or_proof_of_absence(self):
  text=(ROOT/'internal/lima/vault_guest.py').read_text();self.assertIn('TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED',text);self.assertNotIn("if not os.path.exists(d+'/runtime-stopped.json')",text)

 def test_synthetic_403_retains_history_and_rechecks_even_stale_stopped_receipt(self):
  import hashlib
  with tempfile.TemporaryDirectory()as temp:
   root=pathlib.Path(temp)/'volatile';root.mkdir(mode=0o700);session=str(__import__('uuid').uuid4());d=root/session;d.mkdir(mode=0o700);(d/'tokens').mkdir(mode=0o700);token='PUBLIC_SYNTHETIC_TASK_TOKEN_NOT_A_CREDENTIAL';name=hashlib.sha256(token.encode()).hexdigest();(d/'tokens'/name).write_text(token);(d/'tokens'/name).chmod(0o600);(d/'runtime-stopped.json').write_text('{"stopped":true,"processes":[]}')
   fake=pathlib.Path(temp)/'runtime.py';fake.write_text('import pathlib,json\ndef stop_runtime(d):\n pathlib.Path(d,"stop-rechecked").write_text("yes")\n pathlib.Path(d,"runtime-stopped.json").write_text(json.dumps({"stopped":True,"processes":[]}))\n')
   program=(ROOT/'internal/lima/vault_guest.py').read_text().replace("assert any(x.split()[4]=='/dev/shm' and x.split(' - ')[1].split()[0]=='tmpfs' for x in open('/proc/self/mountinfo'))",'assert True # fixture mount only').replace("root='/dev/shm/agent-worker-'+str(os.getuid())",'root='+repr(str(root))).replace("'/opt/nomad-workers/task-main.py'",repr(str(fake)))
   injected="\nclass FixtureOpener:\n def open(self,*a,**kw):raise urllib.error.HTTPError('https://fixture.invalid',403,'denied',{},None)\nurllib.request.build_opener=lambda *a:FixtureOpener()\n"
   program=program.replace('p=json.load(sys.stdin)',injected+'\np=json.load(sys.stdin)')
   r=subprocess.run([sys.executable,'-c',program],input=json.dumps({'session':session,'owner':'a'*32,'action':'revoke','address':'https://fixture.invalid'}),text=True,capture_output=True,timeout=5)
   self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(json.loads(r.stdout),{'ok':False,'code':'TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED'});self.assertTrue((d/'stop-rechecked').exists());self.assertTrue((d/'tokens'/name).exists());self.assertFalse((d/'revoked.json').exists());self.assertNotIn(token,r.stdout+r.stderr)

 def test_stop_only_branch_exits_before_any_token_or_http_access(self):
  with tempfile.TemporaryDirectory()as temp:
   root=pathlib.Path(temp)/'volatile';root.mkdir(mode=0o700);session=str(__import__('uuid').uuid4());fake=pathlib.Path(temp)/'runtime.py';fake.write_text('import pathlib,json\ndef stop_runtime(d):\n pathlib.Path(d,"runtime-stopped.json").write_text(json.dumps({"stopped":True,"processes":[]}))\n')
   program=(ROOT/'internal/lima/vault_guest.py').read_text().replace("assert any(x.split()[4]=='/dev/shm' and x.split(' - ')[1].split()[0]=='tmpfs' for x in open('/proc/self/mountinfo'))",'assert True # fixture mount only').replace("root='/dev/shm/agent-worker-'+str(os.getuid())",'root='+repr(str(root))).replace("'/opt/nomad-workers/task-main.py'",repr(str(fake)))
   program=program.replace('p=json.load(sys.stdin)',"urllib.request.build_opener=lambda *a:(_ for _ in ()).throw(AssertionError('HTTP MUST NOT RUN'))\np=json.load(sys.stdin)")
   r=subprocess.run([sys.executable,'-c',program],input=json.dumps({'session':session,'owner':'a'*32,'action':'stop-runtime'}),text=True,capture_output=True,timeout=5)
   self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(json.loads(r.stdout),{'ok':True,'runtime_stopped':True,'revocation':'not_attempted','tokens_retained':True});self.assertFalse((root/session/'tokens').exists());self.assertTrue((root/session/'sealed').exists())
