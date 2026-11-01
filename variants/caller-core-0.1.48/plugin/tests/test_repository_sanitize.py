import sys,pathlib,tempfile,unittest,os
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import test_template_sanitize as sanitation
class RepositorySanitizeTests(unittest.TestCase):
 def test_known_runtime_link_and_caches_removed_while_apt_and_other_tmp_retained(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,r,m=sanitation.SanitizeTests().fixture(d);p['plan']['data_composition']['retain'].append('/etc/apt');p['plan_sha256']=sanitation.save.sha(p['plan']);r['plan_sha256']=m['plan_sha256']=p['plan_sha256'];parts=root/'etc/apt/apt.conf.d';parts.mkdir(parents=True);link=parts/'99nomad-worker-repositories';link.symlink_to('/run/nomad-worker-repository-profiles/active-apt.conf');(parts/'keep.conf').write_text('APT::Get::Assume-Yes "false";')
   for name in ('nomad-workers-repository-cache','nomad-workers-repository-cache-npm-1000','nomad-workers-repository-cache-go-0'):
    path=root/'tmp'/name;path.mkdir(parents=True);(path/'metadata').write_text('https://cache.example.invalid/')
   (root/'tmp/keep.txt').write_text('unrelated data');sanitation.mod.scrub(root,p,r,m)
   self.assertFalse(link.is_symlink());self.assertTrue((parts/'keep.conf').exists());self.assertEqual(list((root/'tmp').iterdir()),[root/'tmp/keep.txt'])
 def test_regular_conflict_not_deleted_or_silently_exported(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,r,m=sanitation.SanitizeTests().fixture(d);parts=root/'etc/apt/apt.conf.d';parts.mkdir(parents=True);(parts/'99nomad-worker-repositories').write_text('operator configuration')
   with self.assertRaisesRegex(RuntimeError,'DROPIN_REVIEW'):sanitation.mod.scrub(root,p,r,m)
   self.assertTrue((root/'workspace/repo/main').exists())
