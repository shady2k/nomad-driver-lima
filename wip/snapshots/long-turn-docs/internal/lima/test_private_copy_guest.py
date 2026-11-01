import importlib.util,pathlib,tempfile,unittest,json,os,sys,hashlib,subprocess
spec=importlib.util.spec_from_file_location('copy_guest',pathlib.Path(__file__).with_name('private_copy_guest.py'));g=importlib.util.module_from_spec(spec);spec.loader.exec_module(g)
class CopyGuestTests(unittest.TestCase):
 def fixture(self,d):
  root=pathlib.Path(d)
  for file,data in {'etc/machine-id':'old-machine','proc/sys/kernel/random/boot_id':'old-boot','proc/mounts':'tmpfs /run tmpfs rw 0 0\n','workspace/repo/.git/config':'git state','workspace/repo/.tracker/items':'tracker state','var/tmp/nomad-workers/00000000-0000-4000-8000-000000000001/prime-config/auth.json':'synthetic-runtime-secret','var/tmp/nomad-workers/00000000-0000-4000-8000-000000000099/prime-config/auth.json':'synthetic-second-session-secret','root/.prime/state':'synthetic-runtime-secret','root/.vault-token':'synthetic-runtime-secret','etc/ssh/ssh_host_ed25519_key':'synthetic-host-key','var/lib/systemd/random-seed':'old-seed'}.items():
   p=root/file;p.parent.mkdir(parents=True,exist_ok=True);p.write_text(data)
  p={'action':'clean','session_id':'00000000-0000-4000-8000-000000000002','source_session':'00000000-0000-4000-8000-000000000001','copy_sha256':'a'*64,'manifest_sha256':'b'*64,'source_key':'c'*64}
  calls=[]
  def run(args,**kwargs):
   calls.append(args)
   if args[0]=='pgrep':return subprocess.CompletedProcess(args,1,'','')
   if args[0]=='cloud-init':(root/'etc/machine-id').write_text('uninitialized\n')
   return subprocess.CompletedProcess(args,0,'','')
  return root,p,run,calls
 def test_cleanup_keeps_workspace_git_tracker_and_probe_never_reexecutes(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,run,calls=self.fixture(d);proof=g.prepare(p,root,run);self.assertTrue(proof['known_runtime_credentials_removed']);self.assertEqual((root/'workspace/repo/.git/config').read_text(),'git state');self.assertEqual((root/'workspace/repo/.tracker/items').read_text(),'tracker state')
   self.assertFalse((root/'var/tmp/nomad-workers').exists());self.assertFalse((root/'root/.vault-token').exists());self.assertFalse((root/'var/tmp/nomad-workers'/p['source_session']).exists());self.assertEqual((root/'etc/machine-id').read_text().strip(),'uninitialized')
   count=len(calls);self.assertEqual(g.prepare({**p,'action':'probe'},root,run),proof);self.assertEqual(g.prepare(p,root,run),proof);self.assertEqual(len(calls),count)
   self.assertNotIn('synthetic-runtime-secret',json.dumps(proof))
 def test_probe_missing_or_changed_binding_fails_without_cleanup(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,run,calls=self.fixture(d)
   with self.assertRaisesRegex(RuntimeError,'UNCONFIRMED'):g.prepare({**p,'action':'probe'},root,run)
   self.assertFalse(calls);g.prepare(p,root,run)
   with self.assertRaisesRegex(RuntimeError,'CHANGED'):g.prepare({**p,'action':'probe','copy_sha256':'d'*64},root,run)
 def test_parent_symlink_and_unexpected_runtime_fail_before_key_delivery(self):
  with tempfile.TemporaryDirectory()as d,tempfile.TemporaryDirectory()as outside:
   root,p,run,calls=self.fixture(d);import shutil;shutil.rmtree(root/'var/tmp/nomad-workers');(root/'var/tmp/nomad-workers').symlink_to(outside);f=pathlib.Path(outside)/'keep';f.write_text('keep')
   with self.assertRaisesRegex(RuntimeError,'SYMLINK'):g.prepare(p,root,run)
   self.assertEqual(f.read_text(),'keep')
  with tempfile.TemporaryDirectory()as d:
   root,p,run,calls=self.fixture(d)
   with self.assertRaisesRegex(RuntimeError,'UNEXPECTED_RUNTIME'):g.prepare(p,root,lambda args,**kw:subprocess.CompletedProcess(args,0,'',''))
 def test_workspace_alias_to_runtime_is_rejected_before_reserved_cleanup(self):
  with tempfile.TemporaryDirectory()as d:
   root,p,run,calls=self.fixture(d);import shutil
   shutil.rmtree(root/'workspace/repo');(root/'workspace/repo').symlink_to(root/'var/tmp/nomad-workers')
   with self.assertRaisesRegex(RuntimeError,'SYMLINK'):g.prepare(p,root,run)
   self.assertTrue((root/'var/tmp/nomad-workers/00000000-0000-4000-8000-000000000099/prime-config/auth.json').exists())
if __name__=='__main__':unittest.main()
