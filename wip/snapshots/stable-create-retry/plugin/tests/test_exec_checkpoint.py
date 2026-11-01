"""Real ephemeral guest checkpoint program; synthetic files, no VM/model calls."""
import pathlib,sys,tempfile,json,uuid,subprocess,shutil,types,copy,unittest
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,exec_checkpoint as cp,activity,vm_interface
import test_guest_exec as fixture
NODE=shutil.which('node')
class Finish(unittest.TestCase):
 def fixture(self,d):
  a,s,row,owner=fixture.ExecTests().fixture(d);root=pathlib.Path(d);guest=root/'guest';guest.mkdir(mode=0o700);workspace=root/'workspace';workspace.mkdir();(workspace/'checks.txt').write_text('19 checks PASS\n')
  a.c['guest']={'workspace':str(workspace),'state_dir':str(guest),'max_artifact_bytes':1048576,'acpx_bin':'/never/run/acpx','node':'/never/run/node','launcher':'/never/run/launcher'};a.hash=w.digest(w.canonical(a.c));state=json.loads((a.local(s)/'session.json').read_text());state['config_hash']=a.hash;w.atomic(a.local(s)/'session.json',state)
  calls=[];identity={'machine_id':'1'*32,'boot_id':str(uuid.uuid4())}
  def nomad(args,**kw):
   if args[:2]==['alloc','exec']:
    self.assertIn('--input-type=module',args);program=args[-1];payload=json.loads(kw['input']);calls.append(payload['action']);program=program.replace('run(JSON.parse(raw))','run(JSON.parse(raw),()=>('+json.dumps(identity)+'))')
    return subprocess.run([NODE,'--input-type=module','-e',program],input=kw['input'],text=True,capture_output=True,timeout=10)
   if args[:2]==['job','allocs']:return subprocess.CompletedProcess(args,0,json.dumps([row]),'')
   if args[:2]==['job','stop']:
    calls.append('stop');self.assertTrue(a.result(s,None,a.load(s)['closing_candidate']['snapshot_id'])['checkpoint']['schema_version']==2);row.update(DesiredStatus='stop',ClientStatus='complete');row['TaskStates']['worker']['State']='dead';return subprocess.CompletedProcess(args,0,'','')
   raise AssertionError('Unexpected transport')
  a.nomad=nomad
  facade=vm_interface.VMs.__new__(vm_interface.VMs);facade.client=types.SimpleNamespace(root=a.root);facade.me={'agent_id':a.c['agent_instance_id'],'native_session_id':a.c['creator_session_id'],'generation':a.c['creator_generation']};facade.resolve=lambda *_:(a,{})
  return a,s,row,calls,identity,facade
 def test_zero_activity_zero_acp_session_finishes_with_typed_verified_snapshot(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);(a.local(s)/'session.json').unlink();out=facade.finish(s,[]);self.assertEqual(out['state'],'nomad_terminal');self.assertFalse(out['disk_deleted']);self.assertEqual(calls,['probe','prepare_finish','close','finish_ack','stop']);self.assertFalse(list((pathlib.Path(a.c['guest']['state_dir'])/s).glob('turn-*.json')))
   plan=json.loads((a.root/'vm-interface/finishes'/(s+'.json')).read_text());self.assertIsNone(plan['request_id']);m=a.result(s,None,plan['snapshot_id']);self.assertEqual(m['work_kind'],'exec_only');self.assertIsNone(m['acpx_request_id']);self.assertIsNone(m['terminal']);self.assertEqual(m['checkpoint']['machine_identity'],identity)
 def test_verified_aborted_upload_allows_finish_without_dummy_turn(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);identifier=str(uuid.uuid4());state=a.load(s);binding={'session_id':s,'attempt':state['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
   w.atomic(a.local(s)/('upload-'+identifier+'.json'),{'operation_id':identifier,'work_kind':'upload','phase':'upload_aborted','activity_revision':1})
   folder=pathlib.Path(a.c['guest']['state_dir'])/s/'uploads';folder.mkdir(mode=0o700,parents=True);folder.parent.chmod(0o700);w.atomic(folder/(identifier+'.json'),{'phase':'aborted','binding':{'binding':binding,'extract_to':None}})
   out=facade.finish(s,[]);self.assertTrue(out['results_verified']);self.assertFalse(out['disk_deleted']);self.assertNotIn('submit',calls)
 def test_exec_only_exports_files_without_model_turn(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);w.atomic(a.local(s)/('exec-'+str(uuid.uuid4())+'.json'),{'execution_id':str(uuid.uuid4()),'phase':'exec_completed','activity_revision':1});out=facade.finish(s,['checks.txt']);self.assertTrue(out['results_verified']);self.assertIn('artifact',calls);self.assertNotIn('submit',calls);self.assertNotIn('create',calls)
 def test_unknown_legacy_exec_blocks_all_finish_effects(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);w.atomic(a.local(s)/'exec-unknown.json',{'execution_id':str(uuid.uuid4()),'phase':'exec_unknown'});self.assertRaises(RuntimeError,facade.finish,s,[]);self.assertEqual(calls,[])
 def test_pending_agent_turn_blocks_management_checkpoint(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);r=str(uuid.uuid4());w.atomic(a.local(s)/(r+'.json'),{'session_id':s,'request_id':r,'phase':'queued','prompt_sha256':'a'*64});self.assertRaises(RuntimeError,facade.finish,s,[]);self.assertEqual(calls,[])
 def test_foreign_creator_not_authorized_even_same_project_label(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);row['Job']['Meta'].update(project='same-project',creator_agent_id=str(uuid.uuid4()));self.assertRaisesRegex(RuntimeError,'OWNER_CHANGED',facade.finish,s,[]);self.assertEqual(calls,[])
 def test_foreign_generation_rejected_before_any_effect(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);record=json.loads(pathlib.Path(a.c['creator_record']).read_text());record['generation']=str(uuid.uuid4());w.atomic(a.c['creator_record'],record);self.assertRaises(RuntimeError,facade.finish,s,[]);self.assertEqual(calls,[])
 def test_changed_activity_revision_rejects_old_snapshot(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);snap=str(uuid.uuid4());a.collect(s,None,['checks.txt'],snap);state=a.load(s);state['activity_revision']=1;w.atomic(a.local(s)/'session.json',state);self.assertRaisesRegex(RuntimeError,'STALE',a.finish,s,None,snap);self.assertNotIn('prepare_finish',calls);self.assertNotIn('stop',calls)
 def test_boot_changed_before_finish_retains_vm(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);snap=str(uuid.uuid4());a.collect(s,None,['checks.txt'],snap);identity['boot_id']=str(uuid.uuid4());self.assertRaisesRegex(RuntimeError,'MACHINE_BOOT_CHANGED',a.finish,s,None,snap);self.assertNotIn('stop',calls)
 def test_guest_turn_missing_locally_is_not_ignored(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);folder=pathlib.Path(a.c['guest']['state_dir'])/s;folder.mkdir(mode=0o700);w.atomic(folder/('turn-'+str(uuid.uuid4())+'.json'),{'phase':'terminal'});self.assertRaisesRegex(RuntimeError,'AGENT_TURN_PRESENT',facade.finish,s,[]);self.assertNotIn('stop',calls)
 def test_guest_operation_missing_locally_blocks_revision_guess(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);folder=pathlib.Path(a.c['guest']['state_dir'])/s/'operations'/str(uuid.uuid4());folder.mkdir(parents=True,mode=0o700);folder.parent.chmod(0o700);folder.parent.parent.chmod(0o700);w.atomic(folder/'state.json',{'phase':'completed','binding':{'foreign':'binding'}});self.assertRaises(RuntimeError,facade.finish,s,[]);self.assertNotIn('stop',calls)
 def test_failed_export_keeps_vm_without_close(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d)
   with patch.object(a,'_collect_locked',side_effect=RuntimeError('synthetic transfer failure')):self.assertRaises(RuntimeError,facade.finish,s,['checks.txt'])
   self.assertNotIn('prepare_finish',calls);self.assertNotIn('close',calls);self.assertNotIn('stop',calls)
 def test_verified_s3_management_snapshot_accepts_null_model_request(self):
  import storage
  from test_catalog_lifecycle import MemoryStore
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);a.c.update(storage={'backend':'s3'},diagnostics_required=False);a.hash=w.digest(w.canonical(a.c));state=json.loads((a.local(s)/'session.json').read_text());state['config_hash']=a.hash;w.atomic(a.local(s)/'session.json',state);store=MemoryStore()
   with patch.object(storage,'store',return_value=(store,storage.binding(store.cfg))):out=facade.finish(s,[])
   self.assertTrue(out['results_verified']);self.assertFalse(out['disk_deleted']);self.assertIn('stop',calls)
 def test_project_label_consistency_is_not_an_authorization_grant(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);w.atomic(a.local(s)/'allocation-intent.json',{'project':'owned-project'});row['Job']['Meta']['project']='other-project';self.assertRaisesRegex(RuntimeError,'PROJECT_BINDING_CHANGED',facade.finish,s,[]);self.assertEqual(calls,[])
 def test_terminal_agent_turn_does_not_become_exec_checkpoint(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);r=str(uuid.uuid4());w.atomic(a.local(s)/(r+'.json'),{'session_id':s,'request_id':r,'phase':'terminal','prompt_sha256':'a'*64});self.assertRaisesRegex(RuntimeError,'AGENT_TURN_PRESENT',a.collect,s,None,[],str(uuid.uuid4()));self.assertEqual(calls,[])
 def test_nineteen_managed_checks_need_no_model_turn(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);binding={'session_id':s,'attempt':a.load(s)['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
   for number in range(19):
    op=str(uuid.uuid4());record={'operation_id':op,'binding':binding,'phase':'completed','activity_revision':number+1};w.atomic(a.local(s)/('operation-'+op+'.json'),record);folder=pathlib.Path(a.c['guest']['state_dir'])/s/'operations'/op;folder.mkdir(parents=True,mode=0o700);folder.parent.chmod(0o700);folder.parent.parent.chmod(0o700);w.atomic(folder/'state.json',record)
   with patch('operations.verify_terminal')as verified:out=facade.finish(s,['checks.txt'])
   self.assertTrue(out['results_verified']);self.assertGreaterEqual(verified.call_count,2);self.assertNotIn('submit',calls)
 def test_no_acp_binary_needed_even_for_lifecycle_events(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,calls,identity,facade=self.fixture(d);(a.local(s)/'session.json').unlink();a.event=lambda session,stage,r=None:list(a.remote(session,'lifecycle',stage=stage))
   out=facade.finish(s,[]);self.assertEqual(out['state'],'nomad_terminal');self.assertIn('lifecycle',calls);self.assertFalse(pathlib.Path('/never/run/acpx').exists())
if __name__=='__main__':unittest.main()
