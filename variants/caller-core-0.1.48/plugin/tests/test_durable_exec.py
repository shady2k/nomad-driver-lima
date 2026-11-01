import sys,pathlib,tempfile,uuid,unittest,json,subprocess,io,contextlib
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import guest_exec,operations,workerctl as w
import test_operations as fixtures
class DurableExecTests(unittest.TestCase):
 def test_public_cli_exit_codes_match_return_shape(self):
  cases=[({'protocol':'managed-operation','phase':'completed','exit_code':0},0),({'protocol':'managed-operation','phase':'failed','exit_code':7},2),({'protocol':'managed-operation','phase':'running','operation_continues':True,'outcome':'running'},3),({'protocol':'managed-operation','phase':'unknown','outcome':'unknown'},2),({'phase':'exec_completed','exit_code':0},0),({'phase':'exec_unknown'},2)]
  for result,expected in cases:
   with self.subTest(result=result),patch.object(w,'Adapter') as factory,patch.object(sys,'argv',['workerctl','--config','fixture','exec','--session',str(uuid.uuid4()),'--exec-id',str(uuid.uuid4()),'--','id']),contextlib.redirect_stdout(io.StringIO()) as output:
    factory.return_value.exec.return_value=result
    try:w.main();actual=0
    except SystemExit as e:actual=e.code
    self.assertEqual(actual,expected);self.assertEqual(json.loads(output.getvalue()),result)
 def test_same_runner_exit_output_and_no_replay(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=fixtures.OperationsTests().fixture(d);eid=str(uuid.uuid4());count=pathlib.Path(d)/'count'
   argv=[sys.executable,'-c',f'from pathlib import Path;Path({str(count)!r}).write_text("once");print("hello");raise SystemExit(7)']
   with patch.object(operations,'program',return_value=code):
    r=a.exec(s,eid,argv);self.assertEqual(r['exit_code'],7);self.assertIn('hello',r['stdout']);self.assertEqual(r['protocol'],'managed-operation')
    self.assertEqual(a.exec(s,eid,argv)['exit_code'],7);self.assertEqual(calls.count('start'),1);self.assertFalse((a.local(s)/('exec-'+eid+'.json')).exists())
 def test_lost_start_response_reconnect_and_other_activity(self):
  with tempfile.TemporaryDirectory() as d:
   f=fixtures.OperationsTests();a,s,gs,calls,code=f.fixture(d);eid=str(uuid.uuid4());original=a.nomad
   argv=[sys.executable,'-c','import time;time.sleep(.4);print("recovered")']
   def lost(args,**kw):
    r=original(args,**kw)
    if json.loads(kw['input'])['action']=='start':raise subprocess.TimeoutExpired('lost',1)
    return r
   with patch.object(operations,'program',return_value=code):
    a.nomad=lost
    with self.assertRaisesRegex(RuntimeError,'UNKNOWN_NO_REPLAY'):a.exec(s,eid,argv)
    a.nomad=original
    status=guest_exec.status(a,s,eid);self.assertIn(status['phase'],{'running','completed'});self.assertEqual(a.exec(s,eid,argv)['exit_code'],0);self.assertEqual(calls.count('start'),1)
    self.assertEqual(a.exec(s,str(uuid.uuid4()),[sys.executable,'-c','print("new")'])['exit_code'],0)
 def test_ready_and_checkpoint_fences_use_same_activity_revision(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=fixtures.OperationsTests().fixture(d)
   import activity
   with patch.object(operations,'program',return_value=code):
    a.exec(s,str(uuid.uuid4()),[sys.executable,'-c','print("checkpoint-change")']);self.assertEqual(activity.revision(a,s),1)
    state=a.load(s);state['phase']='finishing';w.atomic(a.local(s)/'session.json',state)
    with self.assertRaisesRegex(RuntimeError,'NOT_READY'):a.exec(s,str(uuid.uuid4()),['must-not-run'])
    self.assertEqual(calls.count('start'),1)
 def test_only_new_command_observes_report_only_lease(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,gs,calls,code=fixtures.OperationsTests().fixture(d);eid=str(uuid.uuid4());argv=[sys.executable,'-c','print("ok")']
   with patch.object(operations,'program',return_value=code):
    a.exec(s,eid,argv);p=a.local(s)/'lease.json';first=p.read_bytes()
    guest_exec.status(a,s,eid);a.exec(s,eid,argv);self.assertEqual(first,p.read_bytes())
    a.exec(s,str(uuid.uuid4()),argv);last=json.loads(p.read_text());self.assertEqual(last['sequence'],2)
    import lease
    report=lease.report(a.local(s));self.assertEqual(report['owner_state'],'owner_unconfirmed');self.assertFalse(report['auto_delete_enabled'])
 def test_wait_disconnect_does_not_cancel_and_explicit_cancel(self):
  with tempfile.TemporaryDirectory() as d:
   f=fixtures.OperationsTests();a,s,gs,calls,code=f.fixture(d);eid=str(uuid.uuid4())
   with patch.object(operations,'program',return_value=code):
    with patch.object(operations,'wait',side_effect=KeyboardInterrupt):
     with self.assertRaises(KeyboardInterrupt):a.exec(s,eid,[sys.executable,'-c','import time;time.sleep(1)'])
    self.assertNotIn('cancel',calls);self.assertIn(guest_exec.status(a,s,eid)['phase'],{'running','completed'})
    operations.cancel(a,s,eid);self.assertIn(f.done(a,s,eid)['phase'],{'cancelled','completed'});self.assertEqual(calls.count('start'),1)
if __name__=='__main__':unittest.main()
