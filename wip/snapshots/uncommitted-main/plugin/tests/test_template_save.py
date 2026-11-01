import copy,hashlib,importlib.util,json,pathlib,sys,tempfile,threading,time,unittest,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import template_save as save
spec=importlib.util.spec_from_file_location('publisher',pathlib.Path(__file__).parents[2]/'deploy/publish-artifact.py');pub=importlib.util.module_from_spec(spec);spec.loader.exec_module(pub)
class Pipeline:
 def __init__(self,fail=None):self.calls=[];self.fail=fail
 def preflight(self,packet):
  if self.fail=='preflight':raise RuntimeError('missing operator binding')
 def preflight_retained(self,packet,receipts):self.preflight(packet)
 def stage(self,stage,packet,receipts):
  self.calls.append(stage)
  if stage==self.fail:raise RuntimeError('unknown stage outcome')
  keys={'source_finish':'source_owner_verified results_receipt_verified prime_bao_stopped task_token_revoked source_vm_stopped source_disk_retained','clone_candidate':'source_stopped_verified source_identity_bound candidate_distinct source_disk_unchanged clone_stopped no_rebuild','seal':'runtime_credentials_absent approved_data_composition_verified identity_sealed candidate_stopped source_disk_unchanged','export':'standalone_disk checksum_verified','boot':'two_independent_boots','check':'unique_identities dependency_versions_verified base_checksum_unchanged','catalog':'new_version_no_overwrite','publish':'all_objects_readback_verified','catalog_commit':'atomic_conditional_append new_version_no_overwrite'}[stage].split()
  return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],**{k:True for k in keys},**({'disk_sha256':'a'*64} if stage in ('export','publish','catalog_commit')else{})}
class SaveTests(unittest.TestCase):
 def fixture(self):
  p={'method':'official_stopped_disk_clone_then_existing_seal_export_pipeline','data_composition':{'schema_version':1,'retain':['/usr/local'],'remove':['/workspace/repo'],'description':'Keep installed tools, exclude project checkout'},'operation_id':str(uuid.uuid4()),'source':{'allocation':'source-alloc'},'template':{'template_id':'example-project-dev','version':'v1'},'actions':list(save.ACTIONS)};packet={'plan':p,'plan_sha256':save.sha(p)};approval={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'source_allocation':'source-alloc','template_id':'example-project-dev','version':'v1','actions':list(save.ACTIONS),'expires_unix':time.time()+300};return packet,approval
 def test_no_effect_before_exact_confirmation_or_preflight(self):
  with tempfile.TemporaryDirectory() as d:
   p,a=self.fixture();backend=Pipeline();wrong=dict(a,version='other')
   with self.assertRaisesRegex(RuntimeError,'CONFIRMATION'):save.execute(p,wrong,pathlib.Path(d)/'state',backend)
   self.assertEqual(backend.calls,[])
   backend=Pipeline('preflight')
   with self.assertRaisesRegex(RuntimeError,'binding'):save.execute(p,a,pathlib.Path(d)/'state',backend)
   self.assertEqual(backend.calls,[])
 def test_complete_reuses_pipeline_retains_source_and_idempotent(self):
  with tempfile.TemporaryDirectory() as d:
   p,a=self.fixture();backend=Pipeline();path=pathlib.Path(d)/'state';state=save.execute(p,a,path,backend);self.assertEqual(tuple(backend.calls),save.STAGES);self.assertEqual(state['phase'],'published_pending_activation');self.assertTrue(state['source_disk_retained']);self.assertFalse(state['source_automatic_restart']);self.assertEqual(save.execute(p,a,path,backend),state);self.assertEqual(len(backend.calls),len(save.STAGES))
 def test_unknown_upload_no_replay_retains_source_and_artifacts(self):
  with tempfile.TemporaryDirectory() as d:
   p,a=self.fixture();backend=Pipeline('publish');path=pathlib.Path(d)/'state'
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_PUBLISH'):save.execute(p,a,path,backend)
   self.assertIn('export',save.status(path)['receipts']);calls=list(backend.calls)
   with self.assertRaisesRegex(RuntimeError,'RECONCILE_NO_REPLAY'):save.execute(p,a,path,backend)
   self.assertEqual(backend.calls,calls)
 def test_parallel_same_operation_executes_once(self):
  with tempfile.TemporaryDirectory() as d:
   p,a=self.fixture();backend=Pipeline();path=pathlib.Path(d)/'state';results=[];threads=[threading.Thread(target=lambda:results.append(save.execute(p,a,path,backend)))for _ in range(2)]
   for t in threads:t.start()
   for t in threads:t.join(2)
   self.assertEqual(len(results),2);self.assertEqual(len(backend.calls),len(save.STAGES))
