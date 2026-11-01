import pathlib,sys,tempfile,unittest,json,uuid,hashlib,tarfile,io,os
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import upload_guest as g
from unittest.mock import patch
import gzip,tracemalloc
class Guest(unittest.TestCase):
 def fixture(self,d,body=b'file bytes',destination='input/source.bin',extract=None):
  root=pathlib.Path(d);state=root/'state';state.mkdir(mode=0o700);workspace=root/'workspace';workspace.mkdir();p={'config':{'state_dir':str(state),'workspace':str(workspace)},'session_id':str(uuid.uuid4()),'upload_id':str(uuid.uuid4()),'size':len(body),'sha256':hashlib.sha256(body).hexdigest(),'destination':destination,'extract_to':extract,'binding':{'owner':'verified-current-creator'}};identity={'machine_id':'a'*32,'boot_id':str(uuid.uuid4())};calls=[]
  def download(url,origin,start,count,total):self.assertLessEqual(count,g.PART);self.assertEqual(total,len(body));calls.append((start,count));return body[start:start+count]
  def run(action,**kw):return g.handle({**p,'action':action,**kw},lambda:identity,download)
  return p,identity,calls,run,workspace
 def transfer(self,p,run,body):
  state=run('status')
  while state['offset']<len(body):
   offset=state['offset'];chunk=body[offset:offset+g.PART];state=run('part',offset=offset,part_size=len(chunk),part_sha256=hashlib.sha256(chunk).hexdigest(),url='https://storage.invalid/object?capability=SECRET',origin='https://storage.invalid')
  return run('finish')
 def test_atomic_regular_upload_repeat_without_overwrite(self):
  with tempfile.TemporaryDirectory()as d:
   body=b'private synthetic source';p,identity,calls,run,workspace=self.fixture(d,body);self.assertEqual(self.transfer(p,run,body)['phase'],'completed');before=(workspace/p['destination']).stat().st_ino;self.assertEqual(run('finish')['phase'],'completed');self.assertEqual((workspace/p['destination']).stat().st_ino,before);self.assertEqual(len(calls),1)
 def test_empty_file_upload(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d,b'');self.assertEqual(self.transfer(p,run,b'')['offset'],0);self.assertEqual((w/p['destination']).read_bytes(),b'')
 def test_resume_after_part_ack_loss(self):
  with tempfile.TemporaryDirectory()as d:
   body=b'x'*(g.PART+31);p,identity,calls,run,w=self.fixture(d,body);run('status');first=body[:g.PART];run('part',offset=0,part_size=len(first),part_sha256=hashlib.sha256(first).hexdigest(),url='https://s/object?SECRET',origin='https://s');self.assertEqual(run('status')['offset'],g.PART);self.transfer(p,run,body);self.assertEqual(calls,[(0,g.PART),(g.PART,31)])
 def test_no_capability_or_source_content_in_guest_state(self):
  with tempfile.TemporaryDirectory()as d:
   body=b'NOT_A_SECRET_SYNTHETIC_SOURCE';p,identity,calls,run,w=self.fixture(d,body);self.transfer(p,run,body)
   text=''.join(f.read_text()for f in pathlib.Path(p['config']['state_dir']).rglob('*.json'));self.assertNotIn('SECRET',text);self.assertNotIn('https://',text);self.assertNotIn(body.decode(),text)
 def test_traversal_absolute_and_symlink_parent_rejected(self):
  for path in ['../escape','/absolute','a/../escape','a//file','a/./file']:
   with self.subTest(path=path):self.assertRaises(RuntimeError,g.relative,path)
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);(w/'input').symlink_to(pathlib.Path(d));self.assertRaises(OSError,run,'status');self.assertEqual(calls,[])
 def test_existing_destination_is_never_overwritten(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);(w/'input').mkdir();(w/p['destination']).write_bytes(b'unrelated');self.assertRaisesRegex(RuntimeError,'NO_OVERWRITE',run,'status');self.assertEqual((w/p['destination']).read_bytes(),b'unrelated')
 def test_boot_or_binding_change_rejected(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);run('status');identity['boot_id']=str(uuid.uuid4());self.assertRaisesRegex(RuntimeError,'BINDING_CHANGED',run,'status')
 def test_bad_part_hash_preserves_unpublished_destination(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);run('status');self.assertRaisesRegex(RuntimeError,'PART_HASH',run,'part',offset=0,part_size=p['size'],part_sha256='0'*64,url='https://s',origin='https://s');self.assertFalse((w/p['destination']).exists())
 def tar(self,members):
  out=io.BytesIO()
  with tarfile.open(fileobj=out,mode='w')as tar:
   for name,kind,body in members:
    info=tarfile.TarInfo(name);info.type=kind;info.size=len(body)if kind==tarfile.REGTYPE else 0
    if kind in (tarfile.SYMTYPE,tarfile.LNKTYPE):info.linkname='../../outside'
    tar.addfile(info,io.BytesIO(body)if info.size else None)
  return out.getvalue()
 def test_explicit_tar_extract_atomic_and_repeatable(self):
  with tempfile.TemporaryDirectory()as d:
   body=self.tar([('./',tarfile.DIRTYPE,b''),('./repo/main.go',tarfile.REGTYPE,b'package main\n')]);p,identity,calls,run,w=self.fixture(d,body,'input/source.tar','source');final=self.transfer(p,run,body);self.assertTrue(final['extract_verified']);self.assertEqual((w/'source/repo/main.go').read_bytes(),b'package main\n');self.assertTrue(run('finish')['extract_verified'])
 def test_unsafe_tar_does_not_publish(self):
  for name,kind in [('../outside',tarfile.REGTYPE),('/absolute',tarfile.REGTYPE),('link',tarfile.SYMTYPE),('hard',tarfile.LNKTYPE),('device',tarfile.CHRTYPE)]:
   with self.subTest(name=name),tempfile.TemporaryDirectory()as d:
    body=self.tar([(name,kind,b'x')]);p,identity,calls,run,w=self.fixture(d,body,'input/source.tar','source');self.assertRaises(RuntimeError,self.transfer,p,run,body);self.assertFalse((w/'source').exists())
 def test_extraction_does_not_overwrite_existing_tree(self):
  with tempfile.TemporaryDirectory()as d:
   body=self.tar([('file',tarfile.REGTYPE,b'new')]);p,identity,calls,run,w=self.fixture(d,body,'input/source.tar','source');(w/'source').mkdir();(w/'source/file').write_text('original');self.assertRaisesRegex(RuntimeError,'DESTINATION_EXISTS',self.transfer,p,run,body);self.assertEqual((w/'source/file').read_text(),'original')
 def test_duplicate_tar_paths_rejected(self):
  with tempfile.TemporaryDirectory()as d:
   body=self.tar([('file',tarfile.REGTYPE,b'a'),('./file',tarfile.REGTYPE,b'b')]);p,identity,calls,run,w=self.fixture(d,body,'input/source.tar','source');self.assertRaisesRegex(RuntimeError,'DUPLICATE_PATH',self.transfer,p,run,body);self.assertFalse((w/'source').exists())
 def test_crash_before_partial_identity_commit_resumes_safely(self):
  with tempfile.TemporaryDirectory()as d:
   body=b'synthetic recovery';p,identity,calls,run,w=self.fixture(d,body);original=g.atomic
   def crash(path,value):
    if value.get('partial_identity'):raise OSError('synthetic crash')
    return original(path,value)
   with patch.object(g,'atomic',side_effect=crash):self.assertRaises(OSError,run,'status')
   self.assertEqual(run('status')['offset'],0)
   self.assertEqual(self.transfer(p,run,body)['phase'],'completed')
 def test_compressed_pax_metadata_bomb_rejected_before_allocation(self):
  out=io.BytesIO()
  with tarfile.open(fileobj=out,mode='w',format=tarfile.PAX_FORMAT,pax_headers={'comment':'x'*(16*1024*1024)})as tar:
   info=tarfile.TarInfo('file');info.size=1;tar.addfile(info,io.BytesIO(b'x'))
  body=gzip.compress(out.getvalue())
  with tempfile.TemporaryDirectory()as d:
   archive=pathlib.Path(d)/'source.tar.gz';archive.write_bytes(body);tracemalloc.start()
   try:
    with archive.open('rb')as stream:self.assertRaisesRegex(RuntimeError,'METADATA_BOUND',g.archive_preflight,stream)
    self.assertLess(tracemalloc.get_traced_memory()[1],4*1024*1024)
   finally:tracemalloc.stop()
 def test_inspect_is_readonly_when_transfer_absent(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);self.assertEqual(run('inspect')['phase'],'absent');self.assertEqual(list(pathlib.Path(p['config']['state_dir']).iterdir()),[])
 def test_abort_partial_is_idempotent_and_fences_late_part(self):
  with tempfile.TemporaryDirectory()as d:
   body=b'x'*(g.PART+1);p,identity,calls,run,w=self.fixture(d,body);run('status');first=body[:g.PART];run('part',offset=0,part_size=len(first),part_sha256=hashlib.sha256(first).hexdigest(),url='https://s',origin='https://s')
   self.assertEqual(run('abort')['phase'],'aborted');self.assertEqual(run('abort')['phase'],'aborted');self.assertEqual(run('part')['phase'],'aborted');self.assertEqual(len(calls),1);self.assertFalse((w/p['destination']).exists());self.assertEqual(list(w.rglob('*.partial')),[])
 def test_abort_never_deletes_published_output(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);self.transfer(p,run,b'file bytes');self.assertEqual(run('abort')['phase'],'aborted');self.assertEqual((w/p['destination']).read_bytes(),b'file bytes')
 def test_abort_refuses_changed_partial_inode(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);run('status');partial=next(w.rglob('*.partial'));other=partial.with_suffix('.other');other.write_bytes(b'unrelated');os.replace(other,partial);self.assertRaisesRegex(RuntimeError,'PARTIAL_PATH_CHANGED',run,'abort');self.assertEqual(partial.read_bytes(),b'unrelated')
 def test_abort_intent_survives_crash_before_terminal_commit(self):
  with tempfile.TemporaryDirectory()as d:
   p,identity,calls,run,w=self.fixture(d);original=g.atomic
   def crash(path,value):
    original(path,value)
    if value.get('abort_intent')and value['phase']!='aborted':raise OSError('synthetic crash after abort intent')
   with patch.object(g,'atomic',side_effect=crash):self.assertRaises(OSError,run,'abort')
   self.assertEqual(run('inspect')['phase'],'abort_pending');self.assertEqual(run('part')['phase'],'aborted');self.assertEqual(calls,[]);self.assertFalse((w/p['destination']).exists())
if __name__=='__main__':unittest.main()
