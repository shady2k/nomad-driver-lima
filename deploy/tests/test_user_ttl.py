import unittest,tempfile,pathlib,json,os,subprocess,sys
HELPER=pathlib.Path(__file__).parents[1]/'nomad-user-session-ttl.py'
class UserTTLTests(unittest.TestCase):
 def test_partial_payload_preserves_secret_default_and_idempotence(self):
  for default in [True,False]:
   with tempfile.TemporaryDirectory() as d:
    root=pathlib.Path(d);state=root/'state.json';state.write_text(json.dumps({'MaxTokenTTL':'1h0m0s','Default':default,'Config':{'OIDCClientSecret':'synthetic-not-a-live-credential','Other':'retained'}}))
    cli=root/'cli';cli.write_text('#!'+sys.executable+'\n'+'''import json,pathlib,sys
p=pathlib.Path(__file__).with_name('state.json');s=json.loads(p.read_text())
if sys.argv[1:4]==['acl','auth-method','info']:
 assert '-json' not in sys.argv
 print('authentik-owner|OIDC|'+s['MaxTokenTTL']+'|'+str(s['Default']).lower()+'|local')
elif sys.argv[1:5]==['operator','api','-X','PUT']:
 payload=json.load(sys.stdin);assert set(payload)=={'Name','MaxTokenTTL','Default'};assert payload['Name']=='authentik-owner'
 s.update(MaxTokenTTL='720h0m0s',Default=payload['Default']);s['writes']=s.get('writes',0)+1;p.write_text(json.dumps(s))
 print(json.dumps({'Config':{'OIDCClientSecret':'redacted'}}))
else:raise RuntimeError('Prohibited CLI/read-redacted-roundtrip')
''');cli.chmod(0o700)
    cmd=[sys.executable,str(HELPER),'--cli',str(cli),'--method','authentik-owner','--backup-dir',str(root/'backup')]
    for changed in [True,False]:
     p=subprocess.run(cmd,capture_output=True,text=True);self.assertEqual(p.returncode,0,p.stderr);result=json.loads(p.stdout);self.assertEqual(result['changed'],changed);self.assertNotIn('redacted',p.stdout);self.assertNotIn('synthetic-not-a-live-credential',p.stdout)
    after=json.loads(state.read_text());self.assertEqual(after['Config']['OIDCClientSecret'],'synthetic-not-a-live-credential');self.assertEqual(after['Default'],default);self.assertEqual(after['writes'],1)
if __name__=='__main__':unittest.main()