class FakeStore:
 def __init__(self):self.raw=b'{"schema_version":1,"templates":[]}';self.tag='one';self.puts=0;self.lost=False;self.conflict=False
 def get(self,key):return self.raw
 def stat(self,key):return {'etag':self.tag}
 def conditional_put(self,key,raw,etag):
  self.puts+=1
  if self.conflict:raise RuntimeError('412')
  assert etag==self.tag;self.raw=raw;self.tag='two'
  if self.lost:raise RuntimeError('lost response')
class CatalogTests(unittest.TestCase):
 def entry(self):return {'template_id':'example-project-dev','version':'v1','architecture':'amd64','vm_type':'qemu','disk':{'url':'file:///cache/disk','sha256':'a'*64},'runtime_yaml':{'url':'file:///cache/runtime','sha256':'b'*64},'acceptance':{'sealed':True,'independent_boots':2,'unique_identities':True}}
 def test_atomic_append_lost_ack_idempotent_no_overwrite(self):
  s=FakeStore();s.lost=True;e=self.entry();result=pub.append_catalog(s,'catalog/index',hashlib.sha256(s.raw).hexdigest(),e,True);self.assertTrue(result['atomic_conditional_append']);self.assertTrue(pub.append_catalog(s,'catalog/index','stale',e,True)['idempotent']);self.assertEqual(s.puts,1)
  e['disk']['sha256']='f'*64
  with self.assertRaisesRegex(RuntimeError,'NO_OVERWRITE'):pub.append_catalog(s,'catalog/index','stale',e,True)
 def test_unverified_backend_and_cas_conflict_fail_closed(self):
  s=FakeStore();before=s.raw
  with self.assertRaisesRegex(RuntimeError,'UNVERIFIED'):pub.append_catalog(s,'catalog/index','x',self.entry())
  self.assertEqual(s.puts,0);s.conflict=True
  with self.assertRaisesRegex(RuntimeError,'CONFLICT'):pub.append_catalog(s,'catalog/index',hashlib.sha256(s.raw).hexdigest(),self.entry(),True)
  self.assertEqual(s.raw,before);self.assertEqual(s.puts,1)

class RetryPipeline(Pipeline):
 def __init__(self,fail=None):super().__init__(fail);self.proofs=0;self.proof_valid=True
 def verify_source_pre_effect(self,packet):
  self.proofs+=1
  return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],'source_finish_effects_absent':self.proof_valid,'observation':{'session_ready':True,'closing_candidate_absent':True}}
