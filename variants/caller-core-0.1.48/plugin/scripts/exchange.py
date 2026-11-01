"""Operator-configured S3 exchange. No persistent credentials in guest or receipts."""
import datetime,hashlib,hmac,json,os,re,stat,time,uuid,urllib.parse,urllib.request,urllib.error
from pathlib import Path
class ExchangeError(RuntimeError):pass
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*a,**k):raise ExchangeError('exchange_redirect_refused')
def protected(path):
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o600 or not stat.S_ISREG(st.st_mode):raise ExchangeError('exchange_config_permissions')
  return json.load(f)
def config(path):
 d=protected(path)
 if set(d)!={'endpoint','bucket','region','bao_address','bao_path','token_file'}:raise ExchangeError('exchange_config_shape')
 for name in ['endpoint','bao_address']:
  u=urllib.parse.urlsplit(d[name])
  if u.scheme!='https' or not u.hostname or u.username or u.password or u.path or u.query or u.fragment:raise ExchangeError('exchange_https_origin_required')
 if not re.fullmatch('[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]',d['bucket']):raise ExchangeError('exchange_bucket')
 if not re.fullmatch('[a-z0-9-]+',d['region']):raise ExchangeError('exchange_region')
 if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9/_-]{1,255}',d['bao_path']) or '..' in d['bao_path']:raise ExchangeError('exchange_bao_path')
 return d
