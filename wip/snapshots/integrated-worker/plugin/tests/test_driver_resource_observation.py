import contextlib
import io
import json
import os
import pathlib
import sys
import types
import unittest
from unittest import mock

SOURCE=(pathlib.Path(__file__).parents[2]/'internal/lima/guest.py').read_text()

class DriverResourceObservationTests(unittest.TestCase):
 def observe(self,lvm=False):
  calls=[]
  def command(args,**kwargs):
   calls.append(args)
   if args[0]=='findmnt':return json.dumps({'filesystems':[{'source':'/dev/vda1','fstype':'ext4'}]}).encode()
   if args[0]=='lsblk':return json.dumps({'blockdevices':[{'name':'/dev/vda1','type':'lvm' if lvm else 'part','size':1000000000,'uuid':'root-fixture','children':[{'name':'/dev/vda','type':'disk','size':1073741824}]}]}).encode()
   raise AssertionError('unexpected host/guest operation')
  values={'/proc/meminfo':'MemTotal: 900000 kB\n','/etc/machine-id':'fixture-machine\n','/proc/sys/kernel/random/boot_id':'new-boot\n'}
  def opened(name,*args,**kwargs):
   if str(name) not in values:raise AssertionError('unexpected filesystem access: '+str(name))
   return io.StringIO(values[str(name)])
  out=io.StringIO();oldmask=os.umask(0o077)
  try:
   with mock.patch.object(sys,'stdin',io.StringIO(json.dumps({'action':'resource_status','key':'a'*64,'owner':'b'*32}))),mock.patch('builtins.open',side_effect=opened),mock.patch('subprocess.check_output',side_effect=command),mock.patch('os.cpu_count',return_value=2),mock.patch('os.statvfs',return_value=types.SimpleNamespace(f_blocks=200000,f_frsize=4096)),contextlib.redirect_stdout(out):
    with self.assertRaises(SystemExit) as done:exec(compile(SOURCE,'guest.py','exec'),{})
    self.assertEqual(done.exception.code,0)
  finally:os.umask(oldmask)
  return json.loads(out.getvalue()),calls
 def test_direct_partition_is_read_only_and_reports_distinct_layers(self):
  result,calls=self.observe();r=result['resources']
  self.assertEqual(r['cpus'],2);self.assertEqual(r['memory_bytes'],900000*1024)
  self.assertEqual(r['disk_bytes'],1073741824);self.assertEqual(r['partition_bytes'],1000000000);self.assertEqual(r['filesystem_bytes'],819200000)
  self.assertEqual(r['fs_uuid'],'root-fixture');self.assertTrue(r['direct_partition'])
  self.assertEqual([a[0] for a in calls],['findmnt','lsblk'])
 def test_lvm_layout_stays_explicitly_unverified(self):
  result,calls=self.observe(lvm=True);r=result['resources']
  self.assertFalse(r['direct_partition']);self.assertEqual(r['disk_bytes'],0);self.assertEqual(r['filesystem_bytes'],0)
  self.assertEqual([a[0] for a in calls],['findmnt','lsblk'])

if __name__=='__main__':unittest.main()
