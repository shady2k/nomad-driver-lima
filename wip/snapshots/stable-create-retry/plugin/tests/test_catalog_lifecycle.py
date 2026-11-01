import importlib.util,json,pathlib,sys,tempfile,unittest,uuid
from unittest import mock
SCRIPTS=pathlib.Path(__file__).parents[1]/'scripts';sys.path.insert(0,str(SCRIPTS))
import catalog_cli,lease,storage,exchange
spec=importlib.util.spec_from_file_location('new_workerctl',SCRIPTS/'workerctl.py');w=importlib.util.module_from_spec(spec);spec.loader.exec_module(w)
class CatalogTests(unittest.TestCase):
 def client(self,d):
  s=str(uuid.uuid4());e={'template_id':'agent-base','version':'v1','architecture':'amd64','vm_type':'qemu','guest_os':'linux','disk':{'sha256':'a'*64,'minimum_disk_gib':20,'url':'https://invalid/?secret=not-disclosed'},'acceptance':{'sealed':True,'independent_boots':2,'unique_identities':True},'tools':{'prime':'0.10.1'}};p=pathlib.Path(d)/'catalog';raw=json.dumps({'schema_version':1,'templates':[e]}).encode();p.write_bytes(raw);cfg={'catalog_file':str(p),'catalog_sha256':w.digest(raw),'receipt_root':d,'owner_id':str(uuid.uuid4()),'agent_instance_id':str(uuid.uuid4()),'memory_overhead_mib':128,'guest_bootstrap_command':['/bin/true'],'namespace':'agent-workers','datacenters':['dc1'],'task':'worker'};c=pathlib.Path(d)/'config';c.write_text(json.dumps(cfg));return catalog_cli.CatalogClient(c),s
 def test_lost_create_response_not_resubmitted_and_resources_not_nodepin(self):
  with tempfile.TemporaryDirectory() as d:
   c,s=self.client(d);c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1999},{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':2500}]};calls=[]
   def run(args,input=None):calls.append(json.loads(input));return type('R',(),{'returncode':1})()
   c.run=run;c.request_status=lambda sid:{'lookup_only':True}
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN'):c.request(s,'agent-base','v1','amd64',2,1024,20,'tests','sample',task_title='Validate isolated fixture environment',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
   self.assertTrue(c.request(s,'agent-base','v1','amd64',2,1024,20,'tests','sample',task_title='Validate isolated fixture environment',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')['lookup_only']);self.assertEqual(len(calls),1)
   job=calls[0]['Job'];self.assertEqual(job['TaskGroups'][0]['Tasks'][0]['Resources'],{'CPU':5000,'MemoryMB':1152});self.assertFalse(any('node.unique.id' in x['LTarget'] for x in job['Constraints']));self.assertNotIn('not-disclosed',json.dumps(c.templates()))
   with self.assertRaisesRegex(ValueError,'CHANGED'):c.request(s,'agent-base','v1','amd64',1,1024,20,'tests','sample',task_title='Validate isolated fixture environment',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
 def test_noninteractive_api_explicit_get_and_invalid_success_body_no_intent(self):
  with tempfile.TemporaryDirectory() as d:
   c,s=self.client(d)
   completed=type('R',(),{'returncode':0,'stdout':b'[]'})()
   with mock.patch.object(catalog_cli.subprocess,'run',return_value=completed) as run:
    self.assertEqual(c.api('/v1/nodes?resources=true'),[])
    run.assert_called_once_with([c.bin,'operator','api','-X','GET','/v1/nodes?resources=true'],input=None,capture_output=True)
   completed.stdout=b'Invalid method'
   with mock.patch.object(catalog_cli.subprocess,'run',return_value=completed) as run:
    for _ in range(2):
     with self.assertRaisesRegex(RuntimeError,'NOMAD_READ_INVALID_JSON'):
      c.request(s,'agent-base','v1','amd64',1,1024,20,'tests',task_title='Validate isolated fixture environment',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
    self.assertEqual(run.call_count,2)
    self.assertTrue(all(call.args[0][1:5]==['operator','api','-X','GET'] for call in run.call_args_list))
   self.assertFalse((pathlib.Path(d)/s/'allocation-intent.json').exists())
 def test_vault_job_is_file_only_and_task_scoped(self):
  with tempfile.TemporaryDirectory() as d:
   c,s=self.client(d);c.c['vault']={'role':'agent-worker-prime'};c.c['guest']={'prime_vault':{'synthetic':True}}
   c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000}]};jobs=[]
   def run(args,input=None):jobs.append(json.loads(input));return type('R',(),{'returncode':0})()
   c.run=run;c.request_status=lambda sid:{'lookup_only':True};c.request(s,'agent-base','v1','amd64',1,1024,20,'tests',task_title='Validate isolated fixture environment',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
   task=jobs[0]['Job']['TaskGroups'][0]['Tasks'][0]
   self.assertEqual(task['Vault'],{'Role':'agent-worker-prime','Env':False,'DisableFile':False,'ChangeMode':'noop'})
   self.assertTrue(task['Config']['vault_required']);self.assertNotIn('Env',task)
 def test_changed_catalog_and_no_unapproved_entry(self):
  with tempfile.TemporaryDirectory() as d:
   c,s=self.client(d);pathlib.Path(c.c['catalog_file']).write_text('{}')
   with self.assertRaisesRegex(ValueError,'CHECKSUM'):c.templates()
class LeaseTests(unittest.TestCase):
 def test_replay_forged_owner_generation_and_idle_independent_of_absence(self):
  with tempfile.TemporaryDirectory() as root:
   d=lease.create(root,'agent','session',{'alloc':'a'},now=10);a=lease.observe(root,'agent','session',{'alloc':'a'},d['generation'],1,'request',now=20);b=lease.observe(root,'agent','session',{'alloc':'a'},d['generation'],2,'request',now=100);self.assertEqual(a['expires_at'],b['expires_at'])
   for owner,gen,attempt in [('forged',d['generation'],{'alloc':'a'}),('agent','old',{'alloc':'a'}),('agent',d['generation'],{'alloc':'stale'})]:
    with self.assertRaises(RuntimeError):lease.observe(root,owner,'session',attempt,gen,2,'new',now=100)
   r=lease.report(root,active=True,now=4000);self.assertEqual(r['owner_state'],'owner_unconfirmed');self.assertTrue(r['orphan_suspect']);self.assertEqual(r['idle_state'],'active');self.assertFalse(r['auto_delete_enabled'])
class MemoryStore(exchange.Store):
 def __init__(self):super().__init__({'endpoint':'https://example.invalid','bucket':'synthetic','region':'test'},key=('synthetic','synthetic'));self.objects={};self.fail=False
 def put(self,key,raw):
  if self.fail:raise RuntimeError('storage unavailable')
  self.objects[key]=raw
 def get(self,key,limit=4194304):return self.objects[key]
 def verify(self,key,size,sha):
  raw=self.objects[key]
  if len(raw)!=size or w.digest(raw)!=sha:raise RuntimeError('hash mismatch')
class StorageTests(unittest.TestCase):
 def fixture(self,d,settings=None):
  session=str(uuid.uuid4());rid=str(uuid.uuid4());cfg={'version':1,'allocation':str(uuid.uuid4()),'owner_id':str(uuid.uuid4()),'receipt_root':d,'job':'worker-'+str(uuid.uuid4()),'task':'worker'}
  if settings is not None:cfg['storage']=settings;cfg['diagnostics_required']=False # legacy unit fixture; full diagnostic gate covered separately
  p=pathlib.Path(d)/'config';p.write_text(json.dumps(cfg));a=w.Adapter(p);w.atomic(a.local(session)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':{'allocation':cfg['allocation']}});w.atomic(a.local(session)/(rid+'.json'),{'phase':'terminal','request_id':rid,'session_id':session,'acpx_request_id':rid,'terminal':{'stopReason':'end_turn'}})
  import base64
  a.remote=lambda *args,**kwargs:iter([{'data':base64.b64encode(b'output').decode(),'path':'result.txt','size':6,'sha256':w.digest(b'output')}]);return a,session,rid
 def test_dead_allocation_requires_current_task_cleanup_events(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r=self.fixture(d);a.c['vault']={'role':'agent-worker-prime'}
   row={'DesiredStatus':'stop','ClientStatus':'complete','TaskStates':{'worker':{'Events':[]}}}
   self.assertFalse(a._cleanup_confirmed(row))
   row['TaskStates']['worker']['Events']=[{'DriverMessage':'lima task_token_revoked'}];self.assertFalse(a._cleanup_confirmed(row))
   row['TaskStates']['worker']['Events'].append({'DriverMessage':'lima vm_stopped'});self.assertTrue(a._cleanup_confirmed(row))
 def test_optional_off_does_not_import_secret_reader_full_collect(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r=self.fixture(d)
   with mock.patch.dict(sys.modules,{'storage':None,'exchange':None}):self.assertEqual(a.collect(s,r,['result.txt'])['actual_backend'],'coordinator-local');self.assertEqual(a.result(s,r)['actual_backend'],'coordinator-local')
 def test_s3_failure_retains_and_retry_transfer_only_bound_receipt(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r=self.fixture(d,{'backend':'s3'});store=MemoryStore();backend=storage.binding(store.cfg);store.fail=True
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    with self.assertRaisesRegex(RuntimeError,'VM_RETAINED'):a.collect(s,r,['result.txt'])
    state=json.loads((a.local(s)/(r+'.json')).read_text());self.assertFalse(state['export_verified']);self.assertTrue((a.local(s)/state['manifest_dir']/'0.bin').exists());store.fail=False
    manifest=a.collect(s,r,['result.txt']);self.assertEqual(manifest['actual_backend'],'s3');self.assertEqual(a.result(s,r)['actual_backend'],'s3')
    wrong=dict(manifest['export_binding'],allocation='other')
    with self.assertRaisesRegex(RuntimeError,'ATTEMPT'):storage.verify({},manifest['receipt'],backend,reader=store,attempt_binding=wrong)
    with self.assertRaisesRegex(RuntimeError,'BACKEND'):storage.verify({},manifest['receipt'],'wrong-backend',reader=store,attempt_binding=manifest['export_binding'])
if __name__=='__main__':unittest.main()
