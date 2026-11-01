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
 def test_exact_approved_temp_cache_roots_keep_modes_and_do_not_follow_child_symlinks(self):
  with tempfile.TemporaryDirectory()as d,tempfile.TemporaryDirectory()as outside:
   root,p,r,m=self.fixture(d);p['plan']['data_composition']['remove']+=['/tmp','/var/tmp','/var/cache/apt/archives'];p['plan_sha256']=save.sha(p['plan']);r['plan_sha256']=m['plan_sha256']=p['plan_sha256']
   target=pathlib.Path(outside)/'keep';target.write_text('outside')
   for guest in mod.CLEAN_CONTENT_ROOTS:
    directory=root/guest.lstrip('/');directory.mkdir(parents=True,exist_ok=True);directory.chmod(0o1777 if guest in('/tmp','/var/tmp')else 0o755);(directory/'cache-file').write_text('discard');(directory/'outside-link').symlink_to(target)
   modes={v:(root/v.lstrip('/')).stat().st_mode for v in mod.CLEAN_CONTENT_ROOTS};mod.scrub(root,p,r,m)
   for guest in mod.CLEAN_CONTENT_ROOTS:self.assertEqual(list((root/guest.lstrip('/')).iterdir()),[]);self.assertEqual((root/guest.lstrip('/')).stat().st_mode,modes[guest])
   self.assertEqual(target.read_text(),'outside')
 def test_cleanup_root_symlink_fails_before_any_mutation(self):
  with tempfile.TemporaryDirectory()as d,tempfile.TemporaryDirectory()as outside:
   root,p,r,m=self.fixture(d);p['plan']['data_composition']['remove']+=['/tmp'];p['plan_sha256']=save.sha(p['plan']);r['plan_sha256']=m['plan_sha256']=p['plan_sha256'];(root/'tmp').symlink_to(outside)
   with self.assertRaisesRegex(RuntimeError,'CLEANUP_ROOT_SYMLINK'):mod.scrub(root,p,r,m)
   self.assertTrue((root/'workspace/repo/main').exists())
 def test_realistic_large_library_streamed_without_read_bytes_or_blind_skip(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,r,m=self.fixture(d);lib=root/'usr/lib/libLLVM-fixture.so';lib.parent.mkdir(parents=True);size=143545784
   with lib.open('wb')as stream:stream.seek(size-1);stream.write(b'\0')
   p['plan']['data_composition']['retain'].append('/usr/lib');p['plan_sha256']=save.sha(p['plan']);r['plan_sha256']=m['plan_sha256']=p['plan_sha256']
   from unittest import mock
   with mock.patch.object(pathlib.Path,'read_bytes',side_effect=AssertionError('large scan must stream')):result=mod.scrub(root,p,r,m)
   self.assertGreaterEqual(result['scan_bytes'],size);self.assertTrue(lib.is_file())
 def test_secret_cross_chunk_and_unbounded_whitespace_still_detected(self):
  for payload in (b'\0'*(mod.SCAN_CHUNK_BYTES-4)+b'LLM_API_KEY=synthetic-secret-value',b'LLM_API_KEY='+b' '*(mod.SCAN_CHUNK_BYTES*2)+b'synthetic-secret-value'):
   with self.subTest(kind=len(payload)),tempfile.TemporaryDirectory()as d:
    root,p,r,m=self.fixture(d);(root/'usr/local/tool').write_bytes(payload)
    with self.assertRaisesRegex(RuntimeError,'SECRET_PATTERN_FOUND'):mod.scrub(root,p,r,m)

 def test_token_style_lexical_boundaries_not_identifier_substrings(self):
  for value in (b'fibos.LongGoIdentifierName(',b'name_hvs.LongGoIdentifierName(',b'embeddedhvb.abcdefghijklmnopqrst('):self.assertIsNone(mod.SECRET.search(value))
  for value in (b'"s.abcdefghijklmnopqrst"',b"'hvs.abcdefghijklmnopqrst'",b'\t h vb ignored h vb ignored s.abcdefghijklmnopqrst \n',b'hvb.abcdefghijklmnopqrst'):
   self.assertIsNotNone(mod.SECRET.search(value))
 def test_quoted_and_whitespace_token_across_chunk_still_detected(self):
  for token in (b'"s.abcdefghijklmnopqrst"',b'\t hvs.abcdefghijklmnopqrst \n'):
   with tempfile.TemporaryDirectory()as d:
    root,p,r,m=self.fixture(d);(root/'usr/local/tool').write_bytes(b'\0'*(mod.SCAN_CHUNK_BYTES-6)+token)
    with self.assertRaisesRegex(RuntimeError,'SECRET_PATTERN_FOUND'):mod.scrub(root,p,r,m)

class ProvenanceTests(unittest.TestCase):
 fixture=SanitizeTests.fixture
 def evidence(self,p,path,raw,kind='official_archive'):
  import hashlib
  return {'schema_version':1,'operation_id':p['plan']['operation_id'],'plan_sha256':p['plan_sha256'],'files':[{'path':path,'sha256':hashlib.sha256(raw).hexdigest(),'provenance':{'kind':kind,'evidence_sha256':'a'*64}}]}
 def test_exact_public_artifact_still_scanned_and_reported(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,r,m=self.fixture(d);raw=b'public fixture -----BEGIN PRIVATE KEY-----';(root/'usr/local/tool').write_bytes(raw)
   evidence=self.evidence(p,'/usr/local/tool',raw);result=mod.scrub(root,p,r,m,trusted_provenance=evidence)
   self.assertEqual(result['verified_public_artifact_match_files'],1);self.assertEqual(result['trusted_provenance_sha256'],save.sha(evidence));self.assertGreaterEqual(result['scan_bytes'],len(raw))
 def test_changed_public_file_or_unlisted_sibling_still_rejected(self):
  for sibling in (False,True):
   with self.subTest(sibling=sibling),tempfile.TemporaryDirectory()as d:
    root,p,r,m=self.fixture(d);raw=b'public fixture -----BEGIN PRIVATE KEY-----';(root/'usr/local/tool').write_bytes(raw)
    evidence=self.evidence(p,'/usr/local/tool',raw)
    target=root/'usr/local/other'if sibling else root/'usr/local/tool';target.write_bytes(raw+b'\nLLM_API_KEY=synthetic-secret-material')
    if sibling:
     p['plan']['data_composition']['retain'].append('/usr/local/other');p['plan_sha256']=save.sha(p['plan']);r['plan_sha256']=m['plan_sha256']=evidence['plan_sha256']=p['plan_sha256']
    with self.assertRaisesRegex(RuntimeError,'SECRET_PATTERN'):mod.scrub(root,p,r,m,trusted_provenance=evidence)
 def test_no_user_config_or_unbound_provenance_waiver(self):
  for path,wrong_op in (('/etc/config',False),('/home/user/key',False),('/usr/local/tool',True)):
   with self.subTest(path=path,wrong_op=wrong_op),tempfile.TemporaryDirectory()as d:
    root,p,r,m=self.fixture(d);evidence=self.evidence(p,path,b'fixture')
    if wrong_op:evidence['operation_id']='other'
    with self.assertRaisesRegex(RuntimeError,'PROVENANCE|PATH_REJECTED'):mod.scrub(root,p,r,m,trusted_provenance=evidence)
    self.assertTrue((root/'workspace/repo/main').exists())
