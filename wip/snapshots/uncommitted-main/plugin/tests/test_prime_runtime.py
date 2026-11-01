import importlib.util,pathlib,tempfile,threading,time,unittest
from unittest import mock
spec=importlib.util.spec_from_file_location('guest_task_main',pathlib.Path(__file__).parents[1]/'guest/task-main.py');runtime=importlib.util.module_from_spec(spec);spec.loader.exec_module(runtime)
class RuntimeTests(unittest.TestCase):
 def test_start_stop_are_serialized(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);started=threading.Event();release=threading.Event();order=[]
   def start(*args):started.set();release.wait(2);order.append('start')
   def stop(*args):order.append('stop')
   with mock.patch.object(runtime,'token_root',return_value=root),mock.patch.object(runtime,'_start_runtime',side_effect=start),mock.patch.object(runtime,'_stop_runtime',side_effect=stop):
    a=threading.Thread(target=runtime.start_runtime,args=(root,'session',{}));a.start();self.assertTrue(started.wait(1))
    b=threading.Thread(target=runtime.stop_runtime,args=(root,));b.start();time.sleep(.03);self.assertEqual(order,[])
    release.set();a.join(2);b.join(2);self.assertEqual(order,['start','stop']);self.assertFalse(a.is_alive() or b.is_alive())
 def test_no_replay_after_unknown_start_intent(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);runtime.atomic(root/'runtime-start-intent.json',{'config_hash':'synthetic'})
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):runtime.stop_runtime(root)
   self.assertFalse((root/'runtime-stopped.json').exists())
 def test_reused_pid_is_never_signalled(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);runtime.atomic(root/'runtime-owner.json',{'processes':[{'pid':123,'ticks':'old'}]})
   with mock.patch.object(runtime,'identity',return_value={'pid':123,'ticks':'new'}),mock.patch.object(runtime.os,'kill') as kill:
    runtime.stop_runtime(root);kill.assert_not_called()
   self.assertTrue((root/'runtime-stopped.json').exists())

class ThreadOwnershipTests(unittest.TestCase):
 def test_children_of_nonleader_threads_and_recursive_parentage(self):
  with tempfile.TemporaryDirectory() as d:
   real=pathlib.Path;proc=real(d)
   for tid,children in [('10/task/10',''),('10/task/11','20'),('20/task/20','30'),('30/task/30','')]:
    p=proc/tid;p.mkdir(parents=True);(p/'children').write_text(children)
   identities={n:{'pid':n,'ticks':str(n)} for n in (10,20,30)}
   def mapped(value,*args):
    s=str(value)
    return proc/s.removeprefix('/proc/') if s.startswith('/proc/') else real(value,*args)
   with mock.patch.object(runtime.pathlib,'Path',side_effect=mapped),mock.patch.object(runtime,'identity',side_effect=lambda pid:identities.get(pid)),mock.patch.object(runtime,'process_parent',side_effect=lambda pid:{20:10,30:20}.get(pid)):
    self.assertEqual({r['pid'] for r in runtime.descendants([identities[10]])},{10,20,30})
 def test_reparented_child_and_reused_root_not_claimed(self):
  with tempfile.TemporaryDirectory() as d:
   real=pathlib.Path;proc=real(d);p=proc/'10/task/11';p.mkdir(parents=True);(p/'children').write_text('20')
   root={'pid':10,'ticks':'old'};child={'pid':20,'ticks':'20'}
   def mapped(value,*args):return proc/str(value).removeprefix('/proc/') if str(value).startswith('/proc/') else real(value,*args)
   with mock.patch.object(runtime.pathlib,'Path',side_effect=mapped),mock.patch.object(runtime,'identity',side_effect=lambda pid:root if pid==10 else child),mock.patch.object(runtime,'process_parent',return_value=99):
    self.assertEqual(runtime.descendants([root]),[root])
   with mock.patch.object(runtime,'identity',return_value={'pid':10,'ticks':'new'}):self.assertEqual(runtime.descendants([root]),[])
 def test_readiness_diagnostics_are_allowlisted_and_secret_free(self):
  import io,contextlib,json
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);(root/'vault_token').write_text('must-not-emit-business-key');(root/'vault_token').chmod(0o600)
   with mock.patch.object(runtime,'descendants',return_value=[]),mock.patch.object(runtime,'alive',return_value=False),mock.patch.object(runtime,'socket_owned_by',return_value=False),contextlib.redirect_stdout(io.StringIO()) as output:
    supervisor=mock.Mock();supervisor.poll.return_value=7
    runtime.readiness_failure(root,'00000000-0000-4000-8000-000000000001',root,{'pid':123,'ticks':'1'},None,supervisor,'BAO_SUPERVISOR_DIED')
   data=(root/'runtime-readiness-diagnostics.json').read_text()
   self.assertNotIn('must-not-emit-business-key',data+output.getvalue())
   doc=json.loads(data);self.assertEqual(doc['supervisor_exit_code'],7);self.assertTrue(doc['token']['private']);self.assertFalse(doc['socket']['present'])
   self.assertEqual((root/'runtime-readiness-diagnostics.json').stat().st_mode&0o777,0o600)
 def test_socket_requires_exact_daemon_identity(self):
  with mock.patch.object(runtime,'alive',return_value=False):self.assertFalse(runtime.socket_owned_by({'pid':123,'ticks':'old'},pathlib.Path('/not-read')))

class ProcStatParsingTests(unittest.TestCase):
 def test_non_utf8_comm_and_parenthesis_do_not_change_identity_fields(self):
  fields=[b'S',b'42']+[b'0']*17+[b'123456']
  raw=b'123 (non-utf8-\xff) name) '+b' '.join(fields)
  with mock.patch.object(runtime.pathlib.Path,'read_bytes',return_value=raw):
   self.assertEqual(runtime.identity(123),{'pid':123,'ticks':'123456'})
   self.assertEqual(runtime.process_parent(123),42)
 def test_malformed_stat_never_creates_owned_identity(self):
  for raw in [b'',b'123 (bad) S 1',b'123 no delimiter']:
   with mock.patch.object(runtime.pathlib.Path,'read_bytes',return_value=raw):
    self.assertIsNone(runtime.identity(123))

class StopListenerGuardTests(unittest.TestCase):
 def test_unknown_live_listener_cannot_be_reported_stopped_or_signalled(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);runtime.atomic(root/'runtime-owner.json',{'processes':[{'pid':123,'ticks':'old'}]})
   with mock.patch.object(runtime,'identity',return_value=None),mock.patch.object(runtime,'listening_socket_inodes',return_value={'123'}),mock.patch.object(runtime.time,'monotonic',side_effect=[0,20]),mock.patch.object(runtime.os,'kill') as kill:
    with self.assertRaisesRegex(RuntimeError,'STOP_UNCONFIRMED'):runtime.stop_runtime(root)
    kill.assert_not_called()
   self.assertFalse((root/'runtime-stopped.json').exists())
