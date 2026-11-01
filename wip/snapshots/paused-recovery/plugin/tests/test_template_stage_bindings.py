import copy,hashlib,importlib.util,json,os,pathlib,pwd,sys,tempfile,unittest,uuid
from types import SimpleNamespace
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import template_save as save
ROOT=pathlib.Path(__file__).parents[2]
def load(name,file):
 spec=importlib.util.spec_from_file_location(name,ROOT/'deploy'/file);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m
source=load('source_finish','template-source-finish.py');node=load('source_clone','template-source-clone.py');artifact=load('artifact_stage','template-artifact-stage.py');cas=load('cas_probe','catalog-cas-probe.py')
class BindingTests(unittest.TestCase):
 def packet(self):
  p={'snapshot':{},'operation_id':str(uuid.uuid4()),'method':'official_stopped_disk_clone_then_existing_seal_export_pipeline','data_composition':{'schema_version':1,'retain':['/usr/local/tool'],'remove':['/workspace/repo'],'description':'Keep installed tools'},'source':{'session_id':str(uuid.uuid4()),'allocation':str(uuid.uuid4()),'task':'worker','creator_agent_id':'creator','creator_session_id':'native-session','attempt':{'restart':0}},'template':{'template_id':'example-project-dev','version':'v1'}};return {'plan':p,'plan_sha256':save.sha(p)}
 def test_node_uses_actual_ledger_lock_and_identity_no_mutation(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);state_dir=root/'state';state_dir.mkdir(mode=0o700);home=root/'lima';home.mkdir(mode=0o700);instance=home/'source';instance.mkdir();(instance/'disk').write_bytes(b'prepared');key='a'*64;p=self.packet();s=p['plan']['source']
   state={'Key':key,'AllocID':s['allocation'],'TaskName':s['task'],'WorkerSessionID':s['session_id'],'CreatorAgentID':s['creator_agent_id'],'CreatorSessionID':s['creator_session_id'],'Owner':'owner','Phase':'destroyed_disk_retained','MachineID':'m','BootID':'b','Instance':'source'}
   f=state_dir/(key+'.json');f.write_text(json.dumps(state));f.chmod(0o600);(state_dir/'owner').write_text('owner');before=f.read_bytes()
   receipt={'operation_id':p['plan']['operation_id'],'plan_sha256':p['plan_sha256'],'source_allocation':s['allocation'],'source_attempt':s['attempt'],**dict.fromkeys(('source_owner_verified','results_receipt_verified','prime_bao_stopped','task_token_revoked','source_vm_stopped','source_disk_retained'),True)}
   def invoke(packet,receipts,context):
    self.assertEqual(receipts['source_finish']['source_instance'],'source');self.assertIn('disk',receipts['source_finish']['source_disk_identity']);return {'cloned':True}
   result=node.bound_clone(p,{'source_finish':receipt},{'ledger_key':key,'state_dir':str(state_dir),'lima_home':str(home)},invoke);self.assertTrue(result['cloned']);self.assertEqual(f.read_bytes(),before)
   state['AllocID']='other';f.write_text(json.dumps(state))
   with self.assertRaisesRegex(RuntimeError,'IDENTITY_CHANGED'):node.bound_clone(p,{'source_finish':receipt},{'ledger_key':key,'state_dir':str(state_dir),'lima_home':str(home)},lambda *a:self.fail('must not clone'))
 def test_guest_program_compiles_without_executing(self):
  p=self.packet();r={'clone_candidate':{'candidate_instance':'candidate'}};program=artifact.guest_program(p,r);compile(program,'candidate-only','exec');self.assertIn('CANDIDATE_RUNTIME_PROCESS_PRESENT',program);self.assertNotIn('reinstall',program)
 def test_source_finish_refuses_nonvault_before_any_stop(self):
  p=self.packet();s=p['plan']['source'];s['config_sha256']='hash';s['job']='worker-job';p['plan_sha256']=save.sha(p['plan'])
  a=SimpleNamespace(hash='hash',c={'allocation':s['allocation'],'job':s['job'],'task':'worker'},require_initiator=lambda *a:self.fail('no effects before vault check'))
  with self.assertRaisesRegex(RuntimeError,'VAULT_CLEANUP'):source.finish(a,p)
class ProbeStore:
 def __init__(self,broken=False):self.raw=None;self.etag='0';self.lock=__import__('threading').Lock();self.broken=broken
 def get(self,key):return self.raw
 def stat(self,key):return {'etag':self.etag}
 def conditional_put(self,key,raw,etag=None):
  with self.lock:
   if not self.broken and ((etag is None and self.raw is not None) or (etag is not None and etag!=self.etag)):raise RuntimeError('412')
   self.raw=raw;self.etag=str(int(self.etag)+1)
class CASTests(unittest.TestCase):
 def test_one_disposable_key_no_catalog_or_delete(self):
  key='environments/.cas-probes/'+str(uuid.uuid4())+'/probe.json';store=ProbeStore();result=cas.probe(store,key);self.assertTrue(result['competing_if_match_one_winner']);self.assertTrue(result['probe_retained']);self.assertTrue(result['catalog_untouched'])
 def test_backend_ignoring_conditions_fails_closed(self):
  with self.assertRaisesRegex(RuntimeError,'IF_NONE_MATCH_NOT_ENFORCED'):cas.probe(ProbeStore(True),'environments/.cas-probes/'+str(uuid.uuid4())+'/probe.json')
