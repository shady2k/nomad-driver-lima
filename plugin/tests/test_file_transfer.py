"""Actual multipart manager + finite guest handler with synthetic S3 protocol."""
import pathlib,sys,tempfile,unittest,json,uuid,hashlib,io,urllib.parse,subprocess,types,contextlib,tracemalloc
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import file_transfer as t,upload_guest as guest,exchange,storage
import test_guest_exec as fixture
class Response:
 def __init__(self,body=b'',headers=None,status=200):self.body=body if hasattr(body,'read')else io.BytesIO(body);self.headers=headers or{};self.status=status
 def read(self,n=-1):return self.body.read(n)
 def __enter__(self):return self
 def __exit__(self,*a):self.body.close()
class S3(exchange.Store):
 def __init__(self,root):
  super().__init__({'endpoint':'https://storage.invalid','bucket':'fixture','region':'test'},key=('synthetic','synthetic'));self.root=pathlib.Path(root);self.root.mkdir();self.objects={};self.uploads={};self.init_count=0;self.part_sizes=[];self.fail_part_ack=False
 def object(self,key):return self.root/hashlib.sha256(key.encode()).hexdigest()
 def route(self,request,timeout=None):
  url=urllib.parse.urlsplit(request.full_url);q=urllib.parse.parse_qs(url.query,keep_blank_values=True);key=urllib.parse.unquote(url.path.split('/',2)[2]);method=request.get_method();body=request.data or b''
  if method=='POST'and'uploads'in q:
   id=str(uuid.uuid4());self.uploads[id]={};self.init_count+=1;return Response(('<InitiateMultipartUploadResult><UploadId>'+id+'</UploadId></InitiateMultipartUploadResult>').encode())
  if 'uploadId'in q:
   id=q['uploadId'][0];parts=self.uploads[id]
   if method=='PUT':
    number=int(q['partNumber'][0]);self.part_sizes.append(len(body));path=self.root/(id+'-'+str(number));path.write_bytes(body);parts[number]=(path,hashlib.sha256(body).hexdigest())
    if self.fail_part_ack:self.fail_part_ack=False;raise OSError('synthetic ACK loss')
    return Response(headers={'ETag':parts[number][1]})
   if method=='GET':return Response(('<ListPartsResult><IsTruncated>false</IsTruncated>'+''.join('<Part><PartNumber>'+str(n)+'</PartNumber><ETag>'+etag+'</ETag><Size>'+str(p.stat().st_size)+'</Size></Part>'for n,(p,etag)in sorted(parts.items()))+'</ListPartsResult>').encode())
   if method=='POST':
    out=self.object(key)
    with out.open('wb')as f:
     for path,etag in [parts[n]for n in sorted(parts)]:
      with path.open('rb')as source:
       while True:
        b=source.read(1024*1024)
        if not b:break
        f.write(b)
    self.objects[key]=out;return Response(b'<CompleteMultipartUploadResult/>')
  if method=='PUT':
   if key in self.objects and request.get_header('If-none-match')=='*':raise exchange.ExchangeError('exchange_s3_http_412')
   path=self.object(key);path.write_bytes(body);self.objects[key]=path;return Response()
  if key not in self.objects:raise exchange.ExchangeError('exchange_s3_http_404')
  path=self.objects[key]
  if method=='HEAD':return Response(headers={'ETag':hashlib.sha256(key.encode()).hexdigest(),'Content-Length':str(path.stat().st_size)})
  stream=path.open('rb');range_=request.get_header('Range')
  if range_:
   start,end=map(int,range_[6:].split('-'));stream.seek(start);raw=stream.read(end-start+1);stream.close();return Response(raw,{'Content-Range':f'bytes {start}-{end}/{path.stat().st_size}'},206)
  return Response(stream)
 def open(self,method,key,body=None,size=None,query=None,headers=None):
  import urllib.request
  return self.route(urllib.request.Request(self.url(method,key,query=query),data=body,method=method,headers=headers or{}))