class PreEffectRetryTests(unittest.TestCase):
 fixture=SaveTests.fixture
 def test_explicit_fresh_source_proof_audits_retry_without_manual_journal_edit(self):
  with tempfile.TemporaryDirectory()as d:
   p,a=self.fixture();backend=RetryPipeline('source_finish');path=pathlib.Path(d)/'state'
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_SOURCE_FINISH'):save.execute(p,a,path,backend)
   before=path.read_bytes()
   with self.assertRaisesRegex(RuntimeError,'RECONCILE_NO_REPLAY'):save.execute(p,a,path,backend)
   self.assertEqual(path.read_bytes(),before);self.assertEqual(backend.proofs,0)
   backend.fail=None;state=save.execute(p,a,path,backend,retry_source_pre_effect=True);self.assertEqual(state['phase'],'published_pending_activation');self.assertEqual(backend.proofs,1);self.assertEqual(state['pre_effect_retry_audit'][0]['previous_inflight'],'source_finish');self.assertEqual(backend.calls.count('source_finish'),2)
 def test_invalid_or_unavailable_proof_preserves_unknown_journal(self):
  with tempfile.TemporaryDirectory()as d:
   p,a=self.fixture();backend=RetryPipeline('source_finish');path=pathlib.Path(d)/'state'
   with self.assertRaises(RuntimeError):save.execute(p,a,path,backend)
   before=path.read_bytes();backend.proof_valid=False
   with self.assertRaisesRegex(RuntimeError,'FRESH_SOURCE'):save.execute(p,a,path,backend,retry_source_pre_effect=True)
   self.assertEqual(path.read_bytes(),before);self.assertEqual(backend.calls,['source_finish'])
 def test_no_retry_of_later_unknown_stage_even_with_source_proof(self):
  with tempfile.TemporaryDirectory()as d:
   p,a=self.fixture();backend=RetryPipeline('clone_candidate');path=pathlib.Path(d)/'state'
   with self.assertRaises(RuntimeError):save.execute(p,a,path,backend)
   before=path.read_bytes()
   with self.assertRaisesRegex(RuntimeError,'ONLY_SOURCE'):save.execute(p,a,path,backend,retry_source_pre_effect=True)
   self.assertEqual(path.read_bytes(),before);self.assertEqual(backend.proofs,0)

class CloneRetryPipeline(Pipeline):
 def __init__(self,fail=None):super().__init__(fail);self.proof_valid=True;self.retained_preflights=0
 def preflight_retained(self,packet,receipts):self.retained_preflights+=1
 def verify_clone_pre_effect(self,packet,receipts):
  return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],'candidate_instance':'agent-base-build-save-'+packet['plan']['operation_id'].replace('-',''),'source_ledger_sha256':'b'*64,'source_disk_identity':{'diffdisk':'c'*64},'source_instance':'source','source_stopped_verified':True,'source_processes_absent':True,'candidate_absent':self.proof_valid,'clone_effects_absent':True}
class CloneJournalRetryTests(unittest.TestCase):
 fixture=SaveTests.fixture
 def test_retry_clone_preserves_finish_receipt_and_never_replays_finish(self):
  with tempfile.TemporaryDirectory()as d:
   p,a=self.fixture();backend=CloneRetryPipeline('clone_candidate');path=pathlib.Path(d)/'state'
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_CLONE'):save.execute(p,a,path,backend)
   finish=save.status(path)['receipts']['source_finish'];backend.fail=None;state=save.execute(p,a,path,backend,retry_clone_pre_effect=True)
   self.assertEqual(state['receipts']['source_finish'],finish);self.assertEqual(backend.calls.count('source_finish'),1);self.assertEqual(backend.calls.count('clone_candidate'),2);self.assertEqual(backend.retained_preflights,1);self.assertEqual(state['pre_effect_retry_audit'][0]['previous_inflight'],'clone_candidate')
 def test_ambiguous_candidate_or_later_stage_keeps_unknown(self):
  for stage in ('clone_candidate','seal'):
   with self.subTest(stage=stage),tempfile.TemporaryDirectory()as d:
    p,a=self.fixture();backend=CloneRetryPipeline(stage);path=pathlib.Path(d)/'state'
    with self.assertRaises(RuntimeError):save.execute(p,a,path,backend)
    before=path.read_bytes();backend.proof_valid=False
    with self.assertRaisesRegex(RuntimeError,'FRESH_CLONE|ONLY_CLONE'):save.execute(p,a,path,backend,retry_clone_pre_effect=True)
    self.assertEqual(path.read_bytes(),before);self.assertEqual(backend.retained_preflights,0)
