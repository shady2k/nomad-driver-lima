import base64,copy,hashlib,importlib.util,json,os,pathlib,pwd,subprocess,sys,tempfile,unittest,uuid
from types import SimpleNamespace
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import template_save as save
ROOT=pathlib.Path(__file__).parents[2]
def load(name,file):
 spec=importlib.util.spec_from_file_location(name,ROOT/'deploy'/file);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m
artifact=load('artifact_gap','template-artifact-stage.py');transfer=load('transfer_gap','template-artifact-transfer.py');node=load('preflight_gap','template-preflight-node.py');publisher=load('publisher_gap','publish-artifact.py')
class MissingStore:
 def __init__(self):self.raw=None;self.puts=0;self.race=None
 def get(self,key):
  if self.raw is None:raise RuntimeError('exchange_s3_http_404')
  return self.raw
 def stat(self,key):return {'etag':'existing'}
 def conditional_put(self,key,raw,etag=None):
  self.puts+=1
  if self.race is not None:self.raw=self.race;raise RuntimeError('412')
  if self.raw is not None:raise RuntimeError('412')
  self.raw=raw
class RuntimeGapTests(unittest.TestCase):
 def test_conditional_init_preserves_base_seed_and_never_overwrites_race(self):
  from test_template_save import CatalogTests
  entry=CatalogTests().entry();base=copy.deepcopy(entry);base.update(template_id='agent-base',version='existing-v5');seed={'schema_version':1,'templates':[base]};store=MissingStore()
  result=publisher.append_catalog(store,'catalog',None,entry,True,seed);self.assertTrue(result['atomic_conditional_append']);self.assertEqual(json.loads(store.raw)['templates'],[base,entry]);self.assertEqual(store.puts,1);self.assertEqual(seed['templates'],[base])
  store=MissingStore();store.race=b'{"schema_version":1,"templates":[]}'
  with self.assertRaisesRegex(RuntimeError,'UNKNOWN_OR_CONFLICT'):publisher.append_catalog(store,'catalog',None,entry,True,seed)
  self.assertEqual(store.raw,store.race);self.assertEqual(store.puts,1)
  with self.assertRaisesRegex(RuntimeError,'SEED_REQUIRED'):publisher.append_catalog(MissingStore(),'catalog',None,entry,True)
 def test_transfer_real_subprocess_binary_hash_and_corruption(self):
  with tempfile.TemporaryDirectory() as d:
   raw=b'accepted-disk\0'*10000;p={'plan':{'operation_id':str(uuid.uuid4())},'plan_sha256':'b'*64};header={'operation_id':p['plan']['operation_id'],'plan_sha256':p['plan_sha256'],'file':'disk.qcow2','size_bytes':len(raw),'sha256':hashlib.sha256(raw).hexdigest()}
   def command(data):return [sys.executable,'-c','import sys;sys.stdin.read();sys.stdout.buffer.write('+repr((json.dumps(header)+'\n').encode()+data)+')']
   dest=pathlib.Path(d)/'disk';cfg={'artifact_fetch_command':command(raw)};self.assertEqual(transfer.receive(p,{},cfg,'disk.qcow2',dest)['sha256'],header['sha256']);self.assertEqual(dest.read_bytes(),raw)
   for data in (raw[:-1],b'corrupt'+raw[7:],raw+b'extra'):
    with self.assertRaises(RuntimeError):transfer.receive(p,{},dict(cfg,artifact_fetch_command=command(data)),'disk.qcow2',pathlib.Path(d)/'bad')
    self.assertFalse((pathlib.Path(d)/'bad').exists())
 def test_acceptance_waits_for_real_shell_before_probes_and_keeps_model_proof(self):
  from test_template_stage_bindings import BindingTests
  with tempfile.TemporaryDirectory() as d:
   p=BindingTests().packet();p['plan']['dependency_recipe']={'packages':[{'manager':'apt','name':'tool','version':'1'}]};p['plan_sha256']=save.sha(p['plan']);op=p['plan']['operation_id'];root=pathlib.Path(d)/op;root.mkdir(mode=0o700);out=root/'artifact';out.mkdir();(out/'export.json').write_text(json.dumps({'disk':{'sha256':'a'*64}}));(root/'seal-receipt.json').write_text(json.dumps({'guest_assets':{}}));(root/'model-definitions.json').write_text('{"models":[]}')
   receipt={'operation_id':op,'plan_sha256':p['plan_sha256'],'candidate_instance':'agent-base-build-save-'+op.replace('-',''),**dict.fromkeys(('source_stopped_verified','source_identity_bound','candidate_distinct','source_disk_unchanged','clone_stopped','no_rebuild'),True)}
   cfg={'build_root':d,'lima_home':str(pathlib.Path(d)/'home'),'runtime_user':pwd.getpwuid(os.getuid()).pw_name,'limactl':'/official/limactl','python':sys.executable,'architecture':'amd64','vm_type':'qemu','disk_gib':20,'dependency_probes':[{'manager':'apt','name':'tool','version':'1','argv':['/tool','version'],'stdout_sha256':hashlib.sha256(b'1\n').hexdigest()}]};calls=[];ready=set();seen={}
   def run(args,**kw):
    calls.append(args)
    if 'list'in args:
     name=args[-1];seen[name]=seen.get(name,0)+1;return SimpleNamespace(returncode=0,stdout=json.dumps({'name':name,'dir':str(pathlib.Path(cfg['lima_home'])/name),'status':'Starting'if seen[name]==1 else'Running'}))
    if '/usr/bin/true'in args:ready.add(args[args.index('shell')+1]);return SimpleNamespace(returncode=0,stdout='')
    if '/tool'in args:self.assertIn(args[args.index('shell')+1],ready);return SimpleNamespace(returncode=0,stdout='1\n')
    if '/usr/bin/python3'in args:return SimpleNamespace(returncode=0,stdout='{"model_requests":0}')
    if len(args)>2 and args[2]=='check':(out/'acceptance.json').write_text('{"independent_boots":2,"unique_identities":true}')
    return SimpleNamespace(returncode=0,stdout='')
   with mock.patch.object(artifact.time,'sleep'):result=artifact.run_stage('boot',p,{'clone_candidate':receipt},cfg,run)
   self.assertTrue(result['two_independent_boots']);self.assertTrue((out/'model-catalog-proof.json').exists());self.assertEqual(len(ready),2);self.assertEqual(sum(1 for a in calls if len(a)>2 and a[2]=='stop'),2)
 def test_node_preflight_stages_frozen_assets_idempotently_without_vm_commands(self):
  from test_template_stage_bindings import BindingTests
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d);state_dir=root/'state';state_dir.mkdir(mode=0o700);key='d'*64;p=BindingTests().packet();s=p['plan']['source'];state={'Phase':'running','AllocID':s['allocation'],'TaskName':s['task'],'WorkerSessionID':s['session_id'],'CreatorAgentID':s['creator_agent_id'],'CreatorSessionID':s['creator_session_id']};f=state_dir/(key+'.json');f.write_text(json.dumps(state));f.chmod(0o600)
   cfg={'ledger_key':key,'state_dir':str(state_dir),'runtime_user':pwd.getpwuid(os.getuid()).pw_name,'build_root':str(root/'builds'),'disk_gib':20};space=SimpleNamespace(f_bavail=1000000000,f_frsize=4096)
   with mock.patch.object(node.os,'statvfs',return_value=space),mock.patch.object(node.sys,'platform','darwin'):
    first=node.preflight(p,cfg);second=node.preflight(p,cfg)
   self.assertEqual(first,second);self.assertTrue(first['frozen_assets_staged']);stage=pathlib.Path(first['build_root']);self.assertTrue((stage/'model-definitions.json').is_file());self.assertTrue((stage/'base-seal.sh').is_file());self.assertEqual(json.loads((stage/'prepare-operation.json').read_text())['plan_sha256'],p['plan_sha256']);self.assertEqual(json.loads(f.read_text()),state)
 def test_preflight_composition_rejects_unbound_role_before_stop(self):
  module=load('pref_comp','template-preflight.py');p={'plan':{'operation_id':str(uuid.uuid4())}};p['plan_sha256']=save.sha(p['plan']);calls=[]
  def run(args,**kw):calls.append(args);return SimpleNamespace(returncode=0,stdout=json.dumps({'operation_id':'wrong','plan_sha256':p['plan_sha256']}))
  with self.assertRaisesRegex(RuntimeError,'UNBOUND_PREFLIGHT'):module.preflight(p,{'source_preflight':['/source'],'node_preflight':['/node'],'publisher_preflight':['/publisher']},run)
  self.assertEqual(calls,[['/source']])
