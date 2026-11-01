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

class TrustedEvidenceBindingTests(unittest.TestCase):
 def test_hash_pinned_operator_evidence_and_symlink_rejected(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);path=root/'proof.json';raw=b'{"schema_version":1}';path.write_bytes(raw);path.chmod(0o600);cfg={'trusted_provenance_file':str(path),'trusted_provenance_sha256':hashlib.sha256(raw).hexdigest()}
   self.assertEqual(artifact.trusted_provenance(cfg),{'schema_version':1})
   path.write_bytes(raw+b' ')
   with self.assertRaisesRegex(RuntimeError,'SHA_MISMATCH'):artifact.trusted_provenance(cfg)
   path.unlink();other=root/'other';other.write_bytes(raw);other.chmod(0o600);path.symlink_to(other)
   with self.assertRaisesRegex(RuntimeError,'PRIVATE_TRUSTED'):artifact.trusted_provenance(cfg)
 def test_embedded_evidence_compiles_and_no_default_exemption(self):
  p=BindingTests().packet();r={'clone_candidate':{'candidate_instance':'candidate'}};e={'schema_version':1,'operation_id':p['plan']['operation_id'],'plan_sha256':p['plan_sha256'],'files':[]}
  program=artifact.guest_program(p,r,e);compile(program,'candidate-provenance','exec');self.assertIn('provenance_contract',program);self.assertIsNone(artifact.trusted_provenance({}))

class SourcePreEffectBindingTests(unittest.TestCase):
 def test_exact_snapshot_attempt_binding_required_without_finish_call(self):
  p=BindingTests().packet();s=p['plan']['source'];s.update(config_sha256='hash',job='worker-job',attempt={'allocation':s['allocation']});p['plan']['snapshot']={'request_id':str(uuid.uuid4()),'snapshot_id':str(uuid.uuid4()),'manifest_sha256':'a'*64};p['plan_sha256']=save.sha(p['plan']);calls=[]
  proof={'attempt':s['attempt'],'manifest_sha256':'a'*64,'session_ready':True,'closing_candidate_absent':True,'pending_activity_absent':True}
  adapter=SimpleNamespace(hash='hash',c={'allocation':s['allocation'],'job':s['job'],'task':'worker','vault':{}},finish_preflight=lambda *a:calls.append(a)or proof)
  adapter.c['vault']={'enabled':True};result=source.verify_pre_effect(adapter,p);self.assertTrue(result['source_finish_effects_absent']);self.assertEqual(len(calls),1)
  proof['manifest_sha256']='b'*64
  with self.assertRaisesRegex(RuntimeError,'SNAPSHOT_OR_ATTEMPT'):source.verify_pre_effect(adapter,p)
 def test_fixed_source_command_receives_only_readonly_proof_stage(self):
  from unittest import mock
  from types import SimpleNamespace
  pipeline=object.__new__(save.OperatorPipeline);pipeline.policy={'commands':{'source_finish':['/fixed/source-finish']}};p=BindingTests().packet()
  with mock.patch.object(save.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout='{}'))as run:
   pipeline.verify_source_pre_effect(p);args,kwargs=run.call_args;self.assertEqual(args[0],['/fixed/source-finish']);self.assertEqual(json.loads(kwargs['input'])['stage'],'source_finish_pre_effect')
