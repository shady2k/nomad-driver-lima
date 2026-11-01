import importlib.util,json,pathlib,sys,tempfile,unittest,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import template_save as save
spec=importlib.util.spec_from_file_location('sanitize',pathlib.Path(__file__).parents[2]/'deploy/template-sanitize.py');mod=importlib.util.module_from_spec(spec);spec.loader.exec_module(mod)
class SanitizeTests(unittest.TestCase):
 def fixture(self,d):
  root=pathlib.Path(d);p={'operation_id':str(uuid.uuid4()),'source':{'allocation':'source'},'method':'official_stopped_disk_clone_then_existing_seal_export_pipeline','data_composition':{'schema_version':1,'retain':['/usr/local/tool'],'remove':['/workspace/repo'],'description':'Retain tools, remove project checkout'}};packet={'plan':p,'plan_sha256':save.sha(p)}
  receipt={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'candidate_instance':'candidate',**dict.fromkeys(('source_stopped_verified','source_identity_bound','candidate_distinct','source_disk_unchanged','clone_stopped','no_rebuild'),True)}
  marker={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'candidate_instance':'candidate','source_allocation':'source','purpose':'candidate-only-sanitation'}
  for path,raw in {'usr/local/tool':b'prepared-tool','workspace/repo/main':b'project','var/tmp/nomad-workers/session':b'runtime history','root/.prime/auth':b'synthetic-key','etc/ssh/ssh_host_ed25519_key':b'-----BEGIN OPENSSH PRIVATE KEY-----'}.items():
   target=root/path;target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(raw)
  return root,packet,receipt,marker
 def test_only_candidate_fixture_scrubbed_preserves_tools_and_requires_later_seal(self):
  with tempfile.TemporaryDirectory() as d:
   root,p,r,m=self.fixture(d);result=mod.scrub(root,p,r,m)
   self.assertEqual((root/'usr/local/tool').read_bytes(),b'prepared-tool');self.assertTrue((root/'workspace/repo').is_dir());self.assertEqual(list((root/'workspace/repo').iterdir()),[]);self.assertFalse((root/'root/.prime').exists());self.assertFalse((root/'etc/ssh/ssh_host_ed25519_key').exists());self.assertTrue(result['runtime_credentials_absent']);self.assertFalse(result['identity_sealed']);self.assertFalse(result['candidate_stopped'])
 def test_wrong_marker_cannot_remove_any_data(self):
  with tempfile.TemporaryDirectory() as d:
   root,p,r,m=self.fixture(d);m['source_allocation']='other'
   with self.assertRaisesRegex(RuntimeError,'MARKER'):mod.scrub(root,p,r,m)
   self.assertTrue((root/'workspace/repo/main').exists())
 def test_preserved_secret_rejected_without_returning_contents(self):
  with tempfile.TemporaryDirectory() as d:
   root,p,r,m=self.fixture(d);(root/'usr/local/tool').write_bytes(b'LLM_API_KEY=synthetic-secret-material')
   with self.assertRaisesRegex(RuntimeError,'SECRET_PATTERN_FOUND') as e:mod.scrub(root,p,r,m)
   self.assertNotIn('synthetic-secret',str(e.exception));self.assertTrue((root/'usr/local/tool').exists())
 def test_parent_symlink_and_outside_removal_rejected_before_changes(self):
  with tempfile.TemporaryDirectory() as d:
   root,p,r,m=self.fixture(d);(root/'var/tmp').rename(root/'var/real');(root/'var/tmp').symlink_to(root/'var/real')
   with self.assertRaisesRegex(RuntimeError,'SYMLINK'):mod.scrub(root,p,r,m)
   self.assertTrue((root/'workspace/repo/main').exists())
 def test_scan_limits_fail_closed(self):
  with tempfile.TemporaryDirectory() as d:
   root,p,r,m=self.fixture(d)
   with self.assertRaisesRegex(RuntimeError,'QUOTA'):mod.scrub(root,p,r,m,max_file_bytes=1)
