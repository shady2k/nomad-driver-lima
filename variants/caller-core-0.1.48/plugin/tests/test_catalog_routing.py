import copy,hashlib,json,pathlib,sys,tempfile,types,unittest,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import catalog_cli,catalog_routing

TITLE=dict(task_title='Validate independently pinned native catalogs',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')

class CatalogRoutingTests(unittest.TestCase):
 def fixture(self,root):
  root=pathlib.Path(root)
  def entry(arch,vm,disk,yaml):
   return dict(template_id='agent-base',version='v1',architecture=arch,vm_type=vm,guest_os='linux',
     disk=dict(sha256=disk*64,minimum_disk_gib=8,size_bytes=1,url='https://artifact.invalid/disk?token=PRIVATE_URL_CAPABILITY'),
     runtime_yaml=dict(sha256=yaml*64,size_bytes=1,url='file:///PRIVATE/UNEXPOSED/runtime.yaml'),
     acceptance=dict(sealed=True,independent_boots=2,unique_identities=True),tools={'prime':'0.10.1','prime_model_catalog':'1'})
  def write(name,entries):
   p=root/name;raw=json.dumps(dict(schema_version=1,templates=entries)).encode();p.write_bytes(raw)
   return str(p),hashlib.sha256(raw).hexdigest()
  linux=entry('amd64','qemu','a','c');arm=entry('arm64','vz','b','d')
  base,base_sha=write('linux.json',[linux]);extra,extra_sha=write('arm.json',[arm])
  config=dict(catalog_file=base,catalog_sha256=base_sha,catalog_sources=[dict(id='native-arm',catalog_file=extra,catalog_sha256=extra_sha)],
    receipt_root=str(root/'receipts'),owner_id=str(uuid.uuid4()),agent_instance_id=str(uuid.uuid4()),
    creator_session_id='fixture-session',memory_overhead_mib=128,guest_bootstrap_command=['/bin/true'],namespace='agent-workers',datacenters=['dc1'],task='worker')
  path=root/'client.json';path.write_text(json.dumps(config));c=catalog_cli.CatalogClient(path)
  def node(name,arch,os,sha,per,cpu):
   return dict(node_id=name,architecture=arch,host_os=os,catalog_sha256=sha,placement_eligible=True,late_platform_selection=True,
     cpu_mhz_per_vcpu=per,memory_overhead_mib_per_vm=128,free_cpu_mhz=cpu,free_reservation_mib=28672,
     available_for_one_new_vm=dict(vcpus=cpu//per,guest_memory_mib=24576,disk_gib=64,additional_vm_slots=None,telemetry='fresh_driver_admission_snapshot'))
  nodes=[node('linux','amd64','linux',base_sha,2000,8000),node('mac','arm64','darwin',extra_sha,4000,24000)]
  c.capacity=lambda:{'nodes':copy.deepcopy(nodes)}
  return c,nodes,write,linux,arm

 def request(self,c,session=None,arch=None,**kw):
  return c.request(session or str(uuid.uuid4()),'agent-base',kw.pop('version','v1'),arch,
    kw.pop('cpus',2),kw.pop('memory',2048),kw.pop('disk',8),'tests',**TITLE,**kw)

 def capture(self,c,code=0):
  jobs=[]
  def run(args,input=None):jobs.append(json.loads(input)['Job']);return types.SimpleNamespace(returncode=code)
  c.run=run;c.request_status=lambda s:{'lookup_only':True};return jobs

 def test_discovery_returns_both_sources_without_urls_paths_or_arbitrary_metadata(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,write,linux,arm=self.fixture(t)
   linux['tools']['private_note']='https://PRIVATE_HOST/path';linux['acceptance']['receipt']='file:///PRIVATE/RECEIPT'
   c.c['catalog_file'],c.c['catalog_sha256']=write('linux-redacted.json',[linux])
   rows=c.templates();self.assertEqual({(r['architecture'],r['vm_type'])for r in rows},{('amd64','qemu'),('arm64','vz')})
   self.assertEqual([r['catalog_source']for r in rows],['primary','native-arm'])
   text=json.dumps(rows)
   for secret in ['PRIVATE_URL_CAPABILITY','PRIVATE/UNEXPOSED','PRIVATE_HOST','PRIVATE/RECEIPT',c.c['catalog_file']]:self.assertNotIn(secret,text)

 def test_auto_primary_fit_keeps_linux_pin_and_no_cross_source_envelope(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);before=pathlib.Path(c.c['catalog_file']).read_bytes();jobs=self.capture(c);self.request(c)
   job=jobs[0];task=job['TaskGroups'][0]['Tasks'][0];cfg=task['Config']
   self.assertEqual(cfg['template_catalog_sha256'],c.c['catalog_sha256']);self.assertEqual(task['Resources'],dict(CPU=4000,MemoryMB=2176))
   self.assertEqual([p['architecture']for p in json.loads(cfg['template_variants_json'])],['amd64'])
   self.assertEqual(job['Meta']['requested_architecture'],'auto');self.assertEqual(job['Meta']['catalog_source'],'primary')
   self.assertEqual(pathlib.Path(c.c['catalog_file']).read_bytes(),before)

 def test_auto_falls_back_to_mac_using_its_own_unchanged_pin(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes[0]['free_cpu_mhz']=0;nodes[0]['available_for_one_new_vm']['vcpus']=0;jobs=self.capture(c);s=str(uuid.uuid4());self.request(c,s)
   job=jobs[0];task=job['TaskGroups'][0]['Tasks'][0];sha=c.c['catalog_sources'][0]['catalog_sha256']
   self.assertEqual(task['Config']['template_catalog_sha256'],sha);self.assertEqual(task['Resources'],dict(CPU=8000,MemoryMB=2176))
   self.assertEqual(job['Meta']['catalog_source'],'native-arm')
   constraints={r['LTarget']:r['RTarget']for r in job['Constraints']};self.assertEqual(constraints['${attr.driver.lima.catalog.sha256}'],sha)
   self.assertEqual(constraints['${node.unique.id}'],'mac')
   intent=json.loads((c.root/s/'allocation-intent.json').read_text());self.assertEqual(intent['placement']['catalog_sha256'],sha)
   self.assertEqual(intent['catalog_route']['catalog_routes'][0]['fits_snapshot'],False)

 def test_explicit_arch_and_profile_filter_still_use_correct_source(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);jobs=self.capture(c);self.request(c,arch='arm64')
   self.assertEqual(jobs[0]['Meta']['catalog_source'],'native-arm');self.assertEqual(jobs[0]['Meta']['requested_architecture'],'arm64')
   nodes[0]['repository_profiles']={};nodes[1]['repository_profiles']={'cache':'e'*64}
   self.request(c,repository_profile='cache',repository_profile_sha256='e'*64)
   self.assertEqual(jobs[1]['Meta']['catalog_source'],'native-arm')

 def test_real_effective_caps_unknown_capacity_and_unhealthy_nodes_prevent_dispatch(self):
  for change in [dict(vcpus=1),dict(guest_memory_mib=1024),dict(disk_gib=7),dict(disk_gib=None),dict(additional_vm_slots=0)]:
   with self.subTest(change=change),tempfile.TemporaryDirectory()as t:
    c,nodes,*_=self.fixture(t);nodes[0]['placement_eligible']=False;nodes[1]['available_for_one_new_vm'].update(change);jobs=self.capture(c);s=str(uuid.uuid4())
    with self.assertRaisesRegex(RuntimeError,'NO_PINNED_CATALOG_SOURCE'):self.request(c,s)
    self.assertEqual(jobs,[]);self.assertFalse((c.root/s/'allocation-intent.json').exists())
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes[0]['placement_eligible']=False;nodes[1]['placement_eligible']=False;self.capture(c)
   with self.assertRaisesRegex(RuntimeError,'NO_PINNED_CATALOG_SOURCE'):self.request(c)

 def test_capacity_preview_exposes_routes_and_keeps_sources_separate_without_dispatch(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes[0]['available_for_one_new_vm']['vcpus']=1;c.run=lambda *a,**k:(_ for _ in ()).throw(AssertionError('no dispatch'))
   result=c.capacity_request('agent-base','v1',None,2,2048,8)
   route=result['auto_placement_request_fit'];self.assertEqual(route['catalog_source'],'native-arm');self.assertEqual(route['cpu_mhz'],8000)
   self.assertEqual(len(route['catalog_routes']),2);self.assertEqual(result['nodes'][0]['available_for_one_new_vm']['vcpus'],1)

 def test_selected_source_excludes_nodes_with_known_no_fit(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes.append(copy.deepcopy(nodes[0]));nodes[-1]['node_id']='linux-full';nodes[-1]['available_for_one_new_vm']['additional_vm_slots']=0
   jobs=self.capture(c);self.request(c)
   ids=next(x['RTarget']for x in jobs[0]['Constraints']if x['LTarget']=='${node.unique.id}')
   self.assertEqual(ids,'linux')

 def test_unrecognized_node_pin_reports_unknown_instead_of_free_capacity(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes.append(dict(nodes[1],node_id='unknown',catalog_sha256='f'*64))
   result=catalog_routing.capacity({'nodes':nodes},c.templates())
   self.assertEqual([n['catalog_source']for n in result['nodes']],['primary','native-arm',None])
   unknown=result['nodes'][-1];self.assertFalse(unknown['placement_eligible']);self.assertIsNone(unknown['available_for_one_new_vm']['vcpus'])
   self.assertEqual(len(result['catalog_sources']),2)

 def test_changed_source_bad_source_schema_and_conflicting_versions_fail_before_submit(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,write,linux,arm=self.fixture(t);jobs=self.capture(c);pathlib.Path(c.c['catalog_sources'][0]['catalog_file']).write_bytes(b'changed')
   with self.assertRaisesRegex(ValueError,'CHECKSUM'):self.request(c)
   self.assertEqual(jobs,[])
  with tempfile.TemporaryDirectory()as t:
   c,nodes,write,linux,arm=self.fixture(t);extra=c.c['catalog_sources'][0]
   for bad in [None,{},[dict(extra,id='primary')],[dict(extra,unexpected='value')],[dict(extra,catalog_sha256='bad')],[dict(extra,catalog_file='relative.json')]]:
    with self.subTest(bad=bad),self.assertRaises(ValueError):catalog_routing.metadata(dict(c.c,catalog_sources=bad))
   conflict=copy.deepcopy(linux);conflict['disk']['sha256']='e'*64
   extra['catalog_file'],extra['catalog_sha256']=write('conflict.json',[conflict])
   with self.assertRaisesRegex(ValueError,'PLATFORM_CONFLICT'):c.templates()

 def test_different_release_versions_are_not_silently_substituted(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,write,linux,arm=self.fixture(t);arm['version']='v2';extra=c.c['catalog_sources'][0];extra['catalog_file'],extra['catalog_sha256']=write('arm-v2.json',[arm]);nodes[1]['catalog_sha256']=extra['catalog_sha256'];nodes[0]['placement_eligible']=False;jobs=self.capture(c)
   with self.assertRaisesRegex(RuntimeError,'NO_PINNED_CATALOG_SOURCE'):self.request(c)
   self.request(c,version='v2');self.assertEqual(jobs[0]['Meta']['catalog_source'],'native-arm')

 def test_unknown_submit_never_replans_on_new_capacity_or_changed_source_bytes(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes[0]['placement_eligible']=False;jobs=self.capture(c,1);s=str(uuid.uuid4())
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):self.request(c,s)
   intent=json.loads((c.root/s/'allocation-intent.json').read_text());self.assertEqual(intent['catalog_route']['catalog_source'],'native-arm')
   pathlib.Path(c.c['catalog_sources'][0]['catalog_file']).write_bytes(b'changed after unknown submit')
   c.capacity=lambda:(_ for _ in ()).throw(AssertionError('must not replan'));c.templates=c.capacity
   self.assertTrue(self.request(c,s)['lookup_only']);self.assertEqual(len(jobs),1)
   with self.assertRaisesRegex(ValueError,'CHANGED'):self.request(c,s,cpus=1)

 def test_adding_discovery_sources_keeps_legacy_intent_lookup_only(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);extra=c.c.pop('catalog_sources');jobs=self.capture(c,1);s=str(uuid.uuid4())
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):self.request(c,s)
   c.c['catalog_sources']=extra;c.capacity=lambda:(_ for _ in ()).throw(AssertionError('must not replan'));c.templates=c.capacity
   self.assertTrue(self.request(c,s)['lookup_only']);self.assertEqual(len(jobs),1)
   c.c['catalog_sha256']='f'*64
   with self.assertRaisesRegex(ValueError,'CHANGED'):self.request(c,s)

 def test_source_status_requires_selected_pin_and_preserves_existing_target(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);nodes[0]['placement_eligible']=False;jobs=self.capture(c);s=str(uuid.uuid4());self.request(c,s)
   job=jobs[0];cfg=job['TaskGroups'][0]['Tasks'][0]['Config'];pin=json.loads(cfg['template_variants_json'])[0]
   details=dict(template_catalog_sha256=cfg['template_catalog_sha256'],resolved_architecture=pin['architecture'],resolved_guest_os=pin['guest_os'],resolved_vm_type=pin['vm_type'],template_disk_sha256=pin['disk_sha256'],template_yaml_sha256=pin['yaml_sha256'])
   alloc=str(uuid.uuid4());state=dict(State='running',StartedAt='synthetic-start',Restarts=0,Events=[dict(Details=details)]);full=dict(ID=alloc,NodeID='mac',ClientStatus='running',Job=job,TaskStates={'worker':state})
   c.api=lambda p:[dict(ID=alloc,ClientStatus='running')]if p.startswith('/v1/job/')else full
   read=lambda:catalog_cli.CatalogClient.request_status(c,s)
   result=read();target=pathlib.Path(result['target_config']);before=target.read_bytes();bound=json.loads(before)
   self.assertEqual(bound['resolved_template']['catalog_sha256'],c.c['catalog_sources'][0]['catalog_sha256']);self.assertEqual(bound['catalog_route']['catalog_source'],'native-arm')
   state['Restarts']=1
   with self.assertRaisesRegex(RuntimeError,'NO_REBIND'):read()
   self.assertEqual(target.read_bytes(),before);state['Restarts']=0;details['template_catalog_sha256']=c.c['catalog_sha256']
   with self.assertRaisesRegex(RuntimeError,'CATALOG_CHANGED'):read()

 def test_legacy_target_survives_source_only_rollout_but_never_attempt_rebinding(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);extra=c.c.pop('catalog_sources');jobs=self.capture(c);s=str(uuid.uuid4());self.request(c,s)
   job=jobs[0];cfg=job['TaskGroups'][0]['Tasks'][0]['Config'];pin=json.loads(cfg['template_variants_json'])[0]
   details=dict(template_catalog_sha256=cfg['template_catalog_sha256'],resolved_architecture=pin['architecture'],resolved_guest_os=pin['guest_os'],resolved_vm_type=pin['vm_type'],template_disk_sha256=pin['disk_sha256'],template_yaml_sha256=pin['yaml_sha256'])
   alloc=str(uuid.uuid4());state=dict(State='running',StartedAt='synthetic-start',Restarts=0,Events=[dict(Details=details)]);full=dict(ID=alloc,NodeID='linux',ClientStatus='running',Job=job,TaskStates={'worker':state})
   c.api=lambda p:[dict(ID=alloc,ClientStatus='running')]if p.startswith('/v1/job/')else full
   read=lambda:catalog_cli.CatalogClient.request_status(c,s)
   target=pathlib.Path(read()['target_config']);before=target.read_bytes();c.c['catalog_sources']=extra
   self.assertEqual(read()['target_config'],str(target));self.assertEqual(target.read_bytes(),before)
   state['Restarts']=1
   with self.assertRaisesRegex(RuntimeError,'NO_REBIND'):read()
   self.assertEqual(target.read_bytes(),before)

 def test_ready_legacy_session_receipt_survives_source_only_rollout(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);extra=c.c.pop('catalog_sources');jobs=self.capture(c);s=str(uuid.uuid4());self.request(c,s)
   job=jobs[0];cfg=job['TaskGroups'][0]['Tasks'][0]['Config'];pin=json.loads(cfg['template_variants_json'])[0]
   details=dict(template_catalog_sha256=cfg['template_catalog_sha256'],resolved_architecture=pin['architecture'],resolved_guest_os=pin['guest_os'],resolved_vm_type=pin['vm_type'],template_disk_sha256=pin['disk_sha256'],template_yaml_sha256=pin['yaml_sha256'])
   alloc=str(uuid.uuid4());state=dict(State='running',StartedAt='synthetic-start',Restarts=0,Events=[dict(Details=details)]);full=dict(ID=alloc,NodeID='linux',ClientStatus='running',Job=job,TaskStates={'worker':state})
   c.api=lambda p:[dict(ID=alloc,ClientStatus='running')]if p.startswith('/v1/job/')else full
   read=lambda:catalog_cli.CatalogClient.request_status(c,s)
   target=pathlib.Path(read()['target_config']);before=target.read_bytes();bound=json.loads(before)
   attempt=dict(allocation=alloc,job=job['ID'],task='worker',job_version=job.get('Version'),restarts=0,started_at='synthetic-start',resolved_template=bound['resolved_template'])
   session=target.parent/'session.json';session.write_text(json.dumps(dict(config_hash=hashlib.sha256(catalog_cli.canonical(bound)).hexdigest(),attempt=attempt,phase='ready',record_id=str(uuid.uuid4()))));saved_session=session.read_bytes()
   self.assertEqual(read()['readiness_stage'],'session_ready_receipt');c.c['catalog_sources']=extra
   self.assertEqual(read()['readiness_stage'],'session_ready_receipt')
   self.assertEqual(target.read_bytes(),before);self.assertEqual(session.read_bytes(),saved_session)
   state['Restarts']=1
   with self.assertRaisesRegex(RuntimeError,'NO_REBIND'):read()
   self.assertEqual(target.read_bytes(),before);self.assertEqual(session.read_bytes(),saved_session)

 def test_legacy_mode_templates_and_explicit_requests_keep_existing_contract(self):
  with tempfile.TemporaryDirectory()as t:
   c,nodes,*_=self.fixture(t);c.c.pop('catalog_sources');rows=c.templates();self.assertEqual(len(rows),1);self.assertNotIn('catalog_source',rows[0])
   nodes[0].pop('late_platform_selection');jobs=self.capture(c);self.request(c,arch='amd64');cfg=jobs[0]['TaskGroups'][0]['Tasks'][0]['Config']
   self.assertEqual(cfg['architecture'],'amd64');self.assertEqual(cfg['template_disk_sha256'],'a'*64);self.assertNotIn('template_variants_json',cfg)

if __name__=='__main__':unittest.main()
