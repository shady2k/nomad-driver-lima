import importlib.util,json,os,pathlib,sys,tempfile,unittest,uuid
from types import SimpleNamespace
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import template_save as save
spec=importlib.util.spec_from_file_location('clone_stage',pathlib.Path(__file__).parents[2]/'deploy/template-clone.py');mod=importlib.util.module_from_spec(spec);spec.loader.exec_module(mod)
class CloneTests(unittest.TestCase):
 def fixture(self,d):
  home=pathlib.Path(d);src=home/'source';src.mkdir();(src/'diffdisk').write_bytes(b'prepared-tools-and-data')
  plan={'operation_id':str(uuid.uuid4()),'method':'official_stopped_disk_clone_then_existing_seal_export_pipeline'};p={'plan':plan,'plan_sha256':save.sha(plan)}
  r={'source_finish':{'operation_id':plan['operation_id'],'plan_sha256':p['plan_sha256'],'source_instance':'source','source_disk_identity':mod.disk_identity(src),**dict.fromkeys(('source_owner_verified','results_receipt_verified','prime_bao_stopped','task_token_revoked','source_vm_stopped','source_disk_retained'),True)}}
  calls=[]
  def run(args,**kw):
   calls.append(args)
   if args[1]=='list':name=args[-1];return SimpleNamespace(returncode=0,stdout=json.dumps({'name':name,'status':'Stopped','dir':str(home/name)}))
   self.assertEqual(args[1:4],['clone','--tty=false','--start=false']);self.assertIn('.mounts=[]',args[5]);dst=home/args[-1];dst.mkdir();(dst/'diffdisk').write_bytes((src/'diffdisk').read_bytes());return SimpleNamespace(returncode=0,stdout='')
  return home,p,r,calls,run
 def test_clones_actual_prepared_disk_no_start_rebuild_or_source_change(self):
  with tempfile.TemporaryDirectory() as d:
   home,p,r,calls,run=self.fixture(d);result=mod.clone(p,r,home,'/official/limactl',run)
   self.assertTrue(result['no_rebuild']);self.assertEqual((home/result['candidate_instance']/'diffdisk').read_bytes(),b'prepared-tools-and-data');self.assertEqual(len([x for x in calls if x[1]=='clone']),1)
   with self.assertRaisesRegex(RuntimeError,'CANDIDATE_EXISTS'):mod.clone(p,r,home,'/official/limactl',run)
 def test_running_wrong_identity_symlink_and_changed_disk_fail_before_clone(self):
  for mode in ('running','wrong_identity','symlink','changed'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory() as d:
    home,p,r,calls,run=self.fixture(d)
    if mode=='changed':(home/'source/diffdisk').write_bytes(b'changed')
    if mode=='symlink':(home/'source/diffdisk').unlink();(home/'source/diffdisk').symlink_to(home/'other')
    original=run
    def guarded(args,**kw):
     out=original(args,**kw)
     if args[1]=='list' and mode in ('running','wrong_identity'):
      row=json.loads(out.stdout);row['status' if mode=='running' else 'name']='Running' if mode=='running' else 'other';out.stdout=json.dumps(row)
     return out
    with self.assertRaises(RuntimeError):mod.clone(p,r,home,'/official/limactl',guarded)
    self.assertFalse(any(x[1]=='clone' for x in calls))
 def test_composition_requires_explicit_nonoverlapping_paths(self):
  for value in (None,{'schema_version':1,'retain':['/workspace'],'remove':['/workspace/repo'],'description':'conflict'},{'schema_version':1,'retain':['/'],'remove':[],'description':'whole disk'}):
   with self.assertRaises(ValueError):save.composition_contract(value)
  self.assertEqual(save.composition_contract({'schema_version':1,'retain':['/usr/local'],'remove':['/workspace/repo'],'description':'tools only'})['retain'],['/usr/local'])
