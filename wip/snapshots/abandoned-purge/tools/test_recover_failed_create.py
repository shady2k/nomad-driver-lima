import contextlib
import datetime as dt
import importlib.util
import io
import json
import pathlib
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

path=pathlib.Path(__file__).with_name('recover-failed-create.py')
spec=importlib.util.spec_from_file_location('recovery_controller',path)
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class ControllerTests(unittest.TestCase):
 def args(self):
  return types.SimpleNamespace(runtime_host='existing-runtime',operator_host='existing-operator',driver='/driver',state_dir='/state',lima_home='/lima',runtime_user='runtime',limactl='/limactl',key='a'*64,allocation='00000000-0000-4000-8000-000000000001',node='00000000-0000-4000-8000-000000000002',job='worker-test',namespace='agent-workers',failure_log='/state/diagnostics/create-1.stderr',operator_cli='/existing-operator-cli',operator_python='/usr/bin/python3',ssh_config=None,ssh_identity=None,runtime_sudo=False)
 def test_stopped_source_flags_parse_and_forward_to_same_purge_binary(self):
  a=self.args();a.operation='purge-retained';a.instance='owned-aaaaaaaaaaaa';a.confirm_purge=True;a.ledger_sha256='b'*64;a.stopped_source_receipt='/private/source-stopped.json';a.stopped_source_receipt_sha256='c'*64
  import shlex
  remote=shlex.split(m.runtime_command(a)[-1]);self.assertEqual(remote[0],a.driver);self.assertEqual(remote[1],'purge-retained');self.assertEqual(remote[remote.index('--stopped-source-receipt')+1],a.stopped_source_receipt);self.assertEqual(remote[remote.index('--stopped-source-receipt-sha256')+1],'c'*64)
  required=['--runtime-host','runtime','--operator-host','operator','--driver','/driver','--state-dir','/state','--lima-home','/lima','--runtime-user','runtime','--limactl','/limactl','--key','a'*64,'--allocation',a.allocation,'--node',a.node,'--namespace','agent-workers','--operator-python','/python','--operation','purge-retained','--instance',a.instance,'--confirm-purge','--ledger-sha256','b'*64,'--stopped-source-receipt',a.stopped_source_receipt,'--stopped-source-receipt-sha256','c'*64]
  with mock.patch.object(sys,'argv',['controller',*required]):parsed=m.parse_args()
  self.assertIn('--stopped-source-receipt',shlex.split(m.runtime_command(parsed)[-1]))
 def test_abandoned_flags_forward_without_replacing_driver(self):
  a=self.args();a.operation='purge-retained';a.instance='owned-disk';a.confirm_purge=True;a.ledger_sha256='b'*64;a.stopped_source_receipt='/proof';a.stopped_source_receipt_sha256='c'*64;a.abandoned_stopped=True;a.qemu_img='/existing/qemu-img';a.allow_gc_404=True
  import shlex
  command=shlex.split(m.runtime_command(a)[-1]);self.assertIn('--abandoned-stopped',command);self.assertEqual(command[command.index('--qemu-img')+1],'/existing/qemu-img')
 def test_invalid_receipt_pair_cannot_launch_ssh(self):
  a=self.args();a.stopped_source_receipt='/private/source.json';a.stopped_source_receipt_sha256=None
  with mock.patch.object(m.subprocess,'Popen')as process:
   with self.assertRaises(m.RecoveryError):m.run(a)
   process.assert_not_called()
 def test_ssh_existing_trust_no_forwarding(self):
  cmd=m.ssh_prefix(self.args())
  self.assertIn('StrictHostKeyChecking=yes',cmd);self.assertIn('ForwardAgent=no',cmd)
  self.assertNotIn('-A',cmd)
 def test_challenge_replay_identity_and_deadline(self):
  a=self.args();target=m.target_from(a)
  request=dict(version=1,type='allocation_check',sequence=1,nonce='b'*64,target=target,deadline=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(seconds=8)).isoformat())
  seen=set();m.challenge_valid(request,target,1,seen)
  with self.assertRaises(m.RecoveryError):m.challenge_valid(request,target,1,seen)
  for field,value in [('sequence',2),('target',{}),('deadline','2000-01-01T00:00:00Z')]:
   with self.assertRaises((m.RecoveryError,ValueError)):m.challenge_valid({**request,field:value},target,1,set())
 def test_operator_filters_secrets_on_operator_before_transport(self):
  with tempfile.TemporaryDirectory() as d:
   cli=pathlib.Path(d)/'operator'
   allocation=dict(ID='id',NodeID='node',JobID='job',Namespace='ns',ClientStatus='failed',ModifyIndex=1,Job={'Env':{'secret':'DO-NOT-EXPORT'}},TaskStates={'worker':dict(State='dead',Failed=True,Restarts=0,Env={'secret':'DO-NOT-EXPORT'},Events=[dict(Type='Driver Failure',DisplayMessage='DO-NOT-EXPORT')])})
   cli.write_text('#!'+sys.executable+'\nimport json,sys\nassert sys.argv[1:]==["operator","api","-namespace=ns","-X","GET","/v1/allocation/id"]\nprint('+repr(json.dumps(allocation))+')\n')
   cli.chmod(0o700)
   r=subprocess.run([sys.executable,'-c',m.OPERATOR_READ,str(cli),'ns','id'],capture_output=True,check=True)
   self.assertNotIn(b'DO-NOT-EXPORT',r.stdout)
   result=json.loads(r.stdout)
   self.assertNotIn('Job',result['allocation'])
   self.assertEqual(result['allocation']['TaskStates']['worker']['Events'],[{'Type':'Driver Failure'}])
 def test_controller_two_fresh_reads_no_retries_and_result(self):
  a=self.args();target=m.target_from(a)
  script='''import json,sys,datetime as dt
TARGET=json.loads(sys.argv[1])
for seq in [1,2]:
 nonce=str(seq)*64
 print(json.dumps(dict(version=1,type='allocation_check',sequence=seq,nonce=nonce,target=TARGET,deadline=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(seconds=8)).isoformat())),flush=True)
 r=json.loads(sys.stdin.readline())
 assert r['nonce']==nonce and r['sequence']==seq and r['target']==TARGET
print(json.dumps(dict(key=TARGET['key'],instance='test-aaaaaaaaaaaa',phase='no_vm_failed',audit_sha256='f'*64,already_recovered=False)),flush=True)
'''
  real_popen=subprocess.Popen
  def start(*unused,**kw):return real_popen([sys.executable,'-c',script,json.dumps(target)],**kw)
  observation=dict(allocation={},observed_at=dt.datetime.now(dt.timezone.utc).isoformat())
  with mock.patch.object(m.subprocess,'Popen',side_effect=start),mock.patch.object(m,'operator_read',return_value=observation) as read,contextlib.redirect_stdout(io.StringIO()) as output:
   m.run(a)
  self.assertEqual(read.call_count,2);self.assertEqual(json.loads(output.getvalue())['phase'],'no_vm_failed')
 def test_operator_failure_is_not_retried(self):
  a=self.args();target=m.target_from(a)
  script='''import json,sys,datetime as dt
print(json.dumps(dict(version=1,type='allocation_check',sequence=1,nonce='b'*64,target=json.loads(sys.argv[1]),deadline=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(seconds=8)).isoformat())),flush=True)
sys.stdin.readline()
'''
  real_popen=subprocess.Popen
  def start(*unused,**kw):return real_popen([sys.executable,'-c',script,json.dumps(target)],**kw)
  with mock.patch.object(m.subprocess,'Popen',side_effect=start),mock.patch.object(m,'operator_read',side_effect=m.RecoveryError('denied')) as read:
   with self.assertRaises(m.RecoveryError):m.run(a)
  self.assertEqual(read.call_count,1)


