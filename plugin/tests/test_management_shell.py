import pathlib,tempfile,unittest,json,sys,subprocess
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import exec_checkpoint,operations,workerctl as w
from test_operations import fixture_program
import test_guest_exec as legacy
class Management(unittest.TestCase):
 def fixture(self,d):
  a,s,row,_=legacy.ExecTests().fixture(d);state=pathlib.Path(d)/'guest';state.mkdir(mode=0o700);a.c['guest'].update(state_dir=str(state),workspace=d,max_artifact_bytes=64*1024**2);a.hash=w.digest(w.canonical(a.c));(a.local(s)/'session.json').unlink();exec_checkpoint.ensure_session(a,s);return a,s,row,state
 def test_shell_before_upload_or_acp_creates_only_management_state(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,state=self.fixture(d);code=fixture_program(operations.program());a.nomad=lambda args,**kw:subprocess.run([sys.executable,'-c',code],input=kw['input'],capture_output=True,text=True,timeout=kw['timeout'])
   with patch.object(operations,'program',return_value=code):result=a.exec(s,str(__import__('uuid').uuid4()),[sys.executable,'-c','print("shell before ACP")'],5)
   self.assertEqual(result['exit_code'],0);self.assertTrue((state/s/'management-session.json').exists());self.assertFalse((state/s/'session.json').exists())
 def test_promotion_rejects_attempt_or_creator_drift_before_remote(self):
  for drift in ('attempt','creator'):
   with self.subTest(drift=drift),tempfile.TemporaryDirectory()as d:
    a,s,row,state=self.fixture(d)
    if drift=='attempt':row['TaskStates']['worker']['Restarts']=1
    else:row['Job']['Meta']['creator_generation']='wrong'
    with patch.object(a,'remote')as remote:self.assertRaises(RuntimeError,a.create,s);remote.assert_not_called()
    self.assertEqual(a.load(s)['phase'],'management_ready')
