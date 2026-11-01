import json,pathlib,sys,tempfile,unittest,uuid,subprocess
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import placement,catalog_cli
import test_catalog_lifecycle as lifecycle
class AutoPlacementTests(unittest.TestCase):
 def fixture(self,d):
  c,s=lifecycle.CatalogTests().client(d);p=pathlib.Path(c.c['catalog_file']);doc=json.loads(p.read_text());e=doc['templates'][0];e['runtime_yaml']={'sha256':'c'*64};arm={**e,'architecture':'arm64','vm_type':'vz','disk':{**e['disk'],'sha256':'b'*64},'runtime_yaml':{'sha256':'d'*64}};doc['templates'].append(arm);raw=json.dumps(doc).encode();p.write_bytes(raw);c.c['catalog_sha256']=catalog_cli.hashlib.sha256(raw).hexdigest()
  def node(id,arch,os,per,over,cpu,mem):return {'node_id':id,'architecture':arch,'host_os':os,'placement_eligible':True,'late_platform_selection':True,'catalog_sha256':c.c['catalog_sha256'],'cpu_mhz_per_vcpu':per,'memory_overhead_mib_per_vm':over,'free_cpu_mhz':cpu,'free_reservation_mib':mem,'available_for_one_new_vm':{'vcpus':cpu//per,'guest_memory_mib':mem-over,'disk_gib':100,'additional_vm_slots':4}}
  nodes=[node('linux','amd64','linux',2000,128,0,8000),node('mac','arm64','darwin',3000,256,10000,16000)];c.capacity=lambda:{'nodes':nodes};return c,s,nodes
 def test_auto_one_job_conservative_envelope_and_lost_response_no_replan(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);calls=[]
   def run(args,input=None):calls.append(json.loads(input)['Job']);return type('R',(),{'returncode':1})()
   c.run=run;c.request_status=lambda session:{'lookup_only':True}
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):c.request(s,'agent-base','v1',None,2,4096,20,'tests')
   job=calls[0];task=job['TaskGroups'][0]['Tasks'][0];self.assertEqual(task['Resources'],{'CPU':6000,'MemoryMB':4352});self.assertEqual(task['Config']['architecture'],'');self.assertEqual(task['Config']['template_disk_sha256'],'');self.assertEqual(len(json.loads(task['Config']['template_variants_json'])),2);self.assertFalse(any(x['LTarget']=='${attr.cpu.arch}' for x in job['Constraints']))
   c.capacity=lambda:(_ for _ in ()).throw(AssertionError('must not replan'));c.templates=c.capacity
   self.assertTrue(c.request(s,'agent-base','v1','auto',2,4096,20,'tests')['lookup_only']);self.assertEqual(len(calls),1)
   with self.assertRaisesRegex(ValueError,'CHANGED'):c.request(s,'agent-base','v1',None,1,4096,20,'tests')
 def test_no_old_driver_wrong_catalog_or_unknown_disk_promised(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);pins=placement.variants(c.templates(),'agent-base','v1',None,20)
   for field,bad in [('late_platform_selection',False),('catalog_sha256','wrong')]:
    n=dict(nodes[1]);n[field]=bad
    with self.assertRaisesRegex(RuntimeError,'UNAVAILABLE'):placement.envelope(pins,[n],2,4096,20,c.c['catalog_sha256'])
   nodes[1]['available_for_one_new_vm']['disk_gib']=None
   with self.assertRaisesRegex(RuntimeError,'NO_CURRENT_FIT'):placement.envelope(pins,nodes,2,4096,20,c.c['catalog_sha256'])
 def test_explicit_filter_minimum_duplicate_and_no_arm_image(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);entries=c.templates();self.assertEqual(len(placement.variants(entries,'agent-base','v1','amd64',20)),1)
   with self.assertRaises(ValueError):placement.variants(entries,'agent-base','v1',None,8)
   with self.assertRaisesRegex(ValueError,'DUPLICATE'):placement.variants(entries+[entries[0]],'agent-base','v1',None,20)
   pins=placement.variants(entries[:1],'agent-base','v1',None,20)
   with self.assertRaisesRegex(RuntimeError,'UNAVAILABLE'):placement.envelope(pins,[nodes[1]],2,4096,20,c.c['catalog_sha256'])
 def test_resolved_event_is_bound_and_changed_or_stale_catalog_rejected(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);pins=placement.variants(c.templates(),'agent-base','v1',None,20);expected={'catalog_sha256':c.c['catalog_sha256'],'variants':pins};v=pins[1]
   details={'template_catalog_sha256':expected['catalog_sha256'],'resolved_architecture':v['architecture'],'resolved_guest_os':v['guest_os'],'resolved_vm_type':v['vm_type'],'template_disk_sha256':v['disk_sha256'],'template_yaml_sha256':v['yaml_sha256']};a={'TaskStates':{'worker':{'Events':[]}}};self.assertIsNone(placement.selected(expected,a,'worker'));events=a['TaskStates']['worker']['Events'];events.append({'Details':details});self.assertEqual(placement.selected(expected,a,'worker')['disk_sha256'],'b'*64)
   events.append({'Details':{**details,'template_disk_sha256':'f'*64}})
   with self.assertRaisesRegex(RuntimeError,'NOT_PINNED'):placement.selected(expected,a,'worker')
   events[:]=[{'Details':{**details,'template_catalog_sha256':'old'}}]
   with self.assertRaisesRegex(RuntimeError,'CATALOG_CHANGED'):placement.selected(expected,a,'worker')
 def test_concurrent_same_request_dispatches_once(self):
  import threading,time
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);calls=[];errors=[]
   def run(args,input=None):calls.append(input);time.sleep(.03);return type('R',(),{'returncode':0})()
   c.run=run;c.request_status=lambda sid:{'lookup_only':True}
   def request():
    try:c.request(s,'agent-base','v1',None,2,4096,20,'tests')
    except Exception as e:errors.append(e)
   threads=[threading.Thread(target=request) for _ in range(2)]
   for t in threads:t.start()
   for t in threads:t.join(2)
   self.assertEqual(errors,[]);self.assertEqual(len(calls),1)
 def test_attempt_binding_distinguishes_restart_node_and_start(self):
  a={'ID':'alloc','NodeID':'node','Job':{'Version':1},'TaskStates':{'worker':{'Restarts':0,'StartedAt':'one'}}};before=placement.allocation_attempt(a,'worker')
  a['TaskStates']['worker']['Restarts']=1;self.assertNotEqual(placement.allocation_attempt(a,'worker'),before)
  a['TaskStates']['worker']['Restarts']=0;a['NodeID']='another';self.assertNotEqual(placement.allocation_attempt(a,'worker'),before)
 def test_capacity_preflight_uses_same_request_envelope_no_dispatch(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);c.run=lambda *args,**kw:(_ for _ in ()).throw(AssertionError('must not dispatch'))
   r=c.capacity_request('agent-base','v1','auto',2,4096,20)['auto_placement_request_fit'];self.assertEqual(r['cpu_mhz'],6000);self.assertEqual(r['reservation_mib'],4352);self.assertEqual({n['node_id']:n['fits_snapshot_envelope'] for n in r['per_node_fit']},{'linux':False,'mac':True});self.assertTrue(r['overreservation_possible'])
 def test_status_waits_selection_and_rejects_rebinding_after_restart(self):
  with tempfile.TemporaryDirectory() as d:
   c,s,nodes=self.fixture(d);jobs=[]
   def run(args,input=None):jobs.append(json.loads(input)['Job']);return type('R',(),{'returncode':0})()
   c.run=run;c.request_status=lambda sid:{};c.request(s,'agent-base','v1',None,2,4096,20,'tests');job=jobs[0];alloc=str(uuid.uuid4());state={'State':'pending','Restarts':0,'StartedAt':None,'Events':[]};full={'ID':alloc,'NodeID':'mac','ClientStatus':'pending','Job':job,'TaskStates':{'worker':state}}
   def api(path):return [{'ID':alloc,'ClientStatus':full['ClientStatus']}] if path.startswith('/v1/job/') else full
   c.api=api;read=lambda:catalog_cli.CatalogClient.request_status(c,s)
   self.assertIsNone(read()['target_config']);self.assertFalse((pathlib.Path(d)/s/'target-config.json').exists())
   pin=json.loads(job['TaskGroups'][0]['Tasks'][0]['Config']['template_variants_json'])[1]
   state.update(State='running',StartedAt='synthetic-start');full['ClientStatus']='running';state['Events']=[{'Details':{'template_catalog_sha256':c.c['catalog_sha256'],'resolved_architecture':pin['architecture'],'resolved_guest_os':pin['guest_os'],'resolved_vm_type':pin['vm_type'],'template_disk_sha256':pin['disk_sha256'],'template_yaml_sha256':pin['yaml_sha256']}}]
   result=read();target=pathlib.Path(result['target_config']);before=target.read_bytes();self.assertEqual(json.loads(before)['resolved_template']['disk_sha256'],'b'*64)
   state['Restarts']=1
   with self.assertRaisesRegex(RuntimeError,'NO_REBIND'):read()
   self.assertEqual(target.read_bytes(),before)
 def test_cli_architecture_optional(self):
  p=subprocess.run([sys.executable,str(pathlib.Path(__file__).parents[1]/'scripts/workerctl.py'),'--config','not-read','request','--help'],capture_output=True,text=True);self.assertEqual(p.returncode,0);self.assertIn('[--architecture {auto,amd64,arm64}]',p.stdout)
