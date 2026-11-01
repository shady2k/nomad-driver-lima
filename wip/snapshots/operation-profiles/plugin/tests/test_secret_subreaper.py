"""Linux kernel contract for the finite runner; no credentials, root or VM needed."""
import unittest,sys,pathlib,subprocess,tempfile,json,types,os,time,signal
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations
@unittest.skipUnless(sys.platform=='linux','Linux-only subreaper kernel contract')
class SecretSubreaperTests(unittest.TestCase):
 def test_reparented_child_is_owned_and_stopped(self):
  with tempfile.TemporaryDirectory()as d:
   marker=pathlib.Path(d)/'child-pid'
   child='import os,time,pathlib;pid=os.fork();\nif pid==0:\n os.setsid();pathlib.Path('+repr(str(marker))+').write_text(str(os.getpid()));time.sleep(30)\n'
   code=operations.program().replace("if __name__=='__main__':","if False:")
   code+='\nsecret_subreaper()\n'
   code+='child=subprocess.Popen([sys.executable,"-c",'+repr(child)+']);child.wait(timeout=3)\n'
   code+='end=time.monotonic()+3\nwhile not pathlib.Path('+repr(str(marker))+').exists()and time.monotonic()<end:time.sleep(.02)\n'
   code+='pid=int(pathlib.Path('+repr(str(marker))+').read_text());rows=secret_owned_descendants();assert any(row["pid"]==pid for row in rows)\n'
   code+='owned=SecretOperation.__new__(SecretOperation);owned.processes=[process(os.getpid())];owned.child=child;owned.stop();assert not secret_owned_descendants();print("subreaper-contract-pass")\n'
   result=subprocess.run([sys.executable,'-c',code],capture_output=True,text=True,timeout=12)
   self.assertEqual(result.returncode,0,result.stderr);self.assertIn('subreaper-contract-pass',result.stdout)
 def test_runner_death_after_orphan_adoption_never_confirms_cleanup(self):
  with tempfile.TemporaryDirectory()as d:
   root=pathlib.Path(d);root.chmod(0o700);module=types.ModuleType('fixture_kernel');exec(operations.program(),module.__dict__)
   module.descendants=module.secret_descendants
   operation='00000000-0000-4000-8000-000000000002'
   binding={'session_id':'00000000-0000-4000-8000-000000000001','creator_agent_id':'fixture-agent','creator_session_id':'fixture-native','creator_generation':'fixture-generation','attempt':{'allocation':'fixture-allocation','task':'worker'}}
   machine={'machine_id':'fixture-machine','boot_id':'fixture-boot'}
   module.secret_atomic(root/'secret-profiles.json',{'binding':binding,'machine_identity':machine})
   retained=root/'retained-data';retained.write_text('preserve-me');marker=root/'orphan-pid';ready=root/'runner-ready'
   parent='import os,time,pathlib;pid=os.fork();\nif pid==0:\n os.setsid();pathlib.Path('+repr(str(marker))+').write_text(str(os.getpid()));time.sleep(25)\nelse:time.sleep(.1)\n'
   code=operations.program().replace("if __name__=='__main__':","if False:")
   code+='\nsecret_subreaper()\nroot=pathlib.Path('+repr(str(root))+')\n'
   code+='child=subprocess.Popen([sys.executable,"-c",'+repr(parent)+']);parent_row=process(child.pid);child.wait(timeout=3)\n'
   code+='end=time.monotonic()+3\nwhile not pathlib.Path('+repr(str(marker))+').exists()and time.monotonic()<end:time.sleep(.02)\n'
   code+='orphan=int(pathlib.Path('+repr(str(marker))+').read_text());assert any(row["pid"]==orphan for row in secret_owned_descendants())\n'
   code+='parent_dir=root/"secret-operations";parent_dir.mkdir(mode=0o700);op=parent_dir/'+repr(operation)+';op.mkdir(mode=0o700)\n'
   code+='secret_atomic(op/"owner.json",{"version":2,"phase":"active","registry_id":"a"*32,"operation_id":'+repr(operation)+',"binding":'+repr(binding)+',"machine_identity":'+repr(machine)+',"runner":process(os.getpid()),"processes":[process(os.getpid()),parent_row]})\n'
   code+='pathlib.Path('+repr(str(ready))+').write_text("adopted");time.sleep(25)\n'
   runner=subprocess.Popen([sys.executable,'-c',code],stdout=subprocess.DEVNULL,stderr=subprocess.PIPE);orphan_row=None
   try:
    deadline=time.monotonic()+5
    while not ready.exists()and runner.poll()is None and time.monotonic()<deadline:time.sleep(.02)
    self.assertTrue(ready.exists(),'runner did not confirm orphan adoption')
    orphan_row=module.process(int(marker.read_text()));self.assertIsNotNone(orphan_row)
    runner.kill();runner.wait(timeout=2)
    owner=module.secret_read(root/'secret-operations'/operation/'owner.json');self.assertTrue(all(not module.alive(row)for row in owner['processes']))
    for attempt in range(2):
     with self.assertRaisesRegex(RuntimeError,'SECRET_RUNNER_LOST_CLEANUP_UNCONFIRMED'):module.secret_stop_all(root,module,timeout=.2)
    self.assertTrue(module.alive(orphan_row));self.assertEqual(retained.read_text(),'preserve-me');self.assertFalse((root/'secret-completed'/ (operation+'.json')).exists())
   finally:
    if runner.poll()is None:runner.kill();runner.wait(timeout=2)
    if orphan_row is None and marker.exists():orphan_row=module.process(int(marker.read_text()))
    if orphan_row and module.alive(orphan_row):os.kill(orphan_row['pid'],signal.SIGKILL)
    if orphan_row:
     try:os.waitpid(orphan_row['pid'],0)
     except ChildProcessError:pass
    if runner.stderr is not None:runner.stderr.close()

if __name__=='__main__':unittest.main()
