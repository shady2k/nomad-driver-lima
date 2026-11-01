import hashlib,json,os,pathlib,shutil,subprocess,sys,tempfile,unittest,uuid
import test_all_models as all_models
import model_catalog
ROOT=pathlib.Path(__file__).parents[1]
class ModelControlAdapterTests(unittest.TestCase):
 def run_fixture(self,root,mode='ok',selector_change=False):
  root=pathlib.Path(root);cmeta=all_models.AllModelTests().config(root);provider=model_catalog.select_guest(cmeta)['prime_vault']['provider'];session=str(uuid.uuid4());state=root/'state';directory=state/session;directory.mkdir(parents=True,mode=0o700);models=directory/'prime-config';models.mkdir(mode=0o700);(models/'models.json').write_text(json.dumps(model_catalog.native_json(provider,session)))
  fake=root/'fake-acpx';fake.write_text('#!'+sys.executable+'\n'+'''import sys,json,os
args=sys.argv[1:]
if args==['--version']:print('0.21.0');raise SystemExit(0)
with open(os.environ['CONTROL_TRACE'],'a') as f:f.write(json.dumps(args)+'\\n')
if 'status' in args:print(json.dumps({'status':'dead' if os.environ['MODE']=='dead' else 'alive'}));raise SystemExit(0)
if 'set' in args:
 i=args.index('set');assert args[i+1]=='model'
 if os.environ['MODE']=='fail':raise SystemExit(2)
 print(json.dumps({'action':'model_set','modelId':args[i+2],'resumed':os.environ['MODE']=='resumed','acpxRecordId':'wrong' if os.environ['MODE']=='wrong-record' else 'fixture-record'}));raise SystemExit(0)
raise SystemExit(3)
''');fake.chmod(0o700);work=root/'work';work.mkdir();c={'protocol_version':1,'state_dir':str(state),'workspace':str(work),'agent_argv':['/usr/bin/true'],'node':shutil.which('node'),'launcher':str(ROOT/'guest/launcher.mjs'),'acpx_bin':str(fake),'acpx_version':'0.21.0','runtime_module':str(root/'unused-runtime.js'),'max_artifact_bytes':1048576};ch=hashlib.sha256(json.dumps(c,separators=(',',':')).encode()).hexdigest();before={'config_hash':ch,'phase':'ready','record_id':'fixture-record'};(directory/'session.json').write_text(json.dumps(before));(directory/'worker-models.json').write_text('independent-worker-sentinel')
  value=model_catalog.selectors(provider)['gpt-6.1-sol']['coordinator_acp_value'];value='["not-approved","gpt-6.1-sol"]' if selector_change else value
  payload={'config':c,'session_id':session,'action':'select_model','model_id':'gpt-6.1-sol','coordinator_value':value};trace=root/'trace';env={'PATH':os.defpath,'HOME':str(root),'CONTROL_TRACE':str(trace),'MODE':mode};p=subprocess.run([c['node'],str(ROOT/'guest/adapter.mjs')],input=json.dumps(payload),capture_output=True,text=True,env=env,timeout=10)
  calls=[json.loads(x)for x in trace.read_text().splitlines()] if trace.exists()else[];self.assertFalse((directory/'launch-permit.json').exists());self.assertEqual((directory/'worker-models.json').read_text(),'independent-worker-sentinel');self.assertFalse(any('prompt' in a for a in calls));return p,calls,json.loads((directory/'session.json').read_text())
 def test_native_control_ack_updates_only_coordinator(self):
  with tempfile.TemporaryDirectory() as d:
   p,calls,state=self.run_fixture(d);self.assertEqual(p.returncode,0,p.stdout+p.stderr);row=json.loads(p.stdout);self.assertEqual(row['type'],'model_selected');self.assertFalse(row['worker_models_changed']);self.assertEqual(state['coordinator_model']['id'],'gpt-6.1-sol');self.assertEqual(sum('set' in a for a in calls),1)
 def test_owner_loss_unknown_ack_or_selector_error_never_prompt_or_restart(self):
  for mode,changed in [('dead',False),('fail',False),('resumed',False),('wrong-record',False),('ok',True)]:
   with self.subTest(mode=mode,changed=changed),tempfile.TemporaryDirectory() as d:
    p,calls,state=self.run_fixture(d,mode,changed);self.assertNotEqual(p.returncode,0);self.assertEqual(json.loads(p.stdout)['type'],'error');self.assertNotIn('coordinator_model',state);self.assertLessEqual(sum('set' in a for a in calls),1)
