import unittest,sys,pathlib,tempfile,subprocess,json,base64,copy
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import diagnostics
class DiagnosticErrors(unittest.TestCase):
 def test_safe_taxonomy_never_copies_arbitrary_exception_text(self):
  for exc in (RuntimeError('API_KEY=synthetic-secret https://private/?token=secret'),PermissionError('/private/key'),ValueError('secret')):
   error=diagnostics.failure(exc);self.assertNotIn('secret',str(error));self.assertNotIn('/private',str(error));self.assertIn('VM_RETAINED',str(error))
  error=diagnostics.CaptureFailure('stderr',RuntimeError('DIAGNOSTIC_OFFSET_GAP_VM_RETAINED'))
  self.assertIn(':stderr:DIAGNOSTIC_OFFSET_GAP_VM_RETAINED:',str(error));self.assertIs(diagnostics.failure(error),error)
 def test_noninteractive_log_read_uses_get(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'nomad';p.write_text('#!/usr/bin/env python3\nimport sys,json\nassert sys.argv[1:5]==["operator","api","-X","GET"]\nprint(json.dumps({"File":"worker.stdout.0","Offset":0}))\n');p.chmod(0o700)
   self.assertEqual(list(diagnostics.frames(str(p),'/logs',1024)),[{'File':'worker.stdout.0','Offset':0}])

 def test_real_short_tail_offsets_normalize_only_after_independent_verification(self):
  data=b'x'*200861;lengths=[65536,65536,65536,4253];rows=[];cursor=0
  for i,n in enumerate(lengths):
   rows.append({'File':'worker.stdout.0','Offset':65536*(i+1),'Data':base64.b64encode(data[cursor:cursor+n]).decode()});cursor+=n
  fixed=diagnostics.verified_frames(rows,data,1048576);self.assertEqual(fixed[-1]['Offset'],200861)
  with tempfile.TemporaryDirectory() as d:
   item,coverage=diagnostics.stream_snapshot(fixed,'worker','stdout',pathlib.Path(d),1048576,diagnostics.Redactor());self.assertEqual((pathlib.Path(d)/item['local_file']).read_bytes(),data)
  for corrupted in (data[:-1],b'y'+data[1:]):
   with self.assertRaisesRegex(RuntimeError,'INDEPENDENT_READ_MISMATCH'):diagnostics.verified_frames(rows,corrupted,1048576)
  gap=copy.deepcopy(rows);gap[1]['Offset']+=1
  with self.assertRaisesRegex(RuntimeError,'OFFSET_GAP'):diagnostics.verified_frames(gap,data,1048576)
  missing=rows[:1]+rows[2:]
  with self.assertRaises(RuntimeError):diagnostics.verified_frames(missing,data,1048576)
  # Duplicate/reordered frames and a false short tail cannot hide omitted bytes.
  for bad in (rows+[rows[-1]],rows[1:]+rows[:1]):
   with self.assertRaises(RuntimeError):diagnostics.verified_frames(bad,data,1048576)
