import copy,hashlib,pathlib,struct,sys,tempfile,unittest,zlib
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]))
from base_platform import Platform,raw_gpt_info,export_raw,build_disk_budget_gib,validate_boot_proofs
SIZE=65536
def gpt(path):
 data=bytearray(SIZE)
 for lba,other in [(1,SIZE//512-1),(SIZE//512-1,1)]:
  b=bytearray(512);b[:8]=b'EFI PART';struct.pack_into('<II',b,8,0x10000,92);struct.pack_into('<QQ',b,24,lba,other);b[56:72]=bytes(range(16));struct.pack_into('<I',b,16,zlib.crc32(b[:92])&0xffffffff);data[lba*512:(lba+1)*512]=b
 data[4096:4112]=b'public-fixture!!';path.write_bytes(data)
class NativeArtifactTests(unittest.TestCase):
 def test_platform_mapping_and_no_emulation(self):
  p=Platform('arm64','vz');self.assertEqual((p.lima_arch,p.filename),('aarch64','disk.raw'));p.require_host('Darwin','arm64')
  for args in [('Darwin','x86_64'),('Linux','aarch64')]:
   with self.assertRaises(ValueError):p.require_host(*args)
  with self.assertRaises(ValueError):Platform('amd64','vz')
  self.assertEqual(Platform().filename,'disk.qcow2')
 def test_runtime_has_pins_no_provision_mounts_or_credentials(self):
  s=Platform('arm64','vz').runtime_yaml();self.assertIn('vmType: vz',s);self.assertIn('arch: aarch64',s)
  for word in ['provision:','mounts:','LLM_API_KEY','token','provider']:self.assertNotIn(word,s)
 def test_worst_case_disk_budget(self):
  self.assertEqual(build_disk_budget_gib(8),68)
  with self.assertRaises(ValueError):build_disk_budget_gib(8,1)
 def test_raw_export_standalone_sparse_preserves_original(self):
  with tempfile.TemporaryDirectory()as t:
   src=pathlib.Path(t)/'disk';dest=pathlib.Path(t)/'disk.raw';gpt(src);before=src.read_bytes();calls=[]
   info=export_raw(src,dest,SIZE,lambda:calls.append('stopped'))
   self.assertEqual(info['format'],'raw');self.assertIsNone(info['backing-filename']);self.assertEqual(src.read_bytes(),before);self.assertEqual(dest.read_bytes(),before);self.assertEqual(calls,['stopped','stopped']);self.assertEqual(dest.stat().st_mode&0o777,0o444)
   with self.assertRaises(FileExistsError):export_raw(src,dest,SIZE,lambda:None)
 def test_uncertain_partial_never_replayed(self):
  with tempfile.TemporaryDirectory()as t:
   src=pathlib.Path(t)/'disk';dest=pathlib.Path(t)/'disk.raw';gpt(src);dest.with_name('disk.raw.partial').write_bytes(b'unknown')
   with self.assertRaises(FileExistsError):export_raw(src,dest,SIZE,lambda:None)
   self.assertFalse(dest.exists())
 def test_active_guard_prevents_export(self):
  with tempfile.TemporaryDirectory()as t:
   src=pathlib.Path(t)/'disk';dest=pathlib.Path(t)/'disk.raw';gpt(src)
   def busy():raise RuntimeError('active')
   with self.assertRaises(RuntimeError):export_raw(src,dest,SIZE,busy)
   self.assertFalse(dest.exists());self.assertFalse(dest.with_name('disk.raw.partial').exists())
 def test_changed_source_never_published(self):
  with tempfile.TemporaryDirectory()as t:
   src=pathlib.Path(t)/'disk';dest=pathlib.Path(t)/'disk.raw';gpt(src);calls=[]
   def guard():
    calls.append(1)
    if len(calls)==2:
     with src.open('r+b')as f:f.seek(8192);f.write(b'changed')
   with self.assertRaises(ValueError):export_raw(src,dest,SIZE,guard)
   self.assertFalse(dest.exists());self.assertTrue(dest.with_name('disk.raw.partial').exists())
 def test_rejects_qcow_wrong_size_bad_gpt_and_symlink(self):
  with tempfile.TemporaryDirectory()as t:
   src=pathlib.Path(t)/'disk';gpt(src)
   with self.assertRaises(ValueError):raw_gpt_info(src,SIZE+512)
   data=bytearray(src.read_bytes());data[512+16]^=1;src.write_bytes(data)
   with self.assertRaises(ValueError):raw_gpt_info(src,SIZE)
   src.write_bytes(b'QFI\xfb'+bytes(SIZE-4))
   with self.assertRaises(ValueError):raw_gpt_info(src,SIZE)
   link=pathlib.Path(t)/'link';link.symlink_to(src)
   with self.assertRaises(OSError):raw_gpt_info(link,SIZE)
 def test_identity_asset_arch_and_seal_proofs(self):
  assets={'task-main.py':'a','adapter.mjs':'b','launcher.mjs':'c'};checks=[]
  for i in range(2):checks.append(dict(disk_sha256='d',architecture='arm64',vm_type='vz',assets=assets,**{k:k+str(i)for k in ['machine_id','boot_id','hostname','ssh_host_key']}))
  p=Platform('arm64','vz');validate_boot_proofs(checks,assets,'d',p)
  for key,value in [('machine_id',checks[0]['machine_id']),('disk_sha256','wrong'),('architecture','amd64'),('assets',{})]:
   changed=copy.deepcopy(checks);changed[1][key]=value
   with self.assertRaises(ValueError):validate_boot_proofs(changed,assets,'d',p)
  with self.assertRaises(ValueError):validate_boot_proofs(checks[:1],assets,'d',p)
class MacGuardTests(unittest.TestCase):
 def guard(self,disk_free=100*2**30,percentage=60):
  import runpy,io,contextlib,unittest.mock as mock,types
  script=pathlib.Path(__file__).resolve().parents[1]/'base-mac-preflight.py'
  with tempfile.TemporaryDirectory()as t, mock.patch('sys.argv',[str(script),'--path',t]), mock.patch('platform.system',return_value='Darwin'),mock.patch('platform.machine',return_value='arm64'),mock.patch('shutil.disk_usage',return_value=types.SimpleNamespace(free=disk_free)),mock.patch('subprocess.check_output',side_effect=[str(48*2**30),'System-wide memory free percentage: '+str(percentage)+'%']),contextlib.redirect_stdout(io.StringIO())as output:
   runpy.run_path(str(script),run_name='__main__');return __import__('json').loads(output.getvalue())
 def test_budget_and_memory_guard_mocked(self):
  r=self.guard();self.assertEqual(r['required_disk_bytes'],68*2**30);self.assertEqual(r['mutations'],0)
 def test_reject_low_disk_or_pressure_without_start(self):
  with self.assertRaises(AssertionError):self.guard(disk_free=67*2**30)
  with self.assertRaises(AssertionError):self.guard(percentage=10)
class CatalogCapabilityTests(unittest.TestCase):
 def test_modern_and_legacy_model_lists(self):
  import importlib.util
  script=pathlib.Path(__file__).resolve().parents[1]/'base-model-catalog-probe.py';spec=importlib.util.spec_from_file_location('catalog_probe',script);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  self.assertEqual(m.advertised_values({'configOptions':[{'id':'model','options':[{'value':'a'},{'options':[{'value':'b'}]}]}]}),['a','b'])
  self.assertEqual(m.advertised_values({'models':{'availableModels':[{'modelId':'legacy'}]}}),['legacy'])
 def test_model_capability_requires_matching_actual_no_prompt_proof(self):
  from base_platform import model_catalog_capability
  p={'disk_sha256':'disk','guest_assets':{'task':'sha'},'native_models_advertised':[str(i)for i in range(10)],'native_model_count':10,'compatibility_groups':2,'actual_acpx_control':'pass','guest_model_generator_verified':True,'model_requests':0,'worker_tasks_started':0,'coordinator_selections':['gpt-6.1-sol','deepseek-v4.1-flash-auto'],'acpx_session_closed':True,'prime_daemon_and_direct_transport_stopped':True}
  self.assertEqual(model_catalog_capability(p,'disk',{'task':'sha'}),'1')
  for key,value in [('disk_sha256','other'),('model_requests',1),('native_model_count',9),('guest_model_generator_verified',False),('actual_acpx_control','mock'),('acpx_session_closed',False)]:
   bad=copy.deepcopy(p);bad[key]=value
   with self.assertRaises(ValueError):model_catalog_capability(bad,'disk',{'task':'sha'})
if __name__=='__main__':unittest.main()
