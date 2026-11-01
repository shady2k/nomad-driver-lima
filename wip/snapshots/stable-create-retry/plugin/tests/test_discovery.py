import copy,hashlib,json,pathlib,sys,tempfile,unittest,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import catalog_cli,discovery
class DiscoveryTests(unittest.TestCase):
 def fixture(self):
  tmp=tempfile.TemporaryDirectory();self.addCleanup(tmp.cleanup);root=pathlib.Path(tmp.name);root.chmod(0o700)
  u=lambda:str(uuid.uuid4());agent,native,generation,owner=[u()for _ in range(4)]
  c={'receipt_root':str(root),'owner_id':owner,'agent_instance_id':agent,'namespace':'workers','task':'worker','datacenters':['dc'],'vault':{'role':'agent-worker-prime'},'guest_bootstrap_command':['/runtime','--session={session_id}']}
  cfg=root/'caller.config';catalog_cli.atomic(cfg,c);client=catalog_cli.CatalogClient(cfg);remote={};calls=[]
  def api(path):
   self.assertTrue(path.startswith('/v1/allocation/'));calls.append(path)
   key=path.split('/')[3].split('?')[0]
   if key not in remote:raise RuntimeError('NOMAD_READ_UNAVAILABLE_OR_ACL_DENIED')
   return copy.deepcopy(remote[key])
  client.api=api;client.run=lambda *a,**k:self.fail('discovery must not submit/exec')
  creator={'agent_instance_id':agent,'native_session_id':native,'generation':generation,'ended':False,'evidence':'native_hook_observation_not_authenticated_liveness'}
  record=root/'native.json';catalog_cli.atomic(record,creator)
  def add(session=None,parent=None,actor=None,project='project',state='running'):
   session=session or u();actor=actor or creator;allocation=u();job='worker-'+session
   intent={'session_id':session,'job':job,'creator_agent_id':actor['agent_instance_id'],'creator_session_id':actor['native_session_id'],'creator_generation':actor['generation']}
   binding={**c,'allocation':allocation,'job':job,'agent_instance_id':actor['agent_instance_id'],'creator_session_id':actor['native_session_id'],'creator_generation':actor['generation']}
   meta={'nomad_workers_owner':owner,'creator_agent_id':actor['agent_instance_id'],'creator_session_id':actor['native_session_id'],'creator_generation':actor['generation'],'worker_session_id':session,'project':project,'purpose':'prepare-environment','source_task_title':'Prepare project environment','source_agent_name':'Coordinator','source_agent_type':'Claude'}
   taskcfg={'worker_session_id':session,'guest_cpus':1,'memory_mib':512,'disk_gib':8,'architecture':'amd64','guest_os':'linux'}
   if parent:
    intent.update(private_copy_id='recover-project',recovery_parent={'source_session':parent[0],'source_allocation':parent[1],'source_key':'a'*64,'copy_sha256':'b'*64})
    binding['recovery_parent']=intent['recovery_parent'];meta.update(private_copy_id='recover-project',recovery_source_session=parent[0],recovery_source_allocation=parent[1],recovery_source_key='a'*64,recovery_copy_sha256='b'*64);taskcfg['private_copy_id']='recover-project'
   catalog_cli.atomic(root/session/'allocation-intent.json',intent);catalog_cli.atomic(root/session/'target-config.json',binding)
   remote[allocation]={'ID':allocation,'NodeID':'node-fixture','Namespace':'workers','ClientStatus':state,'DesiredStatus':'stop'if state=='complete'else'run','Job':{'ID':job,'Namespace':'workers','Meta':meta,'TaskGroups':[{'Tasks':[{'Name':'worker','Driver':'lima','Config':taskcfg,'Resources':{'CPU':1000,'MemoryMB':640}}]}]},'TaskStates':{'worker':{'State':'dead'if state=='complete'else'running','StartedAt':'2026-10-31T10:00:00Z'}}}
   return session,allocation
  return root,client,record,creator,remote,add
 def test_mine_exact_native_identity_and_read_only(self):
  root,c,record,native,remote,add=self.fixture();mine=add();other=add(actor={**native,'agent_instance_id':str(uuid.uuid4())})
  before={str(p):hashlib.sha256(p.read_bytes()).hexdigest()for p in root.rglob('*')if p.is_file()}
  out=c.find_workers('project',record);self.assertEqual([r['session_id']for r in out['candidates']],[mine[0]]);self.assertFalse(out['automatic_binding']);self.assertFalse(out['project_is_identity'])
  after={str(p):hashlib.sha256(p.read_bytes()).hexdigest()for p in root.rglob('*')if p.is_file()};self.assertEqual(before,after)
  self.assertEqual(len(c.find_workers('project',record,True)['candidates']),2)
  catalog_cli.atomic(record,{**native,'generation':str(uuid.uuid4())});self.assertFalse(c.find_workers('project',record)['candidates'])
 def test_index_private_copy_ids_and_historical_source_no_target_rewrite(self):
  root,c,record,native,remote,add=self.fixture();old=add(state='complete');new=add(parent=old,project='recover-project')
  original=(root/old[0]/'target-config.json').read_bytes();target=(root/new[0]/'target-config.json').read_bytes()
  out=c.index_lineage(new[0]);self.assertTrue(out['indexed']);self.assertEqual(out,c.index_lineage(new[0]));self.assertEqual(original,(root/old[0]/'target-config.json').read_bytes());self.assertEqual(target,(root/new[0]/'target-config.json').read_bytes())
  found=c.find_workers('project',record);self.assertEqual(found['current_candidate_count'],1);self.assertFalse(found['ambiguous']);rows={r['session_id']:r for r in found['candidates']};self.assertTrue(rows[old[0]]['historical_only']);self.assertEqual(rows[old[0]]['current_session_id'],new[0]);self.assertEqual(rows[new[0]]['previous_session_id'],old[0]);self.assertFalse(out['automatic_alias_adoption']);self.assertNotIn('target_config',rows[old[0]]);self.assertEqual(rows[old[0]]['current_target_config'],str(root/new[0]/'target-config.json'))
 def test_ambiguity_never_picks_latest(self):
  root,c,record,native,remote,add=self.fixture();old=add(state='complete');one=add(parent=old);two=add(parent=old)
  c.index_lineage(one[0]);out=c.index_lineage(two[0]);self.assertTrue(out['ambiguous'])
  found=c.find_workers('project',record);self.assertTrue(found['ambiguous']);self.assertEqual(found['current_candidate_count'],2);source=next(r for r in found['candidates']if r['session_id']==old[0]);self.assertIsNone(source['current_session_id']);self.assertEqual(set(source['resumed_to']),{one[0],two[0]});self.assertFalse(found['automatic_binding'])
 def test_wrong_provenance_owner_and_nonrunning_target_rejected(self):
  for kind in ('owner','parent','task','not-running'):
   root,c,record,native,remote,add=self.fixture();old=add(state='complete');new=add(parent=old);full=remote[new[1]]
   if kind=='owner':full['Job']['Meta']['creator_generation']=str(uuid.uuid4())
   if kind=='parent':full['Job']['Meta']['recovery_source_key']='c'*64
   if kind=='task':full['Job']['TaskGroups'][0]['Tasks'][0]['Config']['private_copy_id']='wrong'
   if kind=='not-running':full['ClientStatus']='complete'
   with self.subTest(kind=kind),self.assertRaises(ValueError):c.index_lineage(new[0])
   self.assertFalse((root/'lineage').exists())
 def test_remote_failure_not_claimed_stopped_and_symlinks_refused(self):
  root,c,record,native,remote,add=self.fixture();old=add();remote.clear();out=c.find_workers('project',record);self.assertFalse(out['candidates']);self.assertTrue(out['unverified'][0]['not_proof_of_stopped'])
  (root/'lineage').symlink_to(root)
  with self.assertRaisesRegex(ValueError,'UNSAFE_LINEAGE'):c.find_workers(None,record)
 def test_resume_parent_supported_without_claiming_private_copy(self):
  root,c,record,native,remote,add=self.fixture();old=add(state='complete');new=add();p=root/new[0]/'allocation-intent.json';i=json.loads(p.read_text());i.update(resumed_from=old[1],resumed_from_session=old[0]);catalog_cli.atomic(p,i);remote[new[1]]['Job']['Meta']['resumed_from']=old[1]
  remote[old[1]]['TaskStates']['worker']['Events']=[{'DriverMessage':'lima vm_stopped','Details':{'stop_revocation_confirmed':'true','resume_source_key':'a'*64,'resume_node_id':'node-fixture','resume_generation':'1'}}]
  remote[new[1]]['Job']['TaskGroups'][0]['Tasks'][0]['Config'].update(resume_from_key='a'*64,resume_node_id='node-fixture',resume_generation=1)
  out=c.index_lineage(new[0]);self.assertEqual(out['record']['kind'],'resume');self.assertEqual(out['record']['previous_session_id'],old[0])
  remote[new[1]]['Job']['TaskGroups'][0]['Tasks'][0]['Config']['resume_from_key']='b'*64
  with self.assertRaisesRegex(ValueError,'SOURCE_RECEIPT'):c.index_lineage(new[0])
 def test_existing_root_required_for_read_only_and_ended_creator_refused(self):
  root,c,record,native,remote,add=self.fixture();cfg=root/'missing.config';catalog_cli.atomic(cfg,{**c.c,'receipt_root':str(root/'missing')})
  with self.assertRaisesRegex(ValueError,'EXISTING_RECEIPT_ROOT'):catalog_cli.CatalogClient(cfg,read_only=True)
  self.assertFalse((root/'missing').exists())
  catalog_cli.atomic(record,{**native,'ended':True})
  with self.assertRaisesRegex(ValueError,'CURRENT_NATIVE_CREATOR'):c.find_workers(None,record)
 def test_verified_multi_hop_mapping_resolves_current_without_autoadoption(self):
  root,c,record,native,remote,add=self.fixture();old=add(state='complete');one=add(parent=old);two=add(parent=one)
  c.index_lineage(one[0]);c.index_lineage(two[0]);out=c.find_workers('project',record);self.assertEqual(out['current_candidate_count'],1)
  first=next(r for r in out['candidates']if r['session_id']==old[0]);self.assertEqual(first['current_session_id'],two[0]);self.assertFalse(out['automatic_binding'])
if __name__=='__main__':unittest.main()
