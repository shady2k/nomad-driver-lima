import hashlib,json,pathlib,sys,tempfile,unittest,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import catalog_cli
import test_catalog_lifecycle as catalog_fixture
class RequestReadinessTests(unittest.TestCase):
 def fixture(self,d):
  c,s=catalog_fixture.CatalogTests().client(d);root=pathlib.Path(d)/s;root.mkdir(mode=0o700);job='worker-'+s;alloc=str(uuid.uuid4());intent={'phase':'submitted','job':job,'session_id':s,'creator_agent_id':c.c['agent_instance_id'],'creator_session_id':s};catalog_cli.atomic(root/'allocation-intent.json',intent)
  full={'ID':alloc,'ClientStatus':'running','Job':{'Version':0,'Meta':{'nomad_workers_owner':c.c['owner_id'],'creator_agent_id':c.c['agent_instance_id'],'creator_session_id':s}},'TaskStates':{'worker':{'State':'running','Restarts':0,'StartedAt':'synthetic-start'}}};calls=[]
  def api(path):calls.append(path);return [{'ID':alloc,'ClientStatus':full['ClientStatus']}]if path.startswith('/v1/job/')else full
  c.api=api;return c,s,root,full,calls
 def test_running_without_create_gives_exact_next_action_no_false_readiness(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d);r=c.request_status(s);self.assertIsNone(r['session_ready']);self.assertEqual(r['next_action'],'create');self.assertEqual(r['readiness_stage'],'session_initialization_required');self.assertFalse((root/'session.json').exists());self.assertEqual(len(calls),2);self.assertIn('--session',r['next_command'])
 def test_unknown_never_recreates_and_ready_is_historical_not_current_probe(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d);r=c.request_status(s);binding=json.loads(pathlib.Path(r['target_config']).read_text());row={'config_hash':hashlib.sha256(catalog_cli.canonical(binding)).hexdigest(),'attempt':{'allocation':full['ID'],'job':'worker-'+s,'task':'worker','job_version':0,'restarts':0,'started_at':'synthetic-start'},'phase':'create_unknown'};catalog_cli.atomic(root/'session.json',row)
   r=c.request_status(s);self.assertEqual(r['next_action'],'inspect_existing_session_no_create_retry');self.assertIsNone(r['session_ready'])
   row.update(phase='ready',record_id='durable-record');catalog_cli.atomic(root/'session.json',row);r=c.request_status(s);self.assertEqual(r['record_id'],'durable-record');self.assertTrue(r['last_observed_session_ready']);self.assertIsNone(r['session_ready']);self.assertEqual(r['next_action'],'status')
   row['attempt']['restarts']=1;catalog_cli.atomic(root/'session.json',row)
   with self.assertRaisesRegex(RuntimeError,'NO_RECREATE'):c.request_status(s)
 def test_pending_allocation_does_not_advise_create(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d);full['ClientStatus']='pending';full['TaskStates']['worker'].update(State='pending',StartedAt=None);r=c.request_status(s);self.assertEqual(r['next_action'],'request-status');self.assertIsNone(r['session_ready']);self.assertFalse((root/'session.json').exists())

 def test_runtime_metadata_is_selected_configuration_not_prime_assumption(self):
  for executable in ('fake-prime','fake-opencode'):
   with self.subTest(executable=executable),tempfile.TemporaryDirectory()as d:
    c,s,root,full,calls=self.fixture(d);c.c['guest']={'agent_argv':['/fixture/'+executable]};r=c.request_status(s);self.assertEqual(r['runtime']['name'],executable);self.assertFalse(r['runtime']['readiness_observed']);self.assertIsNone(r['runtime_ready']);self.assertIsNone(r['transport_ready']);self.assertNotIn('prime_ready',r);self.assertEqual(r['next_action'],'create')