def credentials(d):
 fd=os.open(d['token_file'],os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o600 or not stat.S_ISREG(st.st_mode):raise ExchangeError('exchange_token_permissions')
  token=f.read().strip()
 req=urllib.request.Request(d['bao_address']+'/v1/'+d['bao_path'],headers={'X-Vault-Token':token})
 try:
  with urllib.request.build_opener(NoRedirect).open(req,timeout=8) as f:raw=f.read(65537)
 except urllib.error.HTTPError as e:raise ExchangeError('exchange_bao_http_'+str(e.code)) from None
 except Exception:raise ExchangeError('exchange_bao_unavailable') from None
 if len(raw)>65536:raise ExchangeError('exchange_bao_response_limit')
 try:v=json.loads(raw)['data']['data'];a=v['access_key_id'];s=v['secret_access_key']
 except Exception:raise ExchangeError('exchange_credentials_missing') from None
 if not all(isinstance(x,str) and x and len(x)<4096 for x in (a,s)):raise ExchangeError('exchange_credentials_invalid')
 return a,s

def path(value):
 if not isinstance(value,str) or not value or len(value)>256 or '\\' in value or any(x in ('','.','..','.git') for x in value.split('/')):raise ExchangeError('exchange_path')
 if re.search(r'(^|/)(\.env[^/]*|\.ssh|\.aws|\.kube|credentials|id_rsa|id_ed25519)(/|$)|\.(pem|key)$',value):raise ExchangeError('exchange_secret_path')
 return value
def identity(job,sid,rid):
 if not re.fullmatch(r'worker-[0-9a-f-]{36}',job):raise ExchangeError('exchange_job')
 return 'jobs/'+job+'/sessions/'+str(uuid.UUID(sid))+'/requests/'+str(uuid.UUID(rid))+'/'
def canonical(d):return (json.dumps(d,sort_keys=True,separators=(',',':'))+'\n').encode()
def files(items):
 if not isinstance(items,list) or not 1<=len(items)<=4096:raise ExchangeError('exchange_file_count')
 out=[]
 for f in items:
  p=path(f['path']);size=f['size'];sha=f['sha256']
  if type(size) is not int or not 0<=size<=268435456 or not re.fullmatch('[a-f0-9]{64}',sha):raise ExchangeError('exchange_file_metadata')
  out.append({'path':p,'size':size,'sha256':sha,'executable':bool(f.get('executable',False))})
 if len({f['path'] for f in out})!=len(out) or sum(f['size'] for f in out)>1073741824:raise ExchangeError('exchange_total_quota')
 return sorted(out,key=lambda f:f['path'])
class Store:
 def __init__(self,cfg,key=None):self.cfg=cfg;self.key=key or credentials(cfg)
 def url(self,method,key,seconds=300,now=None,query=None):
  if method not in ('GET','PUT','HEAD','POST','DELETE') or type(seconds) is not int or not 1<=seconds<=900:raise ExchangeError('exchange_sign_operation')
  if key.startswith('/') or any(p in ('','.','..') for p in key.split('/')):raise ExchangeError('exchange_key')
  now=now or datetime.datetime.now(datetime.timezone.utc);stamp=now.strftime('%Y%m%dT%H%M%SZ');day=stamp[:8];region=self.cfg['region'];scope=day+'/'+region+'/s3/aws4_request'
  host=urllib.parse.urlsplit(self.cfg['endpoint']).netloc;uri='/'+self.cfg['bucket']+'/'+urllib.parse.quote(key,safe='/~')
  q={'X-Amz-Algorithm':'AWS4-HMAC-SHA256','X-Amz-Credential':self.key[0]+'/'+scope,'X-Amz-Date':stamp,'X-Amz-Expires':str(seconds),'X-Amz-SignedHeaders':'host'}
  if query:
   if set(query)-{'uploads','uploadId','partNumber'}:raise ExchangeError('exchange_sign_query')
   q.update(query)
  query=urllib.parse.urlencode(sorted(q.items()),quote_via=urllib.parse.quote,safe='~')
  request='\n'.join([method,uri,query,'host:'+host+'\n','host','UNSIGNED-PAYLOAD'])
  text='AWS4-HMAC-SHA256\n'+stamp+'\n'+scope+'\n'+hashlib.sha256(request.encode()).hexdigest()
  def sign(k,v):return hmac.new(k,v.encode(),hashlib.sha256).digest()
  k=sign(('AWS4'+self.key[1]).encode(),day)
  for v in [region,'s3','aws4_request']:k=sign(k,v)
  return self.cfg['endpoint']+uri+'?'+query+'&X-Amz-Signature='+hmac.new(k,text.encode(),hashlib.sha256).hexdigest()
 def open(self,method,key,body=None,size=None,query=None,headers=None):
  req=urllib.request.Request(self.url(method,key,query=query),method=method,data=body)
  if headers:
   if set(headers)-{'If-Match','If-None-Match'}:raise ExchangeError('exchange_conditional_headers')
   for k,v in headers.items():
    if not isinstance(v,str) or any(c in v for c in '\r\n'):raise ExchangeError('exchange_conditional_value')
    req.add_header(k,v)
  if size is not None:req.add_header('Content-Length',str(size))
  try:return urllib.request.build_opener(NoRedirect).open(req,timeout=30)
  except urllib.error.HTTPError as e:raise ExchangeError('exchange_s3_http_'+str(e.code)) from None
  except Exception:raise ExchangeError('exchange_s3_unavailable') from None
 def put(self,key,raw):
  with self.open('PUT',key,raw,len(raw)) as f:f.read(1024)
 def conditional_put(self,key,raw,etag=None):
  headers={'If-Match':etag} if etag is not None else {'If-None-Match':'*'}
  with self.open('PUT',key,raw,len(raw),headers=headers) as f:f.read(1024)
 def get(self,key,limit=4194304):
  with self.open('GET',key) as f:raw=f.read(limit+1)
  if len(raw)>limit:raise ExchangeError('exchange_object_limit')
  return raw
 def stat(self,key):
  with self.open('HEAD',key) as response:
   etag=response.headers.get('ETag');size=response.headers.get('Content-Length')
  if not etag or size is None:raise ExchangeError('exchange_object_stat_unavailable')
  return {'etag':etag,'bytes':int(size)}
 def verify(self,key,size,sha):
  h=hashlib.sha256();n=0
  with self.open('GET',key) as f:
   while True:
    b=f.read(min(1048576,size-n+1))
    if not b:break
    n+=len(b)
    if n>size:raise ExchangeError('exchange_size_mismatch')
    h.update(b)
  if n!=size or h.hexdigest()!=sha:raise ExchangeError('exchange_hash_mismatch')
 def commit(self,job,sid,rid,items,scope="declared-terminal-results"):
  fs=files(items);prefix=identity(job,sid,rid);doc={'version':1,'scope':scope,'job':job,'session_id':sid,'request_id':rid,'files':fs,'storage_kind':'s3','replicated_backup_verified':False}
  for f in fs:self.verify(prefix+'objects/'+f['sha256'],f['size'],f['sha256'])
  raw=canonical(doc);sha=hashlib.sha256(raw).hexdigest();key=prefix+'manifests/'+sha+'.json';self.put(key,raw)
  if self.get(key)!=raw:raise ExchangeError('exchange_commit_readback')
  # Content-addressed receipt: no mutable pointer, no overwrite of other commits.
  marker=canonical({'manifest_sha256':sha,'manifest_key':key});self.put(prefix+'commits/'+sha+'.json',marker)
  if self.get(prefix+'commits/'+sha+'.json')!=marker:raise ExchangeError('exchange_receipt_readback')
  return {'job':job,'session_id':sid,'request_id':rid,'manifest_sha256':sha,'scope':scope,'storage_kind':'s3','auto_close_enabled':False}
 def manifest(self,job,sid,rid,sha):
  if not re.fullmatch('[a-f0-9]{64}',sha):raise ExchangeError('exchange_commit_hash')
  prefix=identity(job,sid,rid);marker=json.loads(self.get(prefix+'commits/'+sha+'.json'));key=prefix+'manifests/'+sha+'.json'
  if marker!={'manifest_sha256':sha,'manifest_key':key}:raise ExchangeError('exchange_commit_binding')
  raw=self.get(key)
  if hashlib.sha256(raw).hexdigest()!=sha:raise ExchangeError('exchange_manifest_hash')
  d=json.loads(raw)
  if (d.get('job'),d.get('session_id'),d.get('request_id'))!=(job,sid,rid):raise ExchangeError('exchange_manifest_binding')
  d['files']=files(d['files']);return d
