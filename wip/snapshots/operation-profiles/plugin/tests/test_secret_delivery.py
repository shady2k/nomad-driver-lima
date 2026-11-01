import pathlib,sys,tempfile,json,types,os,subprocess,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations
from test_secret_profiles import fixture

class SecretDeliveryTests(unittest.TestCase):
 def module(self,root):
  m=types.ModuleType('synthetic_secret_delivery');exec(operations.program(),m.__dict__)
  exec((pathlib.Path(__file__).parents[1]/'scripts/secret_profile_delivery.py').read_text(),m.__dict__)
  m.secret_root=lambda session:root;m.secret_kernel_identity=lambda:{'machine_id':'fixture-machine','boot_id':'fixture-boot'}
  def proc(pid):
   try:os.kill(pid,0)
   except ProcessLookupError:return None
   return {'pid':pid,'ticks':str(pid),'uid':os.getuid(),'state':'R'}
  if sys.platform!='linux':m.process=proc
  m.descendants=m.secret_descendants
  return m
 def payload(self):
  return {'catalogue':fixture(),'binding':{'session_id':'00000000-0000-4000-8000-000000000001','attempt':{'allocation':'alloc','task':'worker'},'creator_agent_id':'agent','creator_session_id':'native','creator_generation':'generation'},'machine_identity':{'machine_id':'fixture-machine','boot_id':'fixture-boot'},'namespace':'workers','task':'worker','address':'https://secrets.example.invalid','bao_bin':'/opt/bao'}
 def test_real_delivery_filtered_immutable_idempotent_and_no_token_reads(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);root.chmod(0o700);m=self.module(root);p=self.payload()
   result=m.deliver_secret_profiles(p);self.assertTrue(result['enabled']);self.assertEqual(m.deliver_secret_profiles(p),result)
   self.assertFalse((root/'vault_token').exists());self.assertEqual((root/'secret-profiles.json').stat().st_mode&0o777,0o600)
   p['binding']['creator_generation']='different'
   with self.assertRaisesRegex(RuntimeError,'NO_REBIND'):m.deliver_secret_profiles(p)
 def test_wrong_guest_binding_symlink_and_finalization_fence(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);root.chmod(0o700);m=self.module(root);p=self.payload();p['machine_identity']['boot_id']='other'
   with self.assertRaisesRegex(RuntimeError,'MACHINE_CHANGED'):m.deliver_secret_profiles(p)
   p=self.payload();target=root/'outside';target.write_text('{}');target.chmod(0o600);(root/'secret-profiles.json').symlink_to(target)
   with self.assertRaises(OSError):m.deliver_secret_profiles(p)
   (root/'secret-profiles.json').unlink();m.secret_atomic(root/'secret-process.sealed',{'sealed':True})
   with self.assertRaisesRegex(RuntimeError,'FINALIZED'):m.deliver_secret_profiles(p)
 def test_finalizer_requests_owned_runner_completion_and_fences_new_starts(self):
  import test_secret_operations as fixture,uuid,time
  with tempfile.TemporaryDirectory()as d:
   test=fixture.SecretOperationTests();base,a,session,gs,calls,code,root,delivery=test.fixture(d);op=str(uuid.uuid4())
   test.start(a,session,op,'prime','import time;time.sleep(20)',code,timeout=25)
   m=self.module(root)
   deadline=time.monotonic()+2
   while not(root/'secret-operations'/op/'owner.json').exists()and time.monotonic()<deadline:time.sleep(.02)
   import types
   def native_identity(pid):
    row=m.process(pid)
    return {k:row[k]for k in ('pid','ticks')}if row else None
   native=types.SimpleNamespace(identity=native_identity,descendants=m.secret_descendants)
   owner=m.secret_read(root/'secret-operations'/op/'owner.json')
   self.assertNotEqual(native_identity(owner['runner']['pid']),owner['runner']) # actual task-main exact-dict contract
   self.assertTrue(m.secret_module_alive(owner['runner'],native))
   self.assertTrue(m.secret_stop_all(root,native));self.assertTrue((root/'secret-process.sealed').exists())
   row=base.done(a,session,op);self.assertEqual(row['phase'],'cancelled')
   proof=json.loads((root/'secret-completed'/(op+'.json')).read_text());self.assertTrue(proof['cleanup_complete']);self.assertEqual(proof['version'],2)
   self.assertEqual(list((root/'secret-operations').iterdir()),[])
   self.assertTrue(m.secret_stop_all(root,m)) # completion is idempotent
   retired=root/'secret-operations'/('.closed-'+op+'-'+proof['registry_id']);retired.mkdir(mode=0o700);(retired/'partial-cache').write_text('retained until token cleanup')
   self.assertTrue(m.secret_stop_all(root,m));self.assertTrue(retired.exists()) # process proof is distinct from tmpfs-file erasure
 def test_missing_owner_and_lost_runner_fail_closed_without_signalling_unrelated(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);root.chmod(0o700);m=self.module(root);p=self.payload();m.deliver_secret_profiles(p)
   parent=root/'secret-operations';parent.mkdir(mode=0o700);operation='00000000-0000-4000-8000-000000000002';op=parent/operation;op.mkdir(mode=0o700)
   with self.assertRaisesRegex(RuntimeError,'START_OUTCOME_UNKNOWN'):m.secret_stop_all(root,m)
   other=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'])
   try:
    row=m.process(other.pid);row['ticks']='99999999999999'
    m.secret_atomic(op/'owner.json',{'version':2,'phase':'active','registry_id':'a'*32,'operation_id':operation,'runner':row,'processes':[row],'binding':p['binding'],'machine_identity':p['machine_identity']})
    with self.assertRaisesRegex(RuntimeError,'RUNNER_LOST_CLEANUP_UNCONFIRMED'):m.secret_stop_all(root,m)
    self.assertIsNone(other.poll())
   finally:other.terminate();other.wait()
 def test_process_identity_survives_uid_change_and_native_row_shape(self):
  with tempfile.TemporaryDirectory()as d:
   m=self.module(pathlib.Path(d));row={'pid':123,'ticks':'456','uid':os.getuid(),'state':'R'}
   m.process=lambda pid:{**row,'uid':os.getuid()+1}
   self.assertTrue(m.secret_alive(row)) # setuid does not make an owned process disappear
   native=types.SimpleNamespace(identity=lambda pid:{'pid':pid,'ticks':'456'})
   self.assertTrue(m.secret_module_alive(row,native))
   native.identity=lambda pid:{'pid':pid,'ticks':'457'}
   self.assertFalse(m.secret_module_alive(row,native))
 def test_stale_completion_cannot_close_active_registration(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);root.chmod(0o700);m=self.module(root);p=self.payload();m.deliver_secret_profiles(p)
   parent=root/'secret-operations';parent.mkdir(mode=0o700);operation='00000000-0000-4000-8000-000000000002';op=parent/operation;op.mkdir(mode=0o700)
   other=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'])
   try:
    owner={'version':2,'phase':'active','registry_id':'a'*32,'operation_id':operation,'runner':m.process(other.pid),'processes':[],'binding':p['binding'],'machine_identity':p['machine_identity']};m.secret_atomic(op/'owner.json',owner)
    proof={**owner,'registry_id':'b'*32,'cleanup_complete':True,'scope':'process-tree','completed_at':1,'stopped_processes':[]};m.secret_atomic(m.secret_completion_dir(root)/(operation+'.json'),proof)
    with self.assertRaisesRegex(RuntimeError,'COMPLETION_BINDING_CHANGED'):m.secret_stop_all(root,m)
    self.assertIsNone(other.poll())
   finally:other.terminate();other.wait()
if __name__=='__main__':unittest.main()
