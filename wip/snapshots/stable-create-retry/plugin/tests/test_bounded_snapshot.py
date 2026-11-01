import os,pathlib,subprocess,unittest
class BoundedSnapshot(unittest.TestCase):
 def test_official_reader_bounded_exact_request_fixtures(self):
  runtime=os.environ.get('NOMAD_WORKERS_TEST_ACPX_PACKAGE')
  if not runtime:self.skipTest('official cached runtime path required')
  p=subprocess.run(['node',str(pathlib.Path(__file__).with_name('bounded_snapshot_fixture.mjs')),runtime],capture_output=True,text=True,timeout=30)
  self.assertEqual(p.returncode,0,p.stderr+p.stdout)
