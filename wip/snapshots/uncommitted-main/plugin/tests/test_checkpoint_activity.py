import sys,pathlib,tempfile,json,uuid,unittest,base64,threading,concurrent.futures,time,subprocess
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,activity,storage
import test_guest_exec as legacy
from test_catalog_lifecycle import MemoryStore
class CheckpointTests(unittest.TestCase):
 def fixture(self,d,s3=False):
  a,s,row,owner=legacy.ExecTests().fixture(d);rid=str(uuid.uuid4());guest={'data':b'preparation','calls':[],'prepared':False}
  if s3:
   a.c['storage']={'backend':'s3'};a.c['diagnostics_required']=False;a.hash=w.digest(w.canonical(a.c));w.atomic(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row)})
  w.atomic(a.local(s)/(rid+'.json'),{'session_id':s,'request_id':rid,'phase':'terminal','acpx_request_id':rid,'terminal':{'stopReason':'end_turn'}})
  def remote(session,action,**kw):
   guest['calls'].append(action)
   if action=='artifact':yield {'data':base64.b64encode(guest['data']).decode(),'size':len(guest['data']),'sha256':w.digest(guest['data'])}
   elif action=='prepare_finish':
    for f in kw['files']:assert f['sha256']==w.digest(guest['data'])
    guest['prepared']=True;yield {'type':'finish_ready'}
   else:yield {'type':'ok'}
  def nomad(args,**kw):
   if args[:2]==['alloc','exec']:
    guest['data']=b'project-results';return subprocess.CompletedProcess(args,0,json.dumps({'version':1,'outcome':'completed','exit_code':0,'stdout':'done','stderr':'','stdout_truncated':False,'stderr_truncated':False}),'')
   if args[:2]==['job','allocs']:return subprocess.CompletedProcess(args,0,json.dumps([row]),'')
   if args[:2]==['job','stop']:
    row.update(DesiredStatus='stop',ClientStatus='complete');row['TaskStates']['worker']['State']='dead';return subprocess.CompletedProcess(args,0,'','')
   raise AssertionError('Unexpected transport')
  a.remote=remote;a.nomad=nomad
  return a,s,rid,row,guest
 def test_prepare_checkpoint_then_work_fresh_snapshot_and_close(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);first=a.collect(s,r,['result.txt']);self.assertEqual(first['checkpoint']['activity_revision'],0)
   a.exec(s,str(uuid.uuid4()),['fixture-command']);self.assertEqual(activity.revision(a,s),1);self.assertEqual(a.result(s,r),first)
   with self.assertRaisesRegex(RuntimeError,'HISTORICAL_CHECKPOINT'):a.collect(s,r,['result.txt'])
   with self.assertRaisesRegex(RuntimeError,'STALE_OR_LEGACY'):a.finish(s,r)
   self.assertNotIn('prepare_finish',g['calls'])
   snap=str(uuid.uuid4());latest=a.collect(s,r,['result.txt'],snap);self.assertEqual(latest['checkpoint']['activity_revision'],1);self.assertNotEqual(latest['files'][0]['sha256'],first['files'][0]['sha256']);self.assertEqual(a.result(s,r),first)
   self.assertEqual(a.finish(s,r,snap)['phase'],'nomad_terminal')
   with self.assertRaisesRegex(RuntimeError,'NOT_READY_OR_FINISHING'):a.exec(s,str(uuid.uuid4()),['must-not-run'])
 def test_finalization_unknown_fences_new_work_and_retries_same_candidate(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);a.collect(s,r,['result.txt']);original=a.remote;lost=[True]
   def remote(session,action,**kw):
    if action=='prepare_finish' and lost[0]:lost[0]=False;g['prepared']=True;raise RuntimeError('TRANSPORT_UNKNOWN')
    return original(session,action,**kw)
   a.remote=remote
   with self.assertRaisesRegex(RuntimeError,'TRANSPORT_UNKNOWN'):a.finish(s,r)
   self.assertEqual(a.load(s)['phase'],'finalizing')
   with self.assertRaisesRegex(RuntimeError,'NOT_READY'):a.exec(s,str(uuid.uuid4()),['must-not-run'])
   with self.assertRaisesRegex(RuntimeError,'CANDIDATE_CHANGED'):a.finish(s,r,str(uuid.uuid4()))
   self.assertEqual(a.finish(s,r)['phase'],'nomad_terminal');self.assertEqual(g['calls'].count('close'),1)
 def test_concurrent_finish_wins_before_new_activity(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);a.collect(s,r,['result.txt']);entered=threading.Event();release=threading.Event();original=a.remote
   def remote(session,action,**kw):
    if action=='prepare_finish':entered.set();self.assertTrue(release.wait(3))
    return original(session,action,**kw)
   a.remote=remote
   with concurrent.futures.ThreadPoolExecutor(2) as pool:
    finishing=pool.submit(a.finish,s,r);self.assertTrue(entered.wait(2));working=pool.submit(a.exec,s,str(uuid.uuid4()),['must-not-run']);time.sleep(.05);self.assertFalse(working.done());release.set();self.assertEqual(finishing.result()['phase'],'nomad_terminal')
    with self.assertRaisesRegex(RuntimeError,'NOT_READY'):working.result()
 def test_receipt_before_pointer_crash_recovers_revision_without_replay(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);a.collect(s,r,['result.txt']);eid=str(uuid.uuid4());original=w.atomic
   def crash(path,value):
    if pathlib.Path(path).name=='session.json' and value.get('activity_revision')==1:raise RuntimeError('INJECTED_POINTER_WRITE_FAILURE')
    return original(path,value)
   with mock.patch.object(w,'atomic',side_effect=crash):
    with self.assertRaisesRegex(RuntimeError,'INJECTED'):a.exec(s,eid,['fixture-command'])
   fresh=w.Adapter(pathlib.Path(d)/'config');fresh.target=a.target;fresh.event=a.event;fresh.nomad=a.nomad;fresh.operations_policy_file=a.operations_policy_file
   self.assertEqual(activity.revision(fresh,s),1);self.assertEqual(fresh.exec(s,eid,['fixture-command'])['phase'],'exec_unknown');self.assertEqual(g['data'],b'preparation')
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_ACTIVITY'):a.exec(s,str(uuid.uuid4()),['must-not-run'])
 def test_work_wins_race_and_old_finish_cannot_close_it(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);a.collect(s,r,['result.txt']);entered=threading.Event();release=threading.Event();original=a.nomad
   def nomad(args,**kw):
    if args[:2]==['alloc','exec']:entered.set();self.assertTrue(release.wait(3))
    return original(args,**kw)
   a.nomad=nomad
   with concurrent.futures.ThreadPoolExecutor(2) as pool:
    working=pool.submit(a.exec,s,str(uuid.uuid4()),['fixture-command']);self.assertTrue(entered.wait(2));closing=pool.submit(a.finish,s,r);time.sleep(.05);self.assertFalse(closing.done());release.set();self.assertEqual(working.result()['exit_code'],0)
    with self.assertRaisesRegex(RuntimeError,'STALE_OR_LEGACY'):closing.result()
   self.assertNotIn('prepare_finish',g['calls'])
 def test_new_model_intent_invalidates_checkpoint_and_lookup_does_not_repeat(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d);a.collect(s,r,['result.txt']);new=str(uuid.uuid4());a.submit(s,new,'synthetic fixture input');self.assertEqual(activity.revision(a,s),1);before=len(g['calls']);a.submit(s,new,'synthetic fixture input');self.assertEqual(len(g['calls']),before)
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN_ACTIVITY'):a.collect(s,r,['result.txt'],str(uuid.uuid4()))
   self.assertNotIn('prepare_finish',g['calls'])
 def test_s3_snapshot_prefix_restore_and_failed_upload_block_new_work(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,r,row,g=self.fixture(d,True);store=MemoryStore();backend=storage.binding(store.cfg)
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    first=a.collect(s,r,['result.txt']);a.exec(s,str(uuid.uuid4()),['fixture-command']);snap=str(uuid.uuid4());store.fail=True
    with self.assertRaisesRegex(RuntimeError,'VM_RETAINED'):a.collect(s,r,['result.txt'],snap)
    with self.assertRaisesRegex(RuntimeError,'UNVERIFIED_EXPORT'):a.exec(s,str(uuid.uuid4()),['must-not-run'])
    store.fail=False;latest=a.collect(s,r,['result.txt'],snap);self.assertEqual(latest['receipt']['request_id'],snap);self.assertEqual(latest['request_id'],r);self.assertEqual(latest['export_binding']['checkpoint'],latest['checkpoint'])
    record=json.loads(a.export_record(s,r,snap).read_text());(a.local(s)/record['manifest_dir']/'0.bin').unlink();self.assertEqual(a.result(s,r,snap)['files'][0]['sha256'],w.digest(b'project-results'));self.assertEqual(a.result(s,r),first)
class SelectedSnapshotFinishTests(unittest.TestCase):
 fixture=CheckpointTests.fixture
 def test_verified_s3_snapshot_exports_current_base_without_rewriting_turn(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,r,row,g=self.fixture(d,True);store=MemoryStore();backend=storage.binding(store.cfg);snap=str(uuid.uuid4());before=(a.local(s)/(r+'.json')).read_bytes()
   with mock.patch.object(storage,'store',return_value=(store,backend)):
    a.collect(s,r,['result.txt'],snap);self.assertEqual((a.local(s)/(r+'.json')).read_bytes(),before)
    proof=a.finish_preflight(s,r,snap);self.assertTrue(proof['closing_candidate_absent']);self.assertEqual(g['calls'],['artifact']);self.assertEqual(a.load(s)['phase'],'ready')
    self.assertEqual(a.finish(s,r,snap)['phase'],'nomad_terminal');self.assertEqual(g['calls'].count('close'),1)
   self.assertEqual((a.local(s)/(r+'.json')).read_bytes(),before)
 def test_other_unexported_turn_or_snapshot_still_blocks_before_effects(self):
  for same_request in (False,True):
   with self.subTest(same_request=same_request),tempfile.TemporaryDirectory()as d:
    a,s,r,row,g=self.fixture(d);snap=str(uuid.uuid4());a.collect(s,r,['result.txt'],snap);other=r if same_request else str(uuid.uuid4());target=a.export_record(s,other,str(uuid.uuid4()))if same_request else a.local(s)/(other+'.json');w.atomic(target,{'session_id':s,'request_id':other,'phase':'terminal','acpx_request_id':other,'terminal':{'stopReason':'end_turn'}})
    with self.assertRaisesRegex(RuntimeError,'OTHER_TERMINAL'):a.finish(s,r,snap)
    self.assertEqual(a.load(s)['phase'],'ready');self.assertNotIn('close',g['calls']);self.assertNotIn('prepare_finish',g['calls'])
 def test_changed_base_terminal_or_corrupt_selected_manifest_not_accepted(self):
  for corrupt_manifest in (False,True):
   with self.subTest(corrupt_manifest=corrupt_manifest),tempfile.TemporaryDirectory()as d:
    a,s,r,row,g=self.fixture(d);snap=str(uuid.uuid4());a.collect(s,r,['result.txt'],snap)
    if corrupt_manifest:
     record=json.loads(a.export_record(s,r,snap).read_text());path=a.local(s)/record['manifest_dir']/'manifest.json';manifest=json.loads(path.read_text());manifest['terminal']={'stopReason':'changed'};w.atomic(path,manifest)
    else:
     path=a.local(s)/(r+'.json');base=json.loads(path.read_text());base['terminal']={'stopReason':'changed'};w.atomic(path,base)
    with self.assertRaisesRegex(RuntimeError,'OTHER_TERMINAL|MANIFEST_MISMATCH'):a.finish(s,r,snap)
    self.assertEqual(a.load(s)['phase'],'ready');self.assertNotIn('close',g['calls'])
 def test_pre_effect_proof_rejects_closing_or_nonrunning_target(self):
  for closing in (False,True):
   with self.subTest(closing=closing),tempfile.TemporaryDirectory()as d:
    a,s,r,row,g=self.fixture(d);snap=str(uuid.uuid4());a.collect(s,r,['result.txt'],snap)
    if closing:
     state=a.load(s);state['closing_candidate']={'step':'intent'};w.atomic(a.local(s)/'session.json',state)
    else:row['ClientStatus']='complete'
    before=(a.local(s)/'session.json').read_bytes()
    with self.assertRaisesRegex(RuntimeError,'EFFECTS_NOT_PROVEN|TASK_NOT_RUNNING'):a.finish_preflight(s,r,snap)
    self.assertEqual((a.local(s)/'session.json').read_bytes(),before);self.assertNotIn('close',g['calls'])

if __name__=='__main__':unittest.main()
