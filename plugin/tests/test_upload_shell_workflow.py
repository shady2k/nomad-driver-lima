"""67 MiB source → multipart → guest file → real managed shell, zero ACP."""
import pathlib,sys,tempfile,unittest,json,subprocess,types,hashlib
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import file_transfer,operations,workerctl,vm_interface
from test_operations import fixture_program
import test_file_transfer as transfer
class Workflow(unittest.TestCase):
 def test_67_mib_upload_then_managed_shell_without_acp(self):
  with tempfile.TemporaryDirectory()as d:
   helper=transfer.Upload();a,vm,service,source,server,calls,lose,workspace=helper.fixture(d,67*1024**2);(a.local(vm)/'session.json').unlink();a.c['guest']['max_artifact_bytes']=1048576;a.hash=workerctl.digest(workerctl.canonical(a.c));policy=json.loads(pathlib.Path(a.operations_policy_file).read_text());policy.update(foreground_timeout_seconds=3,default_timeout_seconds=5,max_timeout_seconds=10,heartbeat_seconds=1,max_log_bytes=1048576,native_output_bytes=2048);workerctl.atomic(a.operations_policy_file,policy);original=a.nomad;code=fixture_program(operations.program())
   def nomad(args,**kw):
    if json.loads(kw['input']).get('upload_id'):return original(args,**kw)
    return subprocess.run([sys.executable,'-c',code],input=kw['input'],capture_output=True,text=True,timeout=kw['timeout'])
   a.nomad=nomad;facade=vm_interface.VMs.__new__(vm_interface.VMs);facade.resolve=service.resolve;facade.native={'native_tool_use_id':'fixture-upload-shell'};facade.me={'agent_id':a.c['agent_instance_id'],'native_session_id':a.c['creator_session_id'],'generation':a.c['creator_generation']};facade.client=types.SimpleNamespace(root=a.root)
   with helper.transport(server),patch.object(operations,'program',return_value=code):
    uploaded=file_transfer.upload(facade,vm,str(source),'input/history.bundle');target=workspace/'input/history.bundle';expected=uploaded['sha256']
    command=[sys.executable,'-c','import hashlib,pathlib; p=pathlib.Path('+repr(str(target))+'); assert p.stat().st_size==67*1024**2; assert hashlib.sha256(p.read_bytes()).hexdigest()=='+repr(expected)+'; print("size and SHA verified")']
    shell=facade.execute(vm,command,timeout=5)
   self.assertEqual(shell['exit_code'],0);self.assertTrue(shell['execution_id']);self.assertEqual(shell['next_action'],'operation-logs');self.assertIn('SHA verified',shell['stdout']);self.assertFalse(shell['requires_acp_connection']);self.assertEqual(a.load(vm)['phase'],'management_ready');self.assertEqual(server.init_count,1);self.assertFalse(list(pathlib.Path(a.c['guest']['state_dir']).rglob('turn-*.json')));self.assertFalse((pathlib.Path(a.c['guest']['state_dir'])/vm/'session.json').exists())
