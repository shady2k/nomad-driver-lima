import ast,hashlib,json,os,pathlib,re,runpy,sys,tempfile,unittest
from unittest.mock import patch
ROOT=pathlib.Path(__file__).parents[2]
class PublicConfigTests(unittest.TestCase):
 def test_tracked_sources_have_no_installation_literal_patterns(self):
  import subprocess
  # General installation-shape rules, not a copy of an operator's private values.
  patterns=[r'https?://[^\s\"\x27]*\.inside\.',r'(?<![0-9])192\.168\.[0-9]+\.[0-9]+',r'/home/(?!OPERATOR|operator|user(?:/|$)|<)[a-z][A-Za-z0-9_-]*/',r'/Users/(?!OPERATOR|<)[a-z][A-Za-z0-9_-]*/']
  bad=[]
  for name in subprocess.check_output(['git','ls-files'],cwd=ROOT,text=True).splitlines():
   if name==str(pathlib.Path(__file__).relative_to(ROOT)):continue
   try:raw=(ROOT/name).read_text()
   except (OSError,UnicodeError):continue
   if any(re.search(pattern,raw)for pattern in patterns):bad.append(name)
  self.assertEqual(bad,[],'Installation values belong in external private config')
 def test_operator_wrapper_uses_private_config_without_output(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);token=root/'bootstrap.json';token.write_text(json.dumps({'SecretID':'synthetic-test-only'}));token.chmod(0o600)
   cfg=root/'operator.json';cfg.write_text(json.dumps({'bootstrap_file':str(token),'nomad_address':'https://scheduler.example.invalid','nomad_binary':'/opt/example/nomad'}));cfg.chmod(0o600)
   with patch.dict(os.environ,{'NOMAD_WORKERS_OPERATOR_CONFIG':str(cfg)}),patch.object(sys,'argv',['wrapper','node','status']),patch('os.execve')as execve:
    runpy.run_path(str(ROOT/'deploy/nomad-operator-cli.py'),run_name='fixture')
   args=execve.call_args.args;self.assertEqual(args[:2],('/opt/example/nomad',['nomad','node','status']));self.assertEqual(args[2]['NOMAD_ADDR'],'https://scheduler.example.invalid');self.assertEqual(args[2]['NOMAD_TOKEN'],'synthetic-test-only')
 def test_changed_python_helpers_parse_without_running(self):
  for name in ('nomad-vault-client.py','nomad-vault-server.py','openbao-nomad-state.py','results-s3-caller.py','results-s3-access.py','final-worker-acceptance.py','llm-record-state.py','nomad-node-name.py'):
   ast.parse((ROOT/'deploy'/name).read_text())
if __name__=='__main__':unittest.main()
