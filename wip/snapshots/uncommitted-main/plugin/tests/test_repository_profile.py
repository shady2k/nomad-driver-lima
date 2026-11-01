import copy,importlib.util,json,os,pathlib,shutil,subprocess,sys,tempfile,unittest
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import repository_profiles as selection
p=selection.guest
PLATFORM={'os':'linux','distribution':'ubuntu','version':'24.04','architecture':'amd64'}
SOURCES="""# Ubuntu distribution configuration
Types: deb deb-src
URIs: https://archive.ubuntu.com/ubuntu/
Suites: noble noble-updates noble-backports
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
Architectures: amd64

Types: deb
URIs: https://security.ubuntu.com/ubuntu/
Suites: noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
"""
THIRD="""Types: deb
URIs: https://vendor.example.invalid/ubuntu/
Suites: noble
Components: stable
Signed-By: /usr/share/keyrings/vendor.gpg
"""
def profile():return {'schema_version':1,'id':'build-cache','version':'v1','apt':{'suite_uris':{'noble':'https://cache.example.invalid/apt-ubuntu/','noble-security':'https://cache.example.invalid/apt-security/'}},'npm':{'registry':'https://cache.example.invalid/npm/'},'go':{'proxy':'https://cache.example.invalid/go-modules/','distribution_base_url':'https://cache.example.invalid/go-dist/'}}
class ProfileTests(unittest.TestCase):
 def test_disabled_default_no_platform_check_no_file_or_environment_effect(self):
  self.assertEqual(selection.select({},None,{}),{'enabled':False,'profile':None,'profile_sha256':None});self.assertFalse(p.render(None,{})['enabled'])
  with mock.patch.object(p.os,'geteuid',side_effect=AssertionError('disabled must not probe host')):self.assertEqual(p.apply_payload({})['changed_files'],0)
 def test_approved_selection_normalizes_and_pins_exact_definition(self):
  cfg={'repository_profiles':{'build-cache':profile()}};a=selection.select(cfg,'build-cache',PLATFORM);self.assertEqual(a['profile_sha256'],p.digest(a['profile']));self.assertEqual(a['profile']['go']['proxy'],'https://cache.example.invalid/go-modules');cfg['repository_profile_default']='build-cache';self.assertEqual(selection.select(cfg,None,PLATFORM),a)
  cfg['repository_profiles']['build-cache']['npm']['registry']='https://other.example.invalid/npm';self.assertNotEqual(selection.select(cfg,None,PLATFORM)['profile_sha256'],a['profile_sha256'])
 def test_unapproved_or_mismatched_profile_rejected(self):
  for selected in ('unknown','https://agent.example.invalid/npm',{'registry':'x'}):
   with self.subTest(selected=selected),self.assertRaises(p.ProfileError):selection.select({'repository_profiles':{'build-cache':profile()}},selected,PLATFORM)
  value=profile();value['id']='different'
  with self.assertRaisesRegex(p.ProfileError,'ID_MISMATCH'):selection.select({'repository_profiles':{'build-cache':value}},'build-cache',PLATFORM)
 def test_http_credentials_queries_auth_fields_and_insecure_knobs_rejected(self):
  for value in ('http://cache.example.invalid','https://cache.example.invalid/\x00','https://user:synthetic-password@cache.example.invalid','https://cache.example.invalid/?token=synthetic','https://cache.example.invalid/#fragment','https://cache.example.invalid/\ntrusted=yes','https://cache.example.invalid/${TOKEN}','https://cache.example.invalid/,direct'):
   with self.subTest(value=value),self.assertRaisesRegex(p.ProfileError,'HTTPS_ENDPOINT'):p.validate(dict(profile(),npm={'registry':value}),PLATFORM)
  for tool,extra in (('npm',{'token':'synthetic'}),('npm',{'strict_ssl':False}),('go',{'sumdb':'off'}),('apt',{'trusted':True}),('npm',{'auth':{'source':'openbao'}})):
   value=profile();value[tool].update(extra)
   with self.subTest(tool=tool,extra=extra),self.assertRaises(p.ProfileError):p.validate(value,PLATFORM)
 def test_unsupported_guest_fails_before_render(self):
  for extra in ({'distribution':'debian'},{'version':'26.04'},{'os':'darwin'},{'architecture':'riscv64'}):
   with self.subTest(extra=extra),self.assertRaisesRegex(p.ProfileError,'UNSUPPORTED_GUEST'):p.render(profile(),dict(PLATFORM,**extra),{'ubuntu.sources':SOURCES})
 def test_only_mapped_suites_change_other_sources_and_verification_preserved(self):
  plan=p.render(profile(),PLATFORM,{'ubuntu.sources':SOURCES,'vendor.sources':THIRD,'custom.list':'deb https://custom.example.invalid stable main\n'},'# main preserved\n');text=plan['files'][plan['base']+'/apt/sources.list.d/ubuntu.sources'];paragraphs=[p.fields(x)[0]for x in text.strip().split('\n\n')]
  by_suite={suite:row for row in paragraphs for suite in row['suites'].split()};self.assertEqual(by_suite['noble']['uris'],'https://cache.example.invalid/apt-ubuntu/');self.assertEqual(by_suite['noble-security']['uris'],'https://cache.example.invalid/apt-security/');self.assertEqual(by_suite['noble-updates']['uris'],'https://archive.ubuntu.com/ubuntu/');self.assertEqual(by_suite['noble-backports']['uris'],'https://archive.ubuntu.com/ubuntu/')
  self.assertTrue(all(row['signed-by']=='/usr/share/keyrings/ubuntu-archive-keyring.gpg'for row in paragraphs));self.assertEqual(plan['files'][plan['base']+'/apt/sources.list.d/vendor.sources'],THIRD);self.assertEqual(plan['files'][plan['base']+'/apt/sources.list'],'# main preserved\n');self.assertNotIn('GOSUMDB',plan['environment']);self.assertNotIn('GONOSUMDB',plan['environment']);self.assertNotIn('GOAUTH',plan['environment']);self.assertNotIn('NPM_CONFIG_CACHE',plan['environment'])
 def test_missing_suite_signature_downgrade_and_duplicate_fields_rejected(self):
  for text in (SOURCES.replace('noble-security','other'),SOURCES.replace('/usr/share/keyrings/ubuntu-archive-keyring.gpg','/tmp/unapproved.gpg'),SOURCES.replace('Types: deb deb-src','Trusted: yes\nTypes: deb deb-src'),SOURCES.replace('Types: deb deb-src','Types: deb\nTypes: deb-src')):
   with self.subTest(text=text[:20]),self.assertRaises(p.ProfileError):p.render(profile(),PLATFORM,{'ubuntu.sources':text})
 def test_unmapped_apt_updates_are_not_added_to_profile(self):
  value=profile();value['apt']['suite_uris']={'noble-security':'https://cache.example.invalid/security'};plan=p.render(value,PLATFORM,{'ubuntu.sources':SOURCES});text=plan['files'][plan['base']+'/apt/sources.list.d/ubuntu.sources'];self.assertIn('Suites: noble noble-updates noble-backports',text);self.assertNotIn('apt-ubuntu',text)
 def test_apply_files_idempotent_umask_no_system_source_overwrite(self):
  plan=p.render(profile(),PLATFORM,{'ubuntu.sources':SOURCES})
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);before=os.umask(0o077)
   try:first=p.write_plan(plan,root);second=p.write_plan(plan,root)
   finally:os.umask(before)
   self.assertGreater(first['changed_files'],0);self.assertEqual(second['changed_files'],0);self.assertFalse((root/'etc').exists());self.assertEqual((root/plan['base'].lstrip('/')).stat().st_mode&0o777,0o755);self.assertEqual((root/(plan['base']+'/environment.json').lstrip('/')).stat().st_mode&0o777,0o444)
 def test_symlink_or_modified_config_fails_before_other_writes(self):
  plan=p.render(profile(),PLATFORM,{'ubuntu.sources':SOURCES})
  with tempfile.TemporaryDirectory()as d,tempfile.TemporaryDirectory()as outside:
   root=pathlib.Path(d);(root/'run').symlink_to(outside)
   with self.assertRaisesRegex(p.ProfileError,'SYMLINK'):p.write_plan(plan,root)
   self.assertEqual(list(pathlib.Path(outside).iterdir()),[]);self.assertFalse((root/'tmp').exists())
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);p.write_plan(plan,root);target=root/next(iter(plan['files'])).lstrip('/');target.chmod(0o600);target.write_text('modified')
   with self.assertRaisesRegex(p.ProfileError,'CONFIG_CHANGED'):p.write_plan(plan,root)
 def test_distribution_mirror_is_explicit_helper_not_fake_go_environment(self):
  self.assertEqual(p.distribution_url(profile(),PLATFORM,'1.26.10'),'https://cache.example.invalid/go-dist/go1.26.10.linux-amd64.tar.gz')
  for value in ('latest','1.26','../1.26.10'):
   with self.assertRaisesRegex(p.ProfileError,'EXACT_GO_VERSION'):p.distribution_url(profile(),PLATFORM,value)
  arm=dict(PLATFORM,architecture='arm64');self.assertTrue(p.distribution_url(profile(),arm,'1.26.10').endswith('.linux-arm64.tar.gz'))
 @unittest.skipUnless(shutil.which('npm'),'npm parser not installed')
 def test_native_npm_parser_uses_profile_without_network_or_personal_config(self):
  value=profile();value.pop('apt');value.pop('go');plan=p.render(value,PLATFORM)
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);p.write_plan(plan,root);npmrc=root/(plan['base']+'/npmrc').lstrip('/');empty=root/'global.npmrc';empty.write_text('');env={'HOME':d,'PATH':os.environ['PATH'],'NPM_CONFIG_USERCONFIG':str(npmrc),'NPM_CONFIG_GLOBALCONFIG':str(empty),'NPM_CONFIG_STRICT_SSL':'true'}
   for key,expected in (('registry','https://cache.example.invalid/npm/'),('strict-ssl','true')):
    result=subprocess.run([shutil.which('npm'),'config','get',key],env=env,cwd=root,capture_output=True,text=True,timeout=15);self.assertEqual(result.returncode,0);self.assertEqual(result.stdout.strip(),expected)
 def test_process_environment_hook_preserves_defaults_and_only_adds_profile(self):
  selected=selection.select({'repository_profiles':{'build-cache':profile()}},'build-cache',PLATFORM);plan=p.render(profile(),PLATFORM,{'ubuntu.sources':SOURCES})
  with tempfile.TemporaryDirectory()as d:
   receipt=p.write_plan(plan,pathlib.Path(d));ref=selection.execution_reference(selected,receipt);base={'PATH':'/usr/bin','GOSUMDB':'sum.golang.org'};env=p.execution_environment(ref,base,pathlib.Path(d),os.getuid());self.assertEqual(env['GOSUMDB'],'sum.golang.org');self.assertEqual(env['GOPROXY'],'https://cache.example.invalid/go-modules');self.assertNotIn('GOPROXY',base);self.assertEqual(p.execution_environment(None),None)
   path=pathlib.Path(d)/(plan['base']+'/environment.json').lstrip('/');path.chmod(0o600);path.write_text('{}');path.chmod(0o444)
   with self.assertRaisesRegex(p.ProfileError,'DIGEST_MISMATCH'):p.execution_environment(ref,base,pathlib.Path(d),os.getuid())
 def test_payload_receipt_mismatch_and_secret_environment_injection_rejected(self):
  self.assertEqual(selection.apply_payload({'enabled':False},{}),{'profile':None});selected=selection.select({'repository_profiles':{'build-cache':profile()}},'build-cache',PLATFORM)
  with self.assertRaisesRegex(p.ProfileError,'RECEIPT_MISMATCH'):selection.execution_reference(selected,{'enabled':True,'profile_sha256':'f'*64,'environment_sha256':'a'*64})
  plan=p.render(profile(),PLATFORM,{'ubuntu.sources':SOURCES});plan['environment']['LLM_API_KEY']='synthetic-must-not-inject'
  with tempfile.TemporaryDirectory()as d:
   receipt=p.write_plan(plan,pathlib.Path(d));ref=selection.execution_reference(selected,receipt)
   with self.assertRaisesRegex(p.ProfileError,'ENVIRONMENT_SHAPE'):p.execution_environment(ref,{},pathlib.Path(d),os.getuid())

if __name__=='__main__':unittest.main()
