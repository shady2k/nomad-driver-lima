import copy,json,pathlib,sys,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import secret_profiles as s

def fixture():
 return {'schema_version':1,'profiles':[{'id':'model-test','version':'v1','description':'Test client access','scope':{'namespaces':['workers'],'tasks':['worker']},'clients':{'prime':{'executable':'/opt/client/prime','work_root':'/workspace'},'opencode':{'executable':'/opt/client/opencode','work_root':'/workspace'}},'bindings':[{'record':'models/data/client','field':'API_KEY','env':'LLM_API_KEY'}]}]}
class ProfilesTests(unittest.TestCase):
 def test_two_clients_use_same_profile_without_new_auth_or_revoke(self):
  c=fixture()
  for client in ('prime','opencode'):
   argv=['/opt/client/'+client,'test'];p=s.select(c,'model-test',s.digest(c),'workers','worker',client,'/workspace/project',argv);h=s.compile_supervisor(p,'https://secrets.example.invalid','/dev/shm/task/vault_token',argv)
   self.assertIn('restart_on_secret_changes = "never"',h);self.assertIn('env_template "LLM_API_KEY"',h);self.assertNotIn('revoke',h);self.assertNotIn('token/create',h);self.assertNotIn('listener',h);self.assertNotIn('sink',h)
 def test_profile_scope_client_digest_and_workspace(self):
  c=fixture();args=[c,'model-test',s.digest(c),'workers','worker','prime','/workspace/project',['/opt/client/prime']]
  for index,value in ((1,'arbitrary'),(2,'0'*64),(3,'other'),(4,'other'),(5,'shell'),(6,'/workspace-escape'),(7,['/bin/sh'])):
   bad=args.copy();bad[index]=value
   with self.assertRaises(s.ProfileError):s.select(*bad)
 def test_mappings_cannot_override_credentials_loader_or_template(self):
  for field,value in [('env','VAULT_TOKEN'),('env','NOMAD_TOKEN'),('env','LD_PRELOAD'),('env','NODE_OPTIONS'),('record','models/../admin'),('field','KEY }} {{ secret "other"')]:
   c=fixture();c['profiles'][0]['bindings'][0][field]=value
   with self.assertRaises(s.ProfileError):s.validate(c)
 def test_public_listing_omits_records_fields_and_executable_paths(self):
  c=fixture();v=s.public_profiles(c,'workers','worker');raw=json.dumps(v)
  for value in ('models/data/client','API_KEY','/opt/client','/workspace'):self.assertNotIn(value,raw)
  self.assertEqual(s.public_profiles(c,'other','worker'),[])
 def test_no_file_delivery_or_arbitrary_extra_parameters(self):
  c=fixture();c['profiles'][0]['bindings'][0]['file']='/workspace/key'
  with self.assertRaises(s.ProfileError):s.validate(c)
 def test_provider_urls_cannot_embed_credentials_or_disable_tls(self):
  for url in ('http://secrets.example.invalid','https://user:password@secrets.example.invalid','https://secrets.example.invalid/?token=value'):
   with self.assertRaises(s.ProfileError):s.compile_supervisor(fixture()['profiles'][0],url,'/dev/shm/task/vault_token',['/opt/client/prime'])
if __name__=='__main__':unittest.main()
