import pathlib,tempfile,sys,os,unittest
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import nomad_cli
class NativeAuthReferenceTests(unittest.TestCase):
 def test_existing_token_used_only_in_native_environment(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'existing-token';p.write_text('synthetic-token');p.chmod(0o600)
   cfg={'nomad_binary':'/native/nomad','nomad_address':'https://nomad.example.test','token_file':str(p)}
   with mock.patch.dict(os.environ,{'NOMAD_SKIP_VERIFY':'true','LLM_API_KEY':'must-not-inherit'},clear=False):binary,argv,env=nomad_cli.command(cfg,['operator','api','-X','GET','/v1/nodes'])
   self.assertEqual(env['NOMAD_TOKEN'],'synthetic-token');self.assertNotIn('synthetic-token',repr(argv));self.assertNotIn('LLM_API_KEY',env);self.assertNotIn('NOMAD_SKIP_VERIFY',env)
   self.assertEqual(p.read_text(),'synthetic-token')
   for args in [['-address=https://other.test'],['operator','api','https://other.test'],['-token','other']]:
    with self.assertRaises(ValueError):nomad_cli.command(cfg,args)
 def test_symlink_or_permissive_token_not_accepted(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'token';p.write_text('synthetic');p.chmod(0o644)
   cfg={'nomad_binary':'/native/nomad','nomad_address':'https://nomad.example.test','token_file':str(p)}
   with self.assertRaises(ValueError):nomad_cli.command(cfg,[])
   p.chmod(0o600);link=pathlib.Path(d)/'link';link.symlink_to(p);cfg['token_file']=str(link)
   with self.assertRaises(OSError):nomad_cli.command(cfg,[])
