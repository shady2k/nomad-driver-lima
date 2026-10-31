import unittest,tempfile,pathlib,json,uuid,sys,subprocess
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,template_draft as td
import test_guest_exec as legacy
class DraftTests(unittest.TestCase):
 def fixture(self,d):
  a,s,row,owner=legacy.ExecTests().fixture(d);rid=str(uuid.uuid4());catalog=pathlib.Path(d)/'catalog';catalog.write_text(json.dumps({'schema_version':1,'templates':[]}));a.c.update(catalog_file=str(catalog),catalog_sha256=w.digest(catalog.read_bytes()));a.hash=w.digest(w.canonical(a.c));w.atomic(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row)})
  row['Job']['TaskGroups'][0]['Tasks'][0]['Config']={'architecture':'amd64','disk_gib':20}
  recipe={'schema_version':1,'purpose':'build-tools','packages':[{'manager':'apt','name':'make','version':'4.3'}],'steps':[{'label':'install-make','argv':['sudo','apt-get','install','make=4.3']}]}
  checks={'schema_version':1,'environment_ready':True,'checks':[{'name':'tool-version','status':'pass','version':'4.3'}],'model_smoke':{'status':'pass','request_id':rid,'provider':'fixture','model':'fixture','usage':{'input_tokens':1,'output_tokens':1}}}
  output=a.local(s)/'export';output.mkdir(mode=0o700);files=[]
  for i,(name,value) in enumerate([('recipe.json',recipe),('checks.json',checks)]):
   raw=w.canonical(value);(output/(str(i)+'.bin')).write_bytes(raw);files.append({'path':name,'size':len(raw),'sha256':w.digest(raw),'local_file':str(i)+'.bin'})
  manifest={'version':1,'session_id':s,'request_id':rid,'actual_backend':'coordinator-local','terminal':{'stopReason':'end_turn'},'files':files};w.atomic(output/'manifest.json',manifest);w.atomic(a.local(s)/(rid+'.json'),{'phase':'terminal','export_verified':True,'manifest_dir':'export','manifest_sha256':w.digest(w.canonical(manifest))})
  return a,s,rid,row,recipe,checks,output
 def test_draft_is_immutable_bound_review_only_and_no_runtime_actions(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d);draft=str(uuid.uuid4())
   with patch.object(a,'nomad',side_effect=AssertionError('Unexpected runtime action')):
    doc=td.prepare(a,s,draft,'build-tools','v1',r,'recipe.json','checks.json');self.assertEqual(doc,td.prepare(a,s,draft,'build-tools','v1',r,'recipe.json','checks.json'));self.assertEqual(doc,td.inspect_draft(a,s,draft))
   self.assertTrue(doc['confirmation_required']);self.assertTrue(all(v==0 for v in doc['manifest']['actions_performed'].values()));self.assertIsNone(doc['manifest']['proposed_template']['disk_sha256']);self.assertEqual(doc['manifest']['state'],'awaiting_operator_confirmation')
   with self.assertRaisesRegex(RuntimeError,'IMMUTABLE_DRAFT_CONTENT_CHANGED'):td.prepare(a,s,draft,'build-tools','v2',r,'recipe.json','checks.json')
 def test_owner_and_mutable_input_rejected(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d);row['Job']['Meta']['creator_generation']='wrong'
   with self.assertRaisesRegex(RuntimeError,'OWNER_MISMATCH'):td.prepare(a,s,str(uuid.uuid4()),'tools','v1',r,'recipe.json','checks.json')
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d);(out/'0.bin').write_text('{}')
   with self.assertRaisesRegex(RuntimeError,'CORRUPT'):td.prepare(a,s,str(uuid.uuid4()),'tools','v1',r,'recipe.json','checks.json')
 def test_secrets_unpinned_and_incomplete_smoke(self):
  value={'schema_version':1,'purpose':'tools','packages':[{'manager':'apt','name':'make','version':'4.3'}],'steps':[{'label':'install','argv':['sh','-c','API_KEY=synthetic-secret cmd']}]}
  with self.assertRaisesRegex(ValueError,'SECRET'):td.recipe(value)
  value['steps'][0]['argv']=['true'];value['packages'][0]['version']='latest'
  with self.assertRaisesRegex(ValueError,'UNPINNED'):td.recipe(value)
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d)
   with self.assertRaisesRegex(ValueError,'TERMINAL_SUCCESS'):td.checks(checks,r,{'stopReason':'max_tokens'})
 def test_existing_version_and_tampered_packet_rejected(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d);draft=str(uuid.uuid4());td.prepare(a,s,draft,'tools','v1',r,'recipe.json','checks.json');p=td.draft_path(a,s,draft);doc=json.loads(p.read_text());doc['manifest']['proposed_template']['version']='forged';p.write_text(json.dumps(doc))
   with self.assertRaisesRegex(RuntimeError,'MANIFEST_CHANGED'):td.inspect_draft(a,s,draft)
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,recipe,checks,out=self.fixture(d);c=pathlib.Path(a.c['catalog_file']);c.write_text(json.dumps({'templates':[{'template_id':'tools','version':'v1','architecture':'amd64'}]}));a.c['catalog_sha256']=w.digest(c.read_bytes());a.hash=w.digest(w.canonical(a.c));w.atomic(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row)})
   with self.assertRaisesRegex(ValueError,'ALREADY_EXISTS'):td.prepare(a,s,str(uuid.uuid4()),'tools','v1',r,'recipe.json','checks.json')
 def test_agent_cli_has_no_approval_or_publication_switch(self):
  cli=pathlib.Path(__file__).parents[1]/'scripts/workerctl.py'
  p=subprocess.run([sys.executable,str(cli),'--config','/dev/null','prepare-template','--session',str(uuid.uuid4()),'--draft',str(uuid.uuid4()),'--request',str(uuid.uuid4()),'--template','tools','--template-version','v1','--recipe-path','recipe.json','--checks-path','checks.json','--publish'],capture_output=True,text=True)
  self.assertEqual(p.returncode,2);self.assertIn('unrecognized arguments: --publish',p.stderr)
if __name__=='__main__':unittest.main()
