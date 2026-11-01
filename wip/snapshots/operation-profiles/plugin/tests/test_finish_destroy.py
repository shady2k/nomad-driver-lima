import json,pathlib,sys,tempfile,unittest,subprocess,uuid,copy
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,storage,finalization
import test_checkpoint_activity as checkpoint_fixture
from test_catalog_lifecycle import MemoryStore
class FinishDestroyTests(unittest.TestCase):
 def fixture(self,d):
  a,s,r,row,g=checkpoint_fixture.CheckpointTests().fixture(d,True);a.c['vault']={'role':'agent-worker-prime'};a.hash=w.digest(w.canonical(a.c));state=json.loads((a.local(s)/'session.json').read_text());state['config_hash']=a.hash;w.atomic(a.local(s)/'session.json',state)
  original=a.nomad;controls=[];stop=mock.Mock()
  def nomad(args,**kw):
   if finalization.CONTROL in args:
    p=json.loads(args[-1]);controls.append(p)
    return subprocess.CompletedProcess(args,0,json.dumps({'protocol':'finish-v1','armed':p['action']=='arm','manifest_sha256':p['manifest_sha256'],'allocation':a.c['allocation'],'session':s}),'')
   result=original(args,**kw)
   if args[:2]==['job','stop']:
    stop();candidate=a.load(s)['closing_candidate'];row['TaskStates']['worker']['Events']=[{'DriverMessage':'lima task_token_revoked'},{'DriverMessage':'lima vm_stopped'},{'DriverMessage':'lima vm_disk_purged','Details':{'finalization_manifest_sha256':candidate['manifest_sha256'],'finalization_request_id':r,'finalization_snapshot_id':candidate['snapshot_id'],'cleanup_mode':'destroy','disk_purge_audit_sha256':'c'*64}}]
   return result
  a.nomad=nomad
  return a,s,r,row,g,controls,stop
 def test_default_verified_s3_finish_destroys_after_close_and_binds_receipt(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g,controls,stop=self.fixture(d);store=MemoryStore();backend=storage.binding(store.cfg)
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    a.collect(s,r,['result.txt']);out=a.finish(s,r)
    self.assertEqual(out['phase'],'finished_destroyed');self.assertTrue(out['quota_released']);self.assertTrue(out['disk_purge_requested']);self.assertEqual([p['action']for p in controls],['preflight','arm']);self.assertNotIn('finish_ack',g['calls']);self.assertEqual(stop.call_count,1)
    self.assertEqual(a.finish(s,r),out);self.assertEqual(stop.call_count,1)
 def test_local_export_requires_explicit_retain_before_side_effects(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g=checkpoint_fixture.CheckpointTests().fixture(d);a.collect(s,r,['result.txt'])
   with self.assertRaisesRegex(RuntimeError,'DURABLE_S3'):a.finish(s,r)
   self.assertEqual(a.load(s)['phase'],'ready');self.assertNotIn('close',g['calls']);self.assertEqual(a.finish(s,r,retain_for='debug')['phase'],'nomad_terminal')
 def test_unavailable_driver_preflight_keeps_runtime_ready_and_does_not_stop(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g,controls,stop=self.fixture(d);store=MemoryStore();backend=storage.binding(store.cfg)
   original=a.nomad
   def nomad(args,**kw):
    if finalization.CONTROL in args:return subprocess.CompletedProcess(args,127,'','unsupported')
    return original(args,**kw)
   a.nomad=nomad
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    a.collect(s,r,['result.txt'])
    with self.assertRaisesRegex(RuntimeError,'DRIVER_CONTROL_UNAVAILABLE'):a.finish(s,r)
   self.assertEqual(a.load(s)['phase'],'ready');self.assertNotIn('close',g['calls']);self.assertEqual(stop.call_count,0)
 def test_stale_purge_event_cannot_release_quota_or_complete_finish(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g,controls,stop=self.fixture(d);store=MemoryStore();backend=storage.binding(store.cfg);original=a.nomad
   def nomad(args,**kw):
    result=original(args,**kw)
    if args[:2]==['job','stop']:row['TaskStates']['worker']['Events'][-1]['Details']['finalization_manifest_sha256']='d'*64
    return result
   a.nomad=nomad
   with mock.patch.object(storage,'store',return_value=(store,backend)),mock.patch('workerctl.time.sleep'):
    a.collect(s,r,['result.txt'])
    with self.assertRaisesRegex(RuntimeError,'PURGE_UNCONFIRMED'):a.finish(s,r)
   self.assertEqual(a.load(s)['phase'],'finish_requested');self.assertNotIn('quota_released',a.load(s))
 def test_failed_upload_never_prepares_closes_or_arms_destroy(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g,controls,stop=self.fixture(d);store=MemoryStore();store.fail=True;backend=storage.binding(store.cfg)
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    with self.assertRaises(RuntimeError):a.collect(s,r,['result.txt'])
    with self.assertRaises(RuntimeError):a.finish(s,r)
   self.assertFalse(controls);self.assertNotIn('close',g['calls']);self.assertFalse(stop.called)
 def test_unknown_arm_retries_same_receipt_without_close_or_stop_replay(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g,controls,stop=self.fixture(d);store=MemoryStore();backend=storage.binding(store.cfg);original=a.nomad;lost=[True]
   def nomad(args,**kw):
    result=original(args,**kw)
    if finalization.CONTROL in args and json.loads(args[-1])['action']=='arm'and lost[0]:lost[0]=False;return subprocess.CompletedProcess(args,1,'','response lost after effect')
    return result
   a.nomad=nomad
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    a.collect(s,r,['result.txt'])
    with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):a.finish(s,r)
    self.assertEqual(a.load(s)['closing_candidate']['step'],'arm_intent');self.assertEqual(stop.call_count,0)
    out=a.finish(s,r);self.assertEqual(out['phase'],'finished_destroyed');self.assertEqual(g['calls'].count('close'),1);self.assertEqual(stop.call_count,1)
if __name__=='__main__':unittest.main()
