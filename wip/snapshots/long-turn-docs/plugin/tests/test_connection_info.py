import sys,pathlib,tempfile,json,unittest,shlex
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import connection_info,workerctl as w
import test_guest_exec as fixtures
class ConnectionTests(unittest.TestCase):
 def fixture(self,d):
  a,s,row,owner=fixtures.ExecTests().fixture(d);row['NodeID']='node-a';row['TaskStates']['worker']['Events']=[{'Details':{'instance':'project-build-a1b2'}}]
  file=pathlib.Path(d)/'profiles.json';w.atomic(file,{'schema_version':1,'nodes':{'node-a':{'id':'human-shell','ssh_alias':'existing-worker','shell_argv':['limactl','shell','{instance}'],'environment_verified':True}}});return a,s,row,file
 def test_render_only_never_execute_or_read_keys(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,p=self.fixture(d)
   with patch('subprocess.run')as run:
    out=connection_info.connection_info(a,s,p);run.assert_not_called()
   self.assertEqual(shlex.split(out['command']),out['ssh_argv']);self.assertEqual(out['ssh_argv'],['ssh','-t','existing-worker','limactl shell project-build-a1b2']);self.assertFalse(out['access_verified']);self.assertFalse(out['access_grants_changed'])
 def test_missing_profile_node_attempt_and_wrong_owner(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,row,p=self.fixture(d);self.assertEqual(connection_info.connection_info(a,s)['status'],'unavailable')
   row['NodeID']='other';self.assertEqual(connection_info.connection_info(a,s,p)['missing_fields'],['node_access_profile']);row['NodeID']='node-a'
   row['TaskStates']['worker']['Restarts']=1
   with self.assertRaisesRegex(RuntimeError,'ATTEMPT'):connection_info.connection_info(a,s,p)
   row['TaskStates']['worker']['Restarts']=0;row['Job']['Meta']['worker_session_id']='other'
   with self.assertRaisesRegex(RuntimeError,'OWNER'):connection_info.connection_info(a,s,p)
 def test_profile_unsafe_alias_secret_symlink_and_ambiguous_instance(self):
  for field,value in [('ssh_alias','-oProxyCommand=bad'),('shell_argv',['ssh','-i','/example/private-key','{instance}']),('shell_argv',['env','API_KEY=private','limactl','shell','{instance}']),('environment_verified',False)]:
   with tempfile.TemporaryDirectory()as d:
    a,s,row,p=self.fixture(d);doc=json.loads(p.read_text());doc['nodes']['node-a'][field]=value;w.atomic(p,doc)
    if field=='environment_verified':self.assertEqual(connection_info.connection_info(a,s,p)['status'],'unavailable')
    else:
     with self.assertRaises(ValueError):connection_info.connection_info(a,s,p)
  with tempfile.TemporaryDirectory()as d:
   a,s,row,p=self.fixture(d);link=p.with_name('link');link.symlink_to(p)
   with self.assertRaises((RuntimeError,ValueError)):connection_info.connection_info(a,s,link)
   row['TaskStates']['worker']['Events'].append({'Details':{'instance':'other-vm'}});self.assertEqual(connection_info.connection_info(a,s,p)['status'],'unavailable')
if __name__=='__main__':unittest.main()
