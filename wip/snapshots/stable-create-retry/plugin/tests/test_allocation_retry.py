"""No network/model/VM effects: deterministic registration fixtures."""
import sys,pathlib,tempfile,unittest,json,uuid,hashlib,types,threading,copy
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'scripts'))
import allocation_retry as ar,catalog_cli as cat,vm_interface as v
import test_vm_interface as fixture
write=fixture.write

class Registration(unittest.TestCase):
 def fixture(self,d):
  root=pathlib.Path(d);session=str(uuid.uuid4());owner=str(uuid.uuid4());agent=str(uuid.uuid4());generation=str(uuid.uuid4());native='native';record=write(root/'creator.json',{'agent_instance_id':agent,'generation':generation,'native_session_id':native,'ended':False})
  config=write(root/'config.json',{'receipt_root':str(root/'results'),'owner_id':owner,'namespace':'workers'});c=cat.CatalogClient(config);job={'ID':'worker-'+session,'Namespace':'workers','Meta':{'nomad_workers_owner':owner,'worker_session_id':session,'creator_agent_id':agent,'creator_session_id':native,'creator_generation':generation},'TaskGroups':[{'Name':'worker','Tasks':[{'Name':'worker','Driver':'lima','Config':{'worker_session_id':session},'Resources':{'CPU':2000,'MemoryMB':2176}}]}]}
  intent={'session_id':session,'job':job['ID'],'phase':'submit_intent','registration_stage':'prepared_not_dispatched','creator_record':str(record),'creator_agent_id':agent,'creator_session_id':native,'creator_generation':generation,'job_spec':job,'job_spec_sha256':hashlib.sha256(cat.canonical(job)).hexdigest()};write(c.root/session/'allocation-intent.json',intent)
  state={'job':None,'registers':0,'lost':False};lock=threading.Lock()
  def run(args,input=None):
   if args[:2]==['operator','api']:
    return types.SimpleNamespace(returncode=0,stdout=b'job not found'if state['job']is None else cat.canonical(state['job']))
   self.assertIn('-check-index=0',args)
   with lock:
    state['registers']+=1
    if state['job']is None:state['job']=json.loads(input)['Job'];return types.SimpleNamespace(returncode=1 if state['lost']else 0)
    return types.SimpleNamespace(returncode=1)
  c.run=run;c.api=lambda path:[];return c,session,state,job,intent
 def test_lost_register_ack_reconciles_existing_same_job(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,_=self.fixture(d);state['lost']=True;first=c.registration_status(s,True);second=c.registration_status(s,True);self.assertEqual(first['registration'],'accepted');self.assertEqual(second['registration'],'accepted');self.assertEqual(state['registers'],1)
 def test_concurrent_retry_registers_once(self):
  from concurrent.futures import ThreadPoolExecutor
  with tempfile.TemporaryDirectory()as d:
   c,s,state,*_=self.fixture(d)
   with ThreadPoolExecutor(max_workers=8)as pool:results=list(pool.map(lambda _:c.registration_status(s,True),range(24)))
   self.assertTrue(all(r['registration']=='accepted'for r in results));self.assertEqual(state['registers'],1)
 def test_accepted_lost_ack_failed_lookup_then_gc_never_registers_twice(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,_=self.fixture(d);original=c.run;lookups=0
   def transport(args,input=None):
    nonlocal lookups
    if args[:2]==['operator','api']:
     lookups+=1
     if lookups==2:return types.SimpleNamespace(returncode=1,stdout=b'')
     return original(args,input)
    original(args,input);return types.SimpleNamespace(returncode=1)
   c.run=transport
   self.assertRaisesRegex(RuntimeError,'LOOKUP_UNAVAILABLE',c.registration_status,s,True)
   self.assertEqual(state['registers'],1);state['job']=None
   out=c.registration_status(s,True);self.assertFalse(out['automatic_registration_allowed']);self.assertEqual(state['registers'],1);self.assertEqual(out['state'],'job_absent_dispatch_not_excludable_lookup_only')
 def test_crash_after_dispatch_then_gc_no_recreate(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);original=c.run
   def crash(args,input=None):
    if args[:2]==['operator','api']:return original(args,input)
    original(args,input);raise SystemExit('simulated process crash after server acceptance')
   c.run=crash;self.assertRaises(SystemExit,c.registration_status,s,True);self.assertEqual(state['registers'],1);state['job']=None;c.run=original
   out=c.registration_status(s,True);self.assertFalse(out['automatic_registration_allowed']);self.assertEqual(state['registers'],1)
 def test_dispatch_marker_even_before_network_is_lookup_only(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);intent['registration_stage']='dispatch_started';write(c.root/s/'allocation-intent.json',intent)
   self.assertFalse(c.registration_status(s,True)['automatic_registration_allowed']);self.assertEqual(state['registers'],0)
 def test_legacy_submit_unknown_404_is_not_proof_never_dispatched(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);intent.pop('registration_stage');intent['phase']='submit_unknown';write(c.root/s/'allocation-intent.json',intent)
   self.assertFalse(c.registration_status(s,True)['automatic_registration_allowed']);self.assertEqual(state['registers'],0)
 def test_dispatch_fsync_failure_happens_before_register(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,*_=self.fixture(d)
   with patch('allocation_retry.os.fsync',side_effect=OSError('synthetic durability failure')):self.assertRaises(OSError,c.registration_status,s,True)
   self.assertEqual(state['registers'],0)
 def test_real_producer_freezes_job_and_uses_create_only_cas(self):
  import test_catalog_lifecycle as lifecycle
  with tempfile.TemporaryDirectory()as d:
   c,s=lifecycle.CatalogTests().client(d);c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000}]};calls=[]
   def run(args,input=None):
    self.assertEqual(json.loads((c.root/s/'allocation-intent.json').read_text())['registration_stage'],'dispatch_started');calls.append((args,input));return types.SimpleNamespace(returncode=0)
   c.run=run;c.request_status=lambda _: {'lookup_only':True}
   c.request(s,'agent-base','v1','amd64',1,1024,20,'tests',task_title='Verify original frozen allocation registration',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
   args,payload=calls[0];self.assertIn('-check-index=0',args);intent=json.loads((c.root/s/'allocation-intent.json').read_text());self.assertEqual(intent['job_spec'],json.loads(payload)['Job']);self.assertEqual(intent['job_spec_sha256'],hashlib.sha256(cat.canonical(intent['job_spec'])).hexdigest())
 def test_absent_readonly_status_never_registers(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,*_=self.fixture(d);self.assertEqual(c.registration_status(s)['registration'],'absent');self.assertEqual(state['registers'],0)
 def test_existing_foreign_payload_rejected_no_overwrite(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,_=self.fixture(d);state['job']=copy.deepcopy(job);state['job']['TaskGroups'][0]['Tasks'][0]['Resources']['CPU']=9000
   self.assertRaisesRegex(RuntimeError,'PAYLOAD_CONFLICT',c.registration_status,s,True);self.assertEqual(state['registers'],0)
 def test_ended_creator_no_mutation(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);p=pathlib.Path(intent['creator_record']);record=json.loads(p.read_text());record['ended']=True;write(p,record);self.assertRaisesRegex(RuntimeError,'CREATOR_CHANGED',c.registration_status,s,True);self.assertEqual(state['registers'],0)
 def test_previously_accepted_job_absence_never_recreates(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);intent['phase']='submitted';write(c.root/s/'allocation-intent.json',intent);out=c.registration_status(s,True);self.assertEqual(out['state'],'previously_accepted_job_absent_no_recreate');self.assertEqual(state['registers'],0)
 def test_transport_invalid_json_is_not_absence(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,*_=self.fixture(d)
   for code,body in [(1,b'job not found'),(0,b''),(0,b'Permission denied'),(0,b'{}')]:
    c.run=lambda *a,**k:types.SimpleNamespace(returncode=code,stdout=body)
    self.assertRaises(RuntimeError,c.registration_status,s,True)
   self.assertEqual(state['registers'],0)
 def test_legacy_missing_spec_not_blindly_replayed(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,intent=self.fixture(d);intent.pop('job_spec');write(c.root/s/'allocation-intent.json',intent);out=c.registration_status(s,True);self.assertIn('legacy_intent',out['state']);self.assertEqual(state['registers'],0)
 def test_evaluation_blocker_surfaces_without_re_register(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,_=self.fixture(d);state['job']=job;c.api=lambda path:[{'ID':'evaluation','Status':'blocked','StatusDescription':'waiting for resources','FailedTGAllocs':{'worker':{'NodesEvaluated':2,'NodesExhausted':2,'DimensionExhausted':{'cpu':2}}}}]
   out=c.registration_status(s);self.assertEqual(out['evaluations'][0]['status'],'blocked');self.assertEqual(out['evaluations'][0]['failed_task_groups'][0]['dimension_exhausted'],{'cpu':2});self.assertEqual(state['registers'],0)
 def test_unavailable_evaluations_not_reported_as_no_blockers(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,state,job,_=self.fixture(d);state['job']=job;c.api=lambda path:(_ for _ in ()).throw(RuntimeError('unavailable'));self.assertEqual(c.registration_status(s)['evaluation_observation'],'unavailable_not_empty')

class Facade(unittest.TestCase):
 def setUp(self):self.f=fixture.Tests();self.f.setUp();self.addCleanup(self.f.doCleanups)
 def test_validation_rejection_exact_error_not_unknown(self):
  f=self.f
  with patch.object(f.client,'request',side_effect=ValueError('PROJECT_PURPOSE_SLUG_REQUIRED: no spaces')):
   one=f.vm.create({**f.params(),'purpose':'environment preparation'});two=f.vm.create({**f.params(),'purpose':'environment preparation'})
  self.assertEqual(one['state'],'validation_rejected');self.assertEqual(one['registration'],'not_attempted');self.assertIn('SLUG',one['error']);self.assertEqual(one['vm_id'],two['vm_id'])
 def test_capacity_preflight_reason_is_not_hidden_as_unknown(self):
  f=self.f
  with patch.object(f.client,'request',side_effect=RuntimeError('NO_PINNED_CATALOG_SOURCE_WITH_CURRENT_FIT')):out=f.vm.create(f.params())
  self.assertEqual(out['state'],'not_dispatched');self.assertEqual(out['error'],'NO_PINNED_CATALOG_SOURCE_WITH_CURRENT_FIT');self.assertEqual(out['registration'],'not_attempted')
 def test_existing_lifemodel_shape_reconciles_absent_without_writing(self):
  f=self.f
  with patch.object(f.client,'request',side_effect=RuntimeError('lost')):out=f.vm.create(f.params())
  path,row=f.vm.create_intent(out['vm_id']);row.pop('parameters');row.pop('never_dispatched_verified',None);row['phase']='create_unknown';write(path,row);before=path.read_bytes()
  with patch('allocation_retry.lookup',return_value=None):status=f.vm.status(out['vm_id'])
  self.assertEqual(status['state'],'legacy_or_post_dispatch_absent_lookup_only');self.assertEqual(path.read_bytes(),before)
 def test_same_id_changed_input_rejected_even_when_no_job(self):
  f=self.f;out=f.vm.create(f.params());self.assertRaisesRegex(RuntimeError,'PAYLOAD_CHANGED',f.vm.create,{**f.params(),'disk_gib':9},out['vm_id'])
 def test_new_native_tool_can_lookup_original_vm_id(self):
  f=self.f;out=f.vm.create(f.params());f.vm.native['native_tool_use_id']='new-call'
  with patch('allocation_retry.lookup',return_value=None),patch.object(f.client,'request')as request:
   result=f.vm.create(f.params(),out['vm_id']);self.assertEqual(result['vm_id'],out['vm_id']);self.assertEqual(request.call_count,0)
 def test_accepted_frontdoor_missing_local_intent_never_recreates(self):
  f=self.f;out=f.vm.create(f.params());(f.client.root/out['vm_id']/'allocation-intent.json').unlink()
  with patch('allocation_retry.lookup',return_value=None),patch.object(f.client,'request')as request:result=f.vm.retry(out['vm_id'],True)
  request.assert_not_called();self.assertIn('no_recreate',result['state'])
 def test_rejected_same_id_does_not_silently_correct_payload(self):
  f=self.f
  with patch.object(f.client,'request',side_effect=ValueError('PROJECT_PURPOSE_SLUG_REQUIRED')):out=f.vm.create({**f.params(),'purpose':'has space'})
  self.assertRaisesRegex(RuntimeError,'PAYLOAD_CHANGED',f.vm.create,{**f.params(),'purpose':'has-space'},out['vm_id'])

class Capacity(unittest.TestCase):
 def test_aliases_preserve_unknown_and_disk_units(self):
  from capacity_view import describe
  nodes=[{'available_for_one_new_vm':{'vcpus':None,'guest_memory_mib':None,'disk_gib':None,'additional_vm_slots':None},'occupied':{'driver':{'observed_unix':123,'physical_available_mib':999,'disk_floor_mib':10}}}];row=describe(nodes)[0];self.assertIsNone(row['effective_available']['vcpus']);self.assertEqual(row['observed_unix'],123);self.assertEqual(row['physical_disk']['available_mib'],999);self.assertEqual(row['physical_disk']['meaning'],'filesystem_free_space_not_ram')
 def test_denied_node_details_keep_unknown_aliases(self):
  from capacity_view import describe
  row=describe([{'available_for_one_new_vm':None,'occupied':None}])[0];self.assertIsNone(row['effective_available']['vcpus']);self.assertIsNone(row['observed_unix'])
if __name__=='__main__':unittest.main()
