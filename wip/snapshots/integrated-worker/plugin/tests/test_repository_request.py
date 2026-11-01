import sys,pathlib,tempfile,json,unittest,subprocess
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import test_auto_placement as auto
class RepositoryRequestTests(unittest.TestCase):
 def test_request_profile_pinned_and_lost_submit_is_lookup_only(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,nodes=auto.AutoPlacementTests().fixture(d)
   for n in nodes:n['repository_profiles']={'cache':'a'*64}
   calls=[]
   def run(args,input=None):calls.append(json.loads(input)['Job']);return subprocess.CompletedProcess(args,1,b'')
   c.run=run;c.request_status=lambda _: {'lookup_only':True}
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):c.request(s,'agent-base','v1',None,2,4096,20,'tests',repository_profile='cache',repository_profile_sha256='a'*64)
   task=calls[0]['TaskGroups'][0]['Tasks'][0];self.assertEqual(task['Config']['repository_profile'],'cache');self.assertEqual(task['Config']['repository_profile_sha256'],'a'*64);self.assertTrue(any(x['LTarget']=='${attr.driver.lima.repositories.profile.cache}'and x['RTarget']=='a'*64 for x in calls[0]['Constraints']))
   self.assertTrue(c.request(s,'agent-base','v1',None,2,4096,20,'tests',repository_profile='cache',repository_profile_sha256='a'*64)['lookup_only']);self.assertEqual(len(calls),1)
   with self.assertRaisesRegex(ValueError,'CHANGED'):c.request(s,'agent-base','v1',None,2,4096,20,'tests',repository_profile='cache',repository_profile_sha256='b'*64)
 def test_incompatible_profile_node_not_used_in_envelope(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,nodes=auto.AutoPlacementTests().fixture(d);nodes[0]['repository_profiles']={'cache':'a'*64};nodes[1]['repository_profiles']={};c.run=lambda *a,**k:self.fail('must not submit')
   with self.assertRaisesRegex(RuntimeError,'NO_CURRENT_FIT'):c.request(s,'agent-base','v1',None,2,4096,20,'tests',repository_profile='cache')
   with self.assertRaisesRegex(ValueError,'PROFILE_ID'):c.request(s,'agent-base','v1',None,2,4096,20,'tests',repository_profile='https://unapproved.example.invalid')
