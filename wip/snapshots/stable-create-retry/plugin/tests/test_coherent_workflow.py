"""Combined native runtime fixture, not live Linux/Mac acceptance."""
import sys,pathlib,tempfile,unittest,json,subprocess,hashlib
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations,storage,file_transfer,upload_guest,workerctl as w,activity
import test_exec_checkpoint as management
import test_file_transfer as transfer
from test_operations import fixture_program
class Workflow(unittest.TestCase):
 def test_zero_acp_upload_real_shell_status_export_finish(self):
  with tempfile.TemporaryDirectory()as d:
   m=management.Finish();a,vm,row,calls,identity,facade=m.fixture(d);(a.local(vm)/'session.json').unlink();a.c.update(job='worker-'+vm,storage={'backend':'s3'},diagnostics_required=False);row['Job']['ID']=a.c['job'];a.hash=w.digest(w.canonical(a.c));facade.native={'native_tool_use_id':'fixture-shell'};facade.record=a.c['creator_record'];server=transfer.S3(pathlib.Path(d)/'s3');original=a.nomad
   policy=json.loads(pathlib.Path(a.operations_policy_file).read_text());policy.update(foreground_timeout_seconds=3,default_timeout_seconds=5,max_timeout_seconds=10,heartbeat_seconds=1,max_log_bytes=1048576,native_output_bytes=2048);w.atomic(a.operations_policy_file,policy)
   code=fixture_program(operations.program()).replace('fixture-machine',identity['machine_id']).replace('fixture-boot',identity['boot_id'])
   def nomad(args,**kw):
    if args[:2]!=['alloc','exec']or'--input-type=module'in args:return original(args,**kw)
    p=json.loads(kw['input'])
    if p.get('upload_id'):
     result=upload_guest.handle(p,lambda:identity);return subprocess.CompletedProcess(args,0,json.dumps({'ok':True,'result':result}),'')
    calls.append('operation_'+p['action']);return subprocess.run([sys.executable,'-c',code],input=kw['input'],text=True,capture_output=True,timeout=kw['timeout'])
   a.nomad=nomad;source=pathlib.Path(d)/'history.bundle'
   with source.open('wb')as f:
    for _ in range(67):f.write(b'x'*1024**2)
   transport=transfer.Upload()
   with transport.transport(server),patch.object(operations,'program',return_value=code):
    uploaded=file_transfer.upload(facade,vm,str(source),'input/history.bundle');self.assertEqual(uploaded['size_bytes'],67*1024**2)
    report=pathlib.Path(a.c['guest']['workspace'])/'report.txt';command=[sys.executable,'-c','import pathlib;pathlib.Path('+repr(str(report))+').write_text("19 synthetic checks PASS\\n");print("checks complete")']
    result=facade.execute(vm,command,timeout=5);self.assertEqual(result['exit_code'],0);self.assertFalse(result['requires_acp_connection']);self.assertEqual(a.load(vm)['phase'],'management_ready')
    status=facade._status_snapshot(vm);self.assertEqual(status['work_items'][0]['state'],'completed');self.assertEqual(status['task_state'],'none')
    finished=facade.finish(vm,['report.txt']);self.assertTrue(finished['results_verified']);self.assertFalse(finished['disk_deleted']);self.assertIn('stop',calls)
    self.assertEqual(a.load(vm)['phase'],'nomad_terminal');self.assertEqual(server.init_count,1);self.assertNotIn('submit',calls);self.assertNotIn('create',calls)
   self.assertFalse(list((pathlib.Path(a.c['guest']['state_dir'])/vm).glob('turn-*.json')))
if __name__=='__main__':unittest.main()