class Upload(unittest.TestCase):
 def fixture(self,d,size):
  root=pathlib.Path(d);a,s,row,owner=fixture.ExecTests().fixture(d);workspace=root/'workspace';workspace.mkdir();state=root/'guest';state.mkdir(mode=0o700);a.c.update(job='worker-'+s,guest={'workspace':str(workspace),'state_dir':str(state)},storage={'backend':'s3'});row['Job']['ID']=a.c['job'];a.hash=hashlib.sha256(t.canonical(a.c)).hexdigest();saved=json.loads((a.local(s)/'session.json').read_text());saved.update(config_hash=a.hash,attempt=a.attempt(row));t.atomic(a.local(s)/'session.json',saved);source=root/'source.bin'
  with source.open('wb')as f:
   while f.tell()<size:f.write(b'x'*min(1024*1024,size-f.tell()))
  server=S3(root/'s3');identity={'machine_id':'a'*32,'boot_id':str(uuid.uuid4())};calls=[];lose=[False]
  def nomad(args,**kw):
   self.assertEqual(args[:2],['alloc','exec']);self.assertNotIn('eHh4eHh4',''.join(args));compile(args[-1],'<upload-helper>','exec');p=json.loads(kw['input']);calls.append(p['action']);result=guest.handle(p,lambda:identity)
   if lose[0]and p['action']=='part':lose[0]=False;raise TimeoutError('synthetic guest ACK loss')
   return subprocess.CompletedProcess(args,0,json.dumps({'ok':True,'result':result}),'')
  a.nomad=nomad;service=types.SimpleNamespace(resolve=lambda _:(a,{}));return a,s,service,source,server,calls,lose,workspace
 @contextlib.contextmanager
 def transport(self,server):
  class Opener:
   def open(self,request,timeout=None):return server.route(request,timeout)
  with patch.object(storage,'store',return_value=(server,storage.binding(server.cfg))),patch('urllib.request.build_opener',return_value=Opener()):yield
 def test_real_multipart_67_mib_input_bounded_and_atomic(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,67*1024**2);tracemalloc.start()
   with self.transport(server):out=t.upload(service,s,str(source),'input/history.bundle')
   peak=tracemalloc.get_traced_memory()[1];tracemalloc.stop();self.assertEqual(out['state'],'uploaded');self.assertEqual(server.init_count,1);self.assertEqual(len(server.part_sizes),9);self.assertLessEqual(max(server.part_sizes),t.PART);self.assertLess(peak,48*1024**2);self.assertEqual((w/'input/history.bundle').stat().st_size,67*1024**2);self.assertNotIn('submit',calls)
 def test_lost_part_ack_resumes_same_s3_upload(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,9*1024**2);server.fail_part_ack=True
   with self.transport(server):
    self.assertRaises(OSError,t.upload,service,s,str(source),'input/source.bin');out=t.upload(service,s,str(source),'input/source.bin')
   self.assertEqual(out['state'],'uploaded');self.assertEqual(server.init_count,1)
 def test_guest_ack_loss_resume_and_duplicate_do_not_overwrite(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,9*1024**2);lose[0]=True
   with self.transport(server):
    self.assertRaisesRegex(RuntimeError,'TRANSPORT_UNKNOWN',t.upload,service,s,str(source),'input/source.bin');out=t.upload(service,s,str(source),'input/source.bin');inode=(w/'input/source.bin').stat().st_ino;again=t.upload(service,s,str(source),'input/source.bin')
   self.assertEqual(out['sha256'],again['sha256']);self.assertEqual((w/'input/source.bin').stat().st_ino,inode);self.assertEqual(server.init_count,1);self.assertEqual(a.load(s)['activity_revision'],1)
 def test_missing_config_fails_before_activity_intent(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,10);a.c.pop('storage');a.hash=hashlib.sha256(t.canonical(a.c)).hexdigest();state=json.loads((a.local(s)/'session.json').read_text());state['config_hash']=a.hash;t.atomic(a.local(s)/'session.json',state);self.assertRaisesRegex(RuntimeError,'EXISTING_S3',t.upload,service,s,str(source),'input/file');self.assertEqual(list(a.local(s).glob('upload-*.json')),[]);self.assertEqual(calls,[])

 def test_abort_after_unknown_part_is_verified_without_replay(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,9*1024**2);lose[0]=True
   with self.transport(server):
    self.assertRaisesRegex(RuntimeError,'TRANSPORT_UNKNOWN',t.upload,service,s,str(source),'input/file')
    row=json.loads(next(a.local(s).glob('upload-*.json')).read_text());identifier=row['operation_id'];out=t.control(service,s,None,'abort');again=t.control(service,s,identifier,'abort')
   self.assertTrue(out['verified_terminal']);self.assertEqual(again['state'],'upload_aborted');self.assertEqual(calls.count('part'),1);self.assertTrue(source.exists());self.assertFalse((w/'input/file').exists())
 def test_reconcile_lost_final_ack_uses_existing_bytes_no_part_replay(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,10);original=t.rpc
   def lost(*args,**kw):
    out=original(*args,**kw)
    if args[3]=='finish':raise RuntimeError('SYNTHETIC_LOST_FINAL_ACK')
    return out
   with self.transport(server):
    with patch.object(t,'rpc',side_effect=lost):self.assertRaisesRegex(RuntimeError,'LOST_FINAL_ACK',t.upload,service,s,str(source),'input/file')
    row=json.loads(next(a.local(s).glob('upload-*.json')).read_text());before=calls.count('part');out=t.control(service,s,row['operation_id'],'reconcile')
   self.assertEqual(out['state'],'upload_completed');self.assertEqual(calls.count('part'),before);self.assertEqual((w/'input/file').read_bytes(),source.read_bytes())

 def test_reconcile_host_abort_intent_when_abort_never_reached_guest(self):
  with tempfile.TemporaryDirectory()as d:
   a,s,service,source,server,calls,lose,w=self.fixture(d,10);original=t.rpc
   def stop_before_finish(*args,**kw):
    if args[3]=='finish':raise RuntimeError('SYNTHETIC_BEFORE_FINISH')
    return original(*args,**kw)
   with self.transport(server):
    with patch.object(t,'rpc',side_effect=stop_before_finish):self.assertRaisesRegex(RuntimeError,'BEFORE_FINISH',t.upload,service,s,str(source),'input/file')
    with patch.object(t,'rpc',side_effect=RuntimeError('SYNTHETIC_ABORT_NOT_SENT')):self.assertRaisesRegex(RuntimeError,'ABORT_NOT_SENT',t.control,service,s,None,'abort')
    out=t.control(service,s,None,'reconcile')
   self.assertEqual(out['state'],'upload_aborted');self.assertNotIn('finish',calls);self.assertFalse((w/'input/file').exists())
