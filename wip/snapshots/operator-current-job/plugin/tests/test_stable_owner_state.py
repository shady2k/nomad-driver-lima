import unittest,tempfile,pathlib,json,sys,os,shutil
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import session_hook
class StableOwnerTests(unittest.TestCase):
 def test_plugin_data_removal_cannot_remove_stable_owner_or_generation_history(self):
  with tempfile.TemporaryDirectory() as d:
   home=pathlib.Path(d);managed=home/'plugin-data';managed.mkdir()
   with patch.dict(os.environ,{'CLAUDE_PLUGIN_DATA':str(managed)}):
    root=session_hook.state_root(home);p,old=session_hook.record(root,{'hook_event_name':'SessionStart','session_id':'fixture-session','source':'startup'},now=10)
    shutil.rmtree(managed);self.assertTrue(pathlib.Path(p).is_file())
    _,new=session_hook.record(root,{'hook_event_name':'SessionStart','session_id':'fixture-session','source':'resume'},now=20)
    self.assertNotEqual(old['generation'],new['generation']);records=[json.loads(x.read_text()) for x in (root/'generations').glob('*.json')]
    self.assertEqual({x['generation'] for x in records},{old['generation'],new['generation']});self.assertEqual(len(records),2)
    session_hook.record(root,{'hook_event_name':'PreToolUse','session_id':'fixture-session'},now=21)
    self.assertEqual(records,[json.loads(x.read_text()) for x in (root/'generations').glob('*.json')])
 def test_missing_record_does_not_forge_observation(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);p,row=session_hook.record(root,{'hook_event_name':'PreToolUse','session_id':'fixture-session'},now=10);self.assertIsNone(p);self.assertIsNone(row);self.assertFalse(list(root.glob('*.json')))
if __name__=='__main__':unittest.main()
