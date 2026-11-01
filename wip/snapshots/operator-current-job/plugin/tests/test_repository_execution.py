"""Real subprocess/client tests in a relocated guest filesystem, not a live VM.
Only Linux identity/UID/path roots are relocated; digest/readonly guards remain.
"""
import sys,pathlib,json,tempfile,unittest,subprocess,os,hashlib,shutil,uuid
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import repository_profiles,guest_exec,operations
import test_repository_profile as profiles
import test_operations as managed
p=repository_profiles.guest
class RepositoryExecutionTests(unittest.TestCase):
 def fixture(self,d):
  root=pathlib.Path(d).resolve();plan=p.render(profiles.profile(),profiles.PLATFORM,{'ubuntu.sources':profiles.SOURCES});prefix=str(root/'run/nomad-worker-repository-profiles');original='/run/nomad-worker-repository-profiles'
  for key,value in list(plan['environment'].items()):plan['environment'][key]=value.replace(original,prefix)
  source=(pathlib.Path(__file__).parents[1]/'guest/repository_profile.py').read_text().replace(original,prefix).replace('owner_uid=0','owner_uid=os.getuid()').replace('/tmp/nomad-workers-repository-cache',str(root/'tmp/nomad-workers-repository-cache'))
  (root/'tmp').mkdir(exist_ok=True);plan['files'][plan['base']+'/executor.py']=source;receipt=p.write_plan(plan,root);ref={'profile_sha256':plan['profile_sha256'],'environment_sha256':receipt['environment_sha256'],'executor_sha256':hashlib.sha256(source.encode()).hexdigest()}
  loader=(pathlib.Path(__file__).parents[1]/'guest/repository_exec.py').read_text().replace(original,prefix).replace('st.st_uid!=0','st.st_uid!=os.getuid()');empty=root/'empty.npmrc';empty.write_text('')
  env={'HOME':d,'PATH':os.environ['PATH'],'GOTOOLCHAIN':'local','GOCACHE':str(root/'go-cache'),'NPM_CONFIG_GLOBALCONFIG':str(empty)}
  wrapper=loader+"\nimport json,os,sys\ncommand=repository_bound_command(json.loads(sys.argv[1]),sys.argv[2:])\nos.execvpe(command[0],command,os.environ)\n"
  return plan,ref,loader,wrapper,env
 @unittest.skipUnless(shutil.which('npm')and shutil.which('go'),'native npm/Go not installed')
 def test_workload_and_native_foreground_use_real_npm_go_clients(self):
  with tempfile.TemporaryDirectory()as d:
   plan,ref,loader,wrapper,env=self.fixture(d)
   for command,expected in (([shutil.which('npm'),'config','get','registry'],'https://cache.example.invalid/npm/'),([shutil.which('go'),'env','GOPROXY'],'https://cache.example.invalid/go-modules')):
    # Same bounded native-driver wrapper used for the main workload.
    result=subprocess.run([sys.executable,'-c',wrapper,json.dumps(ref),*command],env=env,cwd=d,capture_output=True,text=True,timeout=20);self.assertEqual(result.returncode,0,result.stderr);self.assertEqual(result.stdout.strip(),expected)
    # The foreground native exec runner also wraps its actual child process.
    original=(pathlib.Path(__file__).parents[1]/'guest/repository_exec.py').read_text();code=guest_exec.GUEST_RUNNER.replace(original,loader)
    result=subprocess.run([sys.executable,'-c',wrapper,json.dumps(ref),sys.executable,'-c',code],input=json.dumps({'argv':command,'cwd':d,'timeout':15}),env=env,cwd=d,capture_output=True,text=True,timeout=20);self.assertEqual(result.returncode,0,result.stderr);frame=json.loads(result.stdout);self.assertEqual(frame['exit_code'],0);self.assertEqual(frame['stdout'].strip(),expected)
 @unittest.skipUnless(shutil.which('npm'),'native npm not installed')
 def test_managed_operation_supervisor_keeps_profile_after_background_start(self):
  with tempfile.TemporaryDirectory()as d:
   profile_root=pathlib.Path(d)/'profile';profile_root.mkdir();plan,ref,loader,wrapper,env=self.fixture(str(profile_root));work=pathlib.Path(d)/'work';work.mkdir(mode=0o700);a,s,gs,calls,code=managed.OperationsTests().fixture(str(work));original=(pathlib.Path(__file__).parents[1]/'guest/repository_exec.py').read_text();code=code.replace(original,loader)
   def nomad(args,**kw):return subprocess.run([sys.executable,'-c',wrapper,json.dumps(ref),sys.executable,'-c',code],input=kw['input'],env=env,cwd=d,text=True,capture_output=True,timeout=kw['timeout'])
   a.nomad=nomad;op=str(uuid.uuid4())
   with mock.patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[shutil.which('npm'),'config','get','registry'],'profile-check');state=managed.OperationsTests().done(a,s,op);self.assertEqual(state['phase'],'completed');page=operations.logs(a,s,op,0);self.assertIn('https://cache.example.invalid/npm/',json.dumps(page['events']))
 def test_sudo_env_reset_still_loads_profile_inside_target_process(self):
  with tempfile.TemporaryDirectory()as d:
   plan,ref,loader,wrapper,env=self.fixture(d);root=pathlib.Path(d);bin=root/'bin';bin.mkdir();fake=bin/'sudo'
   # No privilege simulation: only sudo's environment reset is exercised.
   fake.write_text('#!'+sys.executable+'\nimport os,sys\na=sys.argv[1:]\nwhile a and a[0] in ("-n","-H","-E"):a=a[1:]\nif a[:1]==["--"]:a=a[1:]\nenv={k:os.environ[k]for k in ("HOME","PATH","GOTOOLCHAIN","GOCACHE","NPM_CONFIG_GLOBALCONFIG")if k in os.environ}\nos.execvpe(a[0],a,env)\n');fake.chmod(0o700);env['PATH']=str(bin)+':'+env['PATH']
   command=[str(fake),'-n',sys.executable,'-c','import os;print(os.environ["GOPROXY"])'];result=subprocess.run([sys.executable,'-c',wrapper,json.dumps(ref),*command],env=env,cwd=d,capture_output=True,text=True,timeout=15);self.assertEqual(result.returncode,0,result.stderr);self.assertEqual(result.stdout.strip(),'https://cache.example.invalid/go-modules')
