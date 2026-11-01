import importlib.util,pathlib,tempfile,json,unittest,shutil,subprocess
from unittest import mock
spec=importlib.util.spec_from_file_location('safe_update',pathlib.Path(__file__).parents[1]/'install/caller_safe_update.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class SafeUpdateTests(unittest.TestCase):
 def test_snapshot_rejects_symlink(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d);(p/'file').write_text('x');(p/'link').symlink_to(p/'file')
   with self.assertRaisesRegex(RuntimeError,'SYMLINK'):m.snapshot(p)
 def test_native_update_is_nondestructive_and_idempotent(self):
  with tempfile.TemporaryDirectory() as d:
   h=pathlib.Path(d);c=h/'.claude';old=h/'old';new=h/'new';(old/'.claude-plugin').mkdir(parents=True);(new/'plugin/.claude-plugin').mkdir(parents=True);(c/'plugins').mkdir(parents=True)
   (old/'.claude-plugin/plugin.json').write_text('{"version":"0.1.5"}');(new/'plugin/.claude-plugin/plugin.json').write_text('{"version":"0.1.6"}')
   reg={'plugins':{'nomad-workers@nomad-workers-local':[{'scope':'user','installPath':str(old)}],'other':[]}};(c/'plugins/installed_plugins.json').write_text(json.dumps(reg));(c/'settings.json').write_text('{"enabledPlugins":{"other":true}}')
   state=h/'.local/state/nomad-workers/initiators';state.mkdir(parents=True);(state/'owner.json').write_text('exact-generation');before=m.snapshot(state);calls=[]
   def native(argv,**kwargs):
    calls.append(argv)
    if argv[1:3]==['plugin','update']:
     dest=h/'cache-new';shutil.copytree(new/'plugin',dest);reg['plugins']['nomad-workers@nomad-workers-local'][0]['installPath']=str(dest);(c/'plugins/installed_plugins.json').write_text(json.dumps(reg));(old/'.orphaned_at').write_text('native-marker')
    return subprocess.CompletedProcess(argv,0,'','')
   with mock.patch.object(m.subprocess,'run',side_effect=native):
    self.assertTrue(m.update(h,'cli',new,'0.1.6')['changed']);self.assertFalse(m.update(h,'cli',new,'0.1.6')['changed'])
   self.assertEqual(m.snapshot(state),before);self.assertEqual(len(calls),3);self.assertFalse(any('remove' in x or 'uninstall' in x for x in calls))
 def test_source_and_cache_generated_bytecode_are_not_release_inputs(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d);a=p/'source';b=p/'cache';a.mkdir();b.mkdir()
   for root in (a,b):(root/'script.py').write_text('same')
   for root,name in ((a,'source'),(b,'cache')):
    (root/'__pycache__').mkdir();(root/'__pycache__'/name).write_text('generated')
   self.assertTrue(m.source_matches(b,a));(b/'script.py').write_text('changed');self.assertFalse(m.source_matches(b,a))
 def test_installer_has_no_remove_or_uninstall(self):
  s=(pathlib.Path(__file__).parents[1]/'install/caller-release.yml').read_text();self.assertNotIn('marketplace, remove',s);self.assertIn('caller_safe_update.py',s)
