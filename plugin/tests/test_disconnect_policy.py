import sys,pathlib,tempfile,unittest,json,subprocess,shutil,uuid,os
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import disconnect_policy
import test_catalog_lifecycle as fixture
class DisconnectTests(unittest.TestCase):
 def job(self,d):
  c,s=fixture.CatalogTests().client(d);c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000}]};jobs=[]
  c.run=lambda args,input=None:(jobs.append(json.loads(input)['Job'])or subprocess.CompletedProcess(args,0,b''));c.request_status=lambda sid:{'lookup_only':True}
  c.request(s,'agent-base','v1','amd64',1,1024,20,'check',task_title='Validate disconnected worker fixture',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')
  return c,s,jobs
 def test_default_native_api_shape_and_no_client_stop_or_other_policy_change(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,jobs=self.job(d);group=jobs[0]['TaskGroups'][0]
   self.assertEqual(group['Disconnect'],{'LostAfter':1800000000000,'Replace':False,'Reconcile':'keep_original'})
   self.assertNotIn('StopOnClientAfter',group['Disconnect']);self.assertEqual(group['RestartPolicy'],{'Attempts':0,'Mode':'fail'});self.assertEqual(group['ReschedulePolicy'],{'Attempts':0,'Unlimited':False})
   self.assertTrue(c.request(s,'agent-base','v1','amd64',1,1024,20,'check',task_title='Validate disconnected worker fixture',source_agent_name='fixture-coordinator',source_agent_type='fixture-client')['lookup_only']);self.assertEqual(len(jobs),1)
 def test_invalid_or_unsafe_policy_rejected(self):
  for value in (None,{}, {'lost_after_seconds':0,'replace':False,'reconcile':'keep_original'}, {'lost_after_seconds':True,'replace':False,'reconcile':'keep_original'}, {'lost_after_seconds':1800,'replace':True,'reconcile':'keep_original'}, {'lost_after_seconds':1800,'replace':False,'reconcile':'best_score'}, {'lost_after_seconds':1800,'replace':False,'reconcile':'keep_original','stop_on_client_after':0}):
   with self.subTest(value=value),self.assertRaises(ValueError):disconnect_policy.group_strategy({'disconnect_policy':value})
  self.assertEqual(disconnect_policy.group_strategy({'disconnect_policy':{'lost_after_seconds':600,'replace':False,'reconcile':'keep_original'}})['LostAfter'],600000000000)
 @unittest.skipUnless(shutil.which('nomad'),'native Nomad CLI not available')
 def test_real_nomad_207_local_validator_and_api_roundtrip_no_server(self):
  binary=shutil.which('nomad')
  with tempfile.TemporaryDirectory()as d:
   version=subprocess.run([binary,'version'],capture_output=True,text=True,check=True).stdout
   if not version.startswith('Nomad v2.0.7'):self.skipTest('Exact native Nomad 2.0.7 required')
   c,s,jobs=self.job(d);job=jobs[0];p=pathlib.Path(d)/'job.json';p.write_text(json.dumps({'Job':job}));env={'PATH':os.environ['PATH'],'HOME':d,'NOMAD_TOKEN':'','NOMAD_ADDR':'http://127.0.0.1:1','NOMAD_CLI_NO_COLOR':'1'}
   # The validate CLI falls back to its built-in exact-version structural validator
   # after refused loopback connection. No production address/token is inherited.
   r=subprocess.run([binary,'job','validate','-address=http://127.0.0.1:1','-json',str(p)],env=env,capture_output=True,text=True,timeout=10)
   self.assertEqual(r.returncode,0,r.stdout+r.stderr);self.assertIn('Job validation successful',r.stdout);self.assertIn('Driver configuration not validated',r.stdout)
   out=subprocess.run([binary,'job','run','-output','-json',str(p)],env=env,capture_output=True,text=True,check=True,timeout=10);api=json.loads(out.stdout);api=api.get('Job',api);actual=api['TaskGroups'][0]['Disconnect'];self.assertEqual(actual['LostAfter'],1800000000000);self.assertIs(actual['Replace'],False);self.assertEqual(actual['Reconcile'],'keep_original');self.assertIsNone(actual.get('StopOnClientAfter'))
   for bad in ({**job['TaskGroups'][0]['Disconnect'],'StopOnClientAfter':0},{**job['TaskGroups'][0]['Disconnect'],'LostAfter':-1}):
    job['TaskGroups'][0]['Disconnect']=bad;p.write_text(json.dumps({'Job':job}));r=subprocess.run([binary,'job','validate','-address=http://127.0.0.1:1','-json',str(p)],env=env,capture_output=True,text=True,timeout=10);self.assertNotEqual(r.returncode,0)
if __name__=='__main__':unittest.main()