class GCStatusVerifierTests(unittest.TestCase):
 def test_real_http_status_metadata_is_required_not_error_text(self):
  import urllib.error,urllib.parse
  for kind in ['success','forbidden','wrong-node','active-node','not-management']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory() as d:
    token=pathlib.Path(d)/'existing-operator-auth';token.write_text(json.dumps({'SecretID':'synthetic-management-token'}));token.chmod(0o600)
    class Response(io.BytesIO):
     status=200
    requests=[]
    def fetch(request,timeout):
     requests.append(request.full_url)
     path=urllib.parse.urlsplit(request.full_url).path
     if path=='/v1/acl/token/self':return Response(json.dumps({'Type':'client' if kind=='not-management' else 'management'}).encode())
     if path.startswith('/v1/allocation/'):
      code=403 if kind=='forbidden' else 404
      raise urllib.error.HTTPError(request.full_url,code,'metadata',{},None)
     if path=='/v1/node/expected-node':return Response(json.dumps({'ID':'wrong-node' if kind=='wrong-node' else 'expected-node'}).encode())
     return Response(json.dumps([{'ID':'other','ClientStatus':'running'}] if kind=='active-node' else []).encode())
    opener=mock.Mock();opener.open.side_effect=fetch
    argv=['reader','https://nomad.example.test',str(token),'json','missing-allocation','agent-workers','expected-node']
    with mock.patch.object(sys,'argv',argv),mock.patch('urllib.request.build_opener',return_value=opener),contextlib.redirect_stdout(io.StringIO()) as output,contextlib.redirect_stderr(io.StringIO()) as err:
     if kind in ['forbidden','wrong-node','not-management']:
      with self.assertRaises(SystemExit):exec(m.OPERATOR_READ_GC,{})
      self.assertEqual(output.getvalue(),'')
     else:
      exec(m.OPERATOR_READ_GC,{})
      result=json.loads(output.getvalue())['allocation']
      self.assertEqual(result['HTTPStatus'],404)
      self.assertEqual(result['NodeActiveAllocations'],1 if kind=='active-node' else 0)
    self.assertNotIn('synthetic-management-token',output.getvalue()+err.getvalue())
    if kind=='forbidden':self.assertEqual(len(requests),2)
    if kind=='not-management':self.assertEqual(len(requests),1)

