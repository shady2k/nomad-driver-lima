import importlib.util,pathlib,unittest
p=pathlib.Path(__file__).with_name('build-resume-candidate.py');s=importlib.util.spec_from_file_location('resume_package',p);m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
class PackageTests(unittest.TestCase):
 def test_required_helpers_shipped_without_baked_runtime(self):
  tracked=['plugin/scripts/secret_profiles.py','plugin/scripts/secret_operation_guest.py','plugin/scripts/workerctl.py','plugin/guest/adapter.mjs','plugin/guest/task-main.py','plugin/guest/launcher.mjs','plugin/guest/repository_exec.py','plugin/guest/repository_profile.py','plugin/tests/test_example.py','deploy/prepare-lima-only.py','deploy/roles/lima/filter_plugins/lima_transition.py']
  caller=m.package_files(tracked,'caller');self.assertTrue(m.CALLER_HELPERS<=set(caller));self.assertNotIn('plugin/guest/task-main.py',caller);self.assertNotIn('plugin/tests/test_example.py',caller)
  deployment=m.package_files(tracked,'deploy');self.assertIn('plugin/guest/repository_profile.py',deployment);self.assertNotIn('plugin/guest/repository_exec.py',deployment);self.assertIn('deploy/prepare-lima-only.py',deployment);self.assertIn('plugin/scripts/secret_profiles.py',deployment)
 def test_missing_schema_fails_build_before_distribution(self):
  with self.assertRaises(ValueError):m.package_files(['plugin/guest/repository_exec.py'],'caller')
  with self.assertRaises(ValueError):m.package_files(['deploy/roles/lima/tasks/main.yml'],'deploy')
if __name__=='__main__':unittest.main()
