import unittest,tempfile,pathlib,json,sys,hashlib,importlib.util
spec=importlib.util.spec_from_file_location('workerctl',pathlib.Path(__file__).parents[1]/'scripts/workerctl.py');module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
class Tests(unittest.TestCase):
 def test_atomic_manifest_is_canonical_private(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'manifest';module.atomic(p,{'b':2,'a':1});self.assertEqual(p.read_bytes(),b'{"a":1,"b":2}');self.assertEqual(p.stat().st_mode&0o777,0o600)
 def test_uuid_no_path_injection(self):
  for x in ('../x','worker','00000000-0000-0000-0000-00000000000G'):
   with self.assertRaises(ValueError):module.uid(x)
 def test_result_integrity_and_backend(self):
  import uuid
  with tempfile.TemporaryDirectory() as d:
   s=str(uuid.uuid4());r=str(uuid.uuid4());cfg={'version':1,'allocation':str(uuid.uuid4()),'owner_id':str(uuid.uuid4()),'receipt_root':d};p=pathlib.Path(d)/'config';p.write_text(json.dumps(cfg));a=module.Adapter(p);root=a.local(s);module.atomic(root/'session.json',{'config_hash':a.hash});out=root/'artifacts';out.mkdir();b=b'fixture';(out/'0.bin').write_bytes(b);m={'session_id':s,'request_id':r,'actual_backend':'coordinator-local','files':[{'local_file':'0.bin','size':len(b),'sha256':module.digest(b)}]};module.atomic(out/'manifest.json',m);module.atomic(root/(r+'.json'),{'export_verified':True,'manifest_dir':'artifacts','manifest_sha256':module.digest(module.canonical(m))});self.assertEqual(a.result(s,r)['actual_backend'],'coordinator-local');(out/'0.bin').write_bytes(b'bad')
   with self.assertRaisesRegex(RuntimeError,'CORRUPT'):a.result(s,r)
if __name__=='__main__':unittest.main()
