import sys,pathlib,tempfile,json,uuid,unittest,subprocess,time
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,guest_exec,activity
import test_guest_exec as fixtures
class AbandonTests(unittest.TestCase):
 def setup_case(self,d):
  a,s,row,owner=fixtures.ExecTests().fixture(d);eid=str(uuid.uuid4())
  with patch.object(a,'nomad',side_effect=subprocess.TimeoutExpired('fixture',1)):
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN'):guest_exec.legacy_execute(a,s,eid,['id'])
  return a,s,row,owner,eid
 def approve(self,a,s,eid,d):
  p=guest_exec.abandon_plan(a,s,eid)
  for k in ('read_only_command','no_related_processes_observed','acknowledge_unknown_outcome','allow_new_activity'):p[k]=True
  path=pathlib.Path(d)/'approval.json';w.atomic(path,p);return path,p
 def test_unknown_preserved_no_replay_new_activity(self):
  with tempfile.TemporaryDirectory() as d,patch.object(guest_exec,'_identity',return_value={'machine_id':'machine','boot_id':'boot'}):
   a,s,row,o,e=self.setup_case(d);p,approval=self.approve(a,s,e,d)
   with patch.object(a,'nomad') as transport:
    result=guest_exec.abandon(a,s,e,p);self.assertEqual(result['phase'],'exec_abandoned');self.assertNotIn('exit_code',result);self.assertEqual(result['command_outcome'],'unknown')
    self.assertEqual(a.exec(s,e,['id']),result);self.assertEqual(guest_exec.abandon(a,s,e,p),result);transport.assert_not_called()
   audit=json.loads((a.local(s)/('exec-'+e+'.abandon.json')).read_text());self.assertEqual(audit['original_receipt']['phase'],'exec_unknown');self.assertFalse(audit['command_replayed']);activity.pending(a,s)
 def test_reject_changed_approval_identity_owner_attempt(self):
  for kind in ('command','expired','boot','owner','attempt','unsigned'):
   with self.subTest(kind=kind),tempfile.TemporaryDirectory() as d,patch.object(guest_exec,'_identity',return_value={'machine_id':'machine','boot_id':'boot'}):
    a,s,row,o,e=self.setup_case(d);p,v=self.approve(a,s,e,d)
    if kind=='command':v['command_sha256']='0'*64
    if kind=='expired':v['expires_unix']=time.time()-1
    if kind=='boot':v['guest_identity']['boot_id']='other'
    if kind=='owner':o['ended']=True;w.atomic(a.c['creator_record'],o)
    if kind=='attempt':row['TaskStates']['worker']['Restarts']=1
    if kind=='unsigned':v['allow_new_activity']=False
    w.atomic(p,v)
    with self.assertRaises(RuntimeError):guest_exec.abandon(a,s,e,p)
    self.assertEqual(json.loads((a.local(s)/('exec-'+e+'.json')).read_text())['phase'],'exec_unknown');self.assertFalse((a.local(s)/('exec-'+e+'.abandon.json')).exists())
 def test_audit_first_crash_retry(self):
  with tempfile.TemporaryDirectory() as d,patch.object(guest_exec,'_identity',return_value={'machine_id':'machine','boot_id':'boot'}):
   a,s,row,o,e=self.setup_case(d);p,v=self.approve(a,s,e,d);original=w.atomic
   def crash(path,value):
    if pathlib.Path(path).name=='exec-'+e+'.json':raise OSError('simulated crash')
    return original(path,value)
   with patch.object(w,'atomic',side_effect=crash):
    with self.assertRaises(OSError):guest_exec.abandon(a,s,e,p)
   audit_path=a.local(s)/('exec-'+e+'.abandon.json');before=audit_path.read_bytes();self.assertEqual(guest_exec.status(a,s,e)['phase'],'exec_unknown')
   guest_exec.abandon(a,s,e,p);self.assertEqual(before,audit_path.read_bytes())
if __name__=='__main__':unittest.main()
