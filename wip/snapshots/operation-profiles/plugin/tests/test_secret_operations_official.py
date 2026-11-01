"""Opt-in installed official Bao + synthetic loopback TLS, no real secrets/model."""
import os,sys,pathlib,json,ssl,threading,tempfile,subprocess,time,uuid,unittest
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
import test_secret_operations as fixture
import operations,workerctl as w

@unittest.skipUnless(os.environ.get('NOMAD_WORKERS_TEST_BAO_BIN'),'Opt-in installed official Bao fixture')
class OfficialSecretOperationTests(unittest.TestCase):
 def test_official_supervisors_two_clients_renew_cancel_and_safe_output(self):
  bao=os.environ['NOMAD_WORKERS_TEST_BAO_BIN']
  self.assertIn('2.5.4',subprocess.check_output([bao,'version'],text=True))
  with tempfile.TemporaryDirectory()as d:
   t=fixture.SecretOperationTests();base,a,s,gs,calls,code,root,delivery=t.fixture(d)
   cert,key=pathlib.Path(d)/'cert.pem',pathlib.Path(d)/'key.pem'
   subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1','-keyout',str(key),'-out',str(cert)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   counts={'lookup':0,'renew':0,'secret':0,'revoke':0};access={'denied':False}
   class H(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def reply(self,value):
     self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(json.dumps(value).encode())
    def do_GET(self):
     if access['denied']:
      self.send_response(403);self.end_headers();return
     if self.path.startswith('/v1/sys/internal/ui/mounts'):v={'data':{'type':'kv','path':'models/','options':{'version':'2'}}}
     elif self.path.startswith('/v1/sys/health'):v={'initialized':True,'sealed':False,'version':'2.5.4'}
     elif self.path.startswith('/v1/auth/token/lookup-self'):counts['lookup']+=1;v={'data':{'id':'synthetic-task-token','ttl':2,'renewable':True,'type':'service','policies':['fixture']}}
     elif self.path.startswith('/v1/models/data/test'):counts['secret']+=1;v={'data':{'data':{'API_KEY':'synthetic-process-secret'}}}
     else:self.send_response(404);self.end_headers();return
     self.reply(v)
    def do_POST(self):
     self.rfile.read(int(self.headers.get('Content-Length','0')))
     if self.path=='/v1/auth/token/renew-self':counts['renew']+=1;self.reply({'auth':{'client_token':'synthetic-task-token','lease_duration':2,'renewable':True}})
     elif self.path=='/v1/auth/token/revoke-self':counts['revoke']+=1;self.reply({})
     else:self.send_response(404);self.end_headers()
    do_PUT=do_POST
   server=ThreadingHTTPServer(('127.0.0.1',0),H);tls=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);tls.load_cert_chain(cert,key);server.socket=tls.wrap_socket(server.socket,server_side=True);thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
   created=[]
   try:
    delivery.update(address='https://127.0.0.1:'+str(server.server_port),bao_bin=bao);w.atomic(root/'secret-profiles.json',delivery)
    # Test-only additional CA: production code never disables TLS verification.
    code=code.replace("config.write_text(hcl)","config.write_text(hcl.replace('vault { address = ', 'vault { ca_cert = '+json.dumps("+repr(str(cert))+")+' address = '))")
    one,two=str(uuid.uuid4()),str(uuid.uuid4());script='import os,time;assert not any(k in os.environ for k in ("VAULT_TOKEN","BAO_TOKEN","NOMAD_TOKEN"));print(os.environ["LLM_API_KEY"],flush=True);time.sleep(4)'
    t.start(a,s,one,'prime',script,code,timeout=15);created.append(one);t.start(a,s,two,'opencode',script,code,timeout=15);created.append(two)
    deadline=time.monotonic()+10
    while any(operations.status(a,s,op)['phase']=='launching'for op in (one,two))and time.monotonic()<deadline:time.sleep(.05)
    self.assertEqual(operations.status(a,s,two)['phase'],'running',str(counts)+str(operations.status(a,s,two)['state'].get('error')));operations.cancel(a,s,one)
    self.assertEqual(base.done(a,s,one)['phase'],'cancelled');self.assertEqual(base.done(a,s,two)['phase'],'completed')
    self.assertGreaterEqual(counts['lookup'],2);self.assertGreaterEqual(counts['secret'],2);self.assertGreater(counts['renew'],0);self.assertEqual(counts['revoke'],0)
    for op in (one,two):self.assertNotIn('synthetic-process-secret',json.dumps(operations.logs(a,s,op,0)))
    self.assertEqual(list((root/'secret-operations').iterdir()),[]);self.assertTrue((root/'vault_token').exists())
    access['denied']=True;third=str(uuid.uuid4());marker=pathlib.Path(d)/'denied-client-started'
    t.start(a,s,third,'opencode','import pathlib;pathlib.Path('+repr(str(marker))+').write_text("must-not-run")',code,timeout=2);created.append(third)
    rejected=base.done(a,s,third);self.assertIn(rejected['phase'],('failed','timed_out'));self.assertFalse(marker.exists());self.assertFalse(rejected['state']['effects_may_be_partial']);self.assertEqual(counts['revoke'],0)
   finally:
    for op in created:
     try:
      if operations.status(a,s,op)['phase']not in operations.TERMINAL:operations.cancel(a,s,op);base.done(a,s,op)
     except Exception:pass
    server.shutdown();server.server_close();thread.join(timeout=2)
