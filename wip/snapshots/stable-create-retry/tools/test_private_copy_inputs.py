import importlib.util,json,pathlib,tempfile,unittest,uuid
spec=importlib.util.spec_from_file_location('private_inputs',pathlib.Path(__file__).with_name('prepare-private-copy-inputs.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class InputTests(unittest.TestCase):
 def fixture(self):
  u=lambda:str(uuid.uuid4());agent,session,allocation,native,generation=[u()for _ in range(5)]
  s={'label':'project','allocation':allocation,'source_key':'a'*64,'source_instance':'project-prepare-abc','source_session':session,'source_creator_agent':agent,'source_creator_session':native,'source_stat':{'size_bytes':4096},'copy_path':'/private/staging/disk.qcow2','copy_sha256':'b'*64,'copy_size_bytes':4096,'virtual_size_bytes':8<<30,'source_unchanged':True,'source_stopped':True,'standalone':True,'qemu_check_passed':True,'boot_executed':False,'runtime_cleanup_executed':False,'owner_evidence':{'creator_agent_id':agent,'creator_session_id':native,'creator_generation':generation,'session':session,'allocation':allocation,'native_record_matches':True,'native_record_ended':False,'guest_workspace':'/workspace/repo','guest_state_dir':'/var/tmp/nomad-workers'}}
  return {'status':'prepared_only_no_restore','sources':[s]}
 def test_conversion_does_not_claim_catalog_or_stop_receipt(self):
  report=self.fixture();out=m.descriptor(report,str(uuid.uuid4()),'workers',88);v=out['inputs']['recover-project'];self.assertEqual(v['copy_sha256'],'b'*64);self.assertEqual(v['virtual_size_bytes'],8<<30);self.assertNotIn('acceptance',json.dumps(out));self.assertNotIn('stop_receipt',json.dumps(out))
 def test_rejects_changed_owner_or_active_source(self):
  for field in ('source_stopped','source_unchanged','standalone'):
   r=self.fixture();r['sources'][0][field]=False
   with self.assertRaises(ValueError):m.descriptor(r,str(uuid.uuid4()),'workers',88)
  r=self.fixture();r['sources'][0]['owner_evidence']['creator_agent_id']=str(uuid.uuid4())
  with self.assertRaises(ValueError):m.descriptor(r,str(uuid.uuid4()),'workers',88)
if __name__=='__main__':unittest.main()
