import copy,json,pathlib,sys,tempfile,unittest,uuid,subprocess
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import private_copy,catalog_cli
class PrivateCopyTest(unittest.TestCase):
 def fixture(self):
  u=lambda:str(uuid.uuid4());v={'id':'recover-project','node_id':u(),'namespace':'workers','creator_agent_id':u(),'creator_session_id':u(),'creator_generation':u(),'source_allocation':u(),'source_session':u(),'source_key':'a'*64,'copy_sha256':'b'*64,'virtual_size_bytes':8<<30,'architecture':'amd64','operator_approved':True}
  c={'namespace':'workers','owner_id':u(),'agent_instance_id':v['creator_agent_id'],'task':'vm','datacenters':['dc'],'vault':{'role':'agent-worker-prime'},'guest':{'prime_vault':{'configured':True}},'guest_bootstrap_command':['/runtime','--session={session_id}']}
  native={'agent_instance_id':v['creator_agent_id'],'native_session_id':v['creator_session_id'],'generation':v['creator_generation'],'ended':False,'evidence':'native_hook_observation_not_authenticated_liveness'}
  nodes=[{'node_id':v['node_id'],'placement_eligible':True,'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000,'memory_overhead_mib_per_vm':128,'available_for_one_new_vm':{'vcpus':4,'guest_memory_mib':8192,'disk_gib':32,'additional_vm_slots':2}}]
  return c,{'schema_version':1,'max_reserved_disk_gib':88,'inputs':{v['id']:v}},native,nodes,u()
 def test_new_job_no_fake_catalog_no_old_command(self):
  c,m,n,nodes,s=self.fixture();job=private_copy.plan(c,m,'recover-project',s,n,2,4096,nodes);task=job['TaskGroups'][0]['Tasks'][0]
  self.assertEqual(task['Config']['disk_gib'],8);self.assertEqual(task['Config']['command'],['/runtime','--session='+s]);self.assertNotIn('template_id',task['Config']);self.assertNotIn('resume_from_key',task['Config']);self.assertTrue(task['Config']['vault_required']);self.assertEqual(task['Resources'],{'CPU':2000,'MemoryMB':4224});self.assertNotIn('sealed',json.dumps(job));self.assertEqual(job['Constraints'][0]['RTarget'],nodes[0]['node_id'])
  binary=pathlib.Path('/opt/homebrew/bin/nomad')
  if binary.exists():
   with tempfile.NamedTemporaryFile(suffix='.json')as f:
    f.write(json.dumps({'Job':job}).encode());f.flush();r=subprocess.run([str(binary),'job','validate','-json',f.name],capture_output=True);self.assertEqual(r.returncode,0,r.stderr.decode())
 def test_wrong_owner_changed_generation_capacity_and_reused_session(self):
  c,m,n,nodes,s=self.fixture()
  for mutate in ('owner','generation','ended','capacity','node','same-session'):
   a,b,d,e=copy.deepcopy((c,m,n,nodes));session=s
   if mutate=='owner':d['agent_instance_id']=str(uuid.uuid4())
   if mutate=='generation':d['generation']=str(uuid.uuid4())
   if mutate=='ended':d['ended']=True
   if mutate=='capacity':e[0]['available_for_one_new_vm']['disk_gib']=0
   if mutate=='node':e[0]['placement_eligible']=False
   if mutate=='same-session':session=b['inputs']['recover-project']['source_session']
   with self.assertRaises(ValueError):private_copy.plan(a,b,'recover-project',session,d,2,4096,e)
 def test_lost_submit_response_no_retry(self):
  from unittest.mock import patch
  from types import SimpleNamespace
  c,m,n,nodes,s=self.fixture()
  with tempfile.TemporaryDirectory()as folder:
   root=pathlib.Path(folder);root.chmod(0o700);p=root/'config';c.update(receipt_root=str(root),private_copies_sha256='c'*64);p.write_text(json.dumps(c));client=catalog_cli.CatalogClient(p);calls=[]
   client.capacity=lambda:{'nodes':nodes};client.run=lambda args,input=None:(calls.append(args)or SimpleNamespace(returncode=1));client.request_status=lambda session:{'session':session,'lookup_only':True}
   with patch('private_copy.load',return_value=m),patch('exchange.protected',return_value=n):
    with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):client.recover_copy(s,'recover-project','native',2,4096)
    self.assertTrue(client.recover_copy(s,'recover-project','native',2,4096)['lookup_only']);self.assertEqual(len(calls),1)
    with self.assertRaisesRegex(ValueError,'REQUEST_CHANGED'):client.recover_copy(s,'recover-project','native',3,4096)
if __name__=='__main__':unittest.main()