class AbandonedJobVerifierTests(unittest.TestCase):
 def test_current_job_get_is_fresh_filtered_and_required(self):
  import urllib.error,urllib.parse
  for status in [200,403,404]:
   with self.subTest(status=status),tempfile.TemporaryDirectory() as folder:
    token=pathlib.Path(folder)/'auth';token.write_text(json.dumps({'SecretID':'synthetic-no-output'}));token.chmod(0o600)
    class Response(io.BytesIO):status=200
    seen=[]
    def fetch(request,timeout):
     seen.append(request.full_url);path=urllib.parse.urlsplit(request.full_url).path
     if path=='/v1/acl/token/self':value={'Type':'management'}
     elif path.startswith('/v1/allocation/'):
      value={'ID':'alloc','NodeID':'node','JobID':'job','Namespace':'workers','ClientStatus':'failed','NextAllocation':'','ModifyIndex':9,'TaskStates':{'worker':{'State':'dead'}}}
     elif path=='/v1/job/job':
      if status!=200:raise urllib.error.HTTPError(request.full_url,status,'metadata',{},None)
      value={'ID':'job','Namespace':'workers','Stop':True,'Status':'dead','ModifyIndex':10,'Meta':{'creator_agent_id':'owner','creator_session_id':'session'},'TaskGroups':[{'Env':{'SECRET':'must-not-output'}}]}
     else:raise AssertionError(path)
     return Response(json.dumps(value).encode())
    opener=mock.Mock();opener.open.side_effect=fetch
    argv=['reader','https://nomad.example.test',str(token),'json','alloc','workers','node','job']
    with mock.patch.object(sys,'argv',argv),mock.patch('urllib.request.build_opener',return_value=opener),contextlib.redirect_stdout(io.StringIO()) as out,contextlib.redirect_stderr(io.StringIO()) as err:
     if status==200:
      exec(m.OPERATOR_READ_GC,{})
      proof=json.loads(out.getvalue())['allocation']['CurrentJob'];self.assertEqual(proof['ModifyIndex'],10);self.assertTrue(proof['Stop'])
     else:
      with self.assertRaises(SystemExit):exec(m.OPERATOR_READ_GC,{})
      self.assertEqual(out.getvalue(),'')
    self.assertIn('/v1/job/job?namespace=workers',seen[-1]);self.assertNotIn('synthetic-no-output',out.getvalue()+err.getvalue());self.assertNotIn('must-not-output',out.getvalue())

class AbandonedGCJobTests(unittest.TestCase):
 def test_job_and_allocation_gc_require_namespace_absence(self):
  import urllib.error,urllib.parse
  for kind in ['absent','job-listed','alloc-listed','forbidden','wrong-namespace']:
   with self.subTest(kind=kind),tempfile.TemporaryDirectory() as folder:
    token=pathlib.Path(folder)/'auth';token.write_text(json.dumps({'SecretID':'synthetic-only'}));token.chmod(0o600)
    class Response(io.BytesIO):status=200
    def fetch(request,timeout):
     url=urllib.parse.urlsplit(request.full_url);path=url.path
     if path=='/v1/acl/token/self':v={'Type':'management'}
     elif path in ['/v1/allocation/alloc','/v1/job/job']:raise urllib.error.HTTPError(request.full_url,404,'metadata',{},None)
     elif path=='/v1/node/node':v={'ID':'node'}
     elif path=='/v1/node/node/allocations':v=[]
     elif path=='/v1/jobs':
      self.assertEqual(url.query,'namespace=workers')
      if kind=='forbidden':raise urllib.error.HTTPError(request.full_url,403,'metadata',{},None)
      v=[{'ID':'job','Namespace':'workers'}] if kind=='job-listed' else []
     elif path=='/v1/allocations':
      self.assertEqual(url.query,'namespace=workers');v=[{'ID':'other','JobID':'job','Namespace':'workers'}] if kind=='alloc-listed' else ([{'ID':'other','JobID':'other','Namespace':'foreign'}] if kind=='wrong-namespace' else [])
     else:raise AssertionError(path)
     return Response(json.dumps(v).encode())
    opener=mock.Mock();opener.open.side_effect=fetch
    with mock.patch.object(sys,'argv',['reader','https://nomad.example.test',str(token),'json','alloc','workers','node','job']),mock.patch('urllib.request.build_opener',return_value=opener),contextlib.redirect_stdout(io.StringIO()) as out,contextlib.redirect_stderr(io.StringIO()):
     if kind=='absent':
      exec(m.OPERATOR_READ_GC,{});j=json.loads(out.getvalue())['allocation']['CurrentJob'];self.assertEqual(j['HTTPStatus'],404);self.assertTrue(j['AbsenceVerified'])
     else:
      with self.assertRaises(SystemExit):exec(m.OPERATOR_READ_GC,{})
      self.assertEqual(out.getvalue(),'')

if __name__=='__main__':unittest.main()
