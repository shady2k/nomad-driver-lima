"""Redacted diagnostic text; protected segmented spool, never an execution API."""
import io,json,os,re,stat,threading,time,urllib.parse
from pathlib import Path

class Redactor:
 def __init__(self,values=()):self.values=[];self.add(*values)
 def add(self,*values):
  for value in values:
   if isinstance(value,str) and value:
    for candidate in [value,json.dumps(value)[1:-1],urllib.parse.quote(value,safe=''),urllib.parse.quote_plus(value)]:
     if candidate not in self.values:self.values.append(candidate)
  self.values.sort(key=len,reverse=True)
 def text(self,text):
  for value in self.values:text=text.replace(value,'[REDACTED]')
  key=r'''["']?\b(?:authorization|proxy-authorization|[A-Z_][A-Z0-9_-]*(?:TOKEN|KEY|PASSWORD|PASSWD|SECRET|SEED)|TOKEN|KEY|PASSWORD|PASSWD|SECRET|SEED)["']?\s*[:=]\s*'''
  quoted=r'''("(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$))'''
  text=re.sub('('+key+')'+quoted,r'\1"[REDACTED]"',text,flags=re.I)
  text=re.sub(r'(?i)(\b(?:authorization|x-vault-token|x-nomad-token)\s*[:=]\s*)(?:Bearer\s+|Basic\s+)?[^\s,;"\'}]+',r'\1[REDACTED]',text)
  text=re.sub(r'(?i)([?&](?:token|access_token|api_key|key|secret|client_secret|password)=)[^&#\s"\']+',r'\1[REDACTED]',text)
  text=re.sub(r'(https?://)[^/@\s:]+:[^/@\s]+@',r'\1[REDACTED]@',text)
  text=re.sub(r'(?i)([\"\']?\b[A-Z_]*(?:TOKEN|API_KEY|PASSWORD|PASSWD|SECRET|SEED)[\"\']?\s*[:=]\s*)([\"\'])(.*?)\2',r'\1\2[REDACTED]\2',text)
  text=re.sub(r'(?i)(\b[A-Z_]*(?:TOKEN|API_KEY|PASSWORD|PASSWD|SECRET|SEED)\s*=\s*)[^\s,;&?#]+',r'\1[REDACTED]',text)
  text=re.sub(r'\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b','[REDACTED_JWT]',text)
  # Presigned object URLs are bearer capabilities: remove the whole query,
  # including X-Amz-* fields, not just generic token/key parameter names.
  def safe_url(match):
   value=match.group(0)
   try:
    u=urllib.parse.urlsplit(value)
    if u.query:return urllib.parse.urlunsplit((u.scheme,u.netloc,u.path,'[REDACTED]',''))
   except ValueError:return '[REDACTED_URL]'
   return value
  text=re.sub(r'https?://[^\s\"\'<>]+',safe_url,text)
  return text

class Spool:
 """Retained until explicit operator removal; no overwrite/delete on rotation.
 At max_bytes, fail closed with an explicit error, retaining every prior segment.
 """
 def __init__(self,directory,redactor=None,segment_bytes=16*1024*1024,max_bytes=256*1024*1024):
  self.path=Path(directory);self.redactor=redactor or Redactor();self.segment_bytes=segment_bytes;self.max_bytes=max_bytes;self.lock=threading.Lock()
  self.path.mkdir(mode=0o700,parents=True,exist_ok=True)
  st=self.path.lstat()
  if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o700:raise ValueError('Unsafe diagnostic spool')
  files=sorted(self.path.glob('*.jsonl'));self.total=sum(f.stat().st_size for f in files);self.index=len(files);self.size=0
 def append(self,stream,text):
  raw=(json.dumps({'timestamp':time.time(),'stream':stream,'text':self.redactor.text(text)},ensure_ascii=False)+'\n').encode()
  with self.lock:
   if self.total+len(raw)>self.max_bytes:raise RuntimeError('diagnostic_spool_quota_exceeded; prior segments retained; output not silently discarded')
   if self.size and self.size+len(raw)>self.segment_bytes:self.index+=1;self.size=0
   path=self.path/('%06d.jsonl'%self.index)
   fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_APPEND|os.O_NOFOLLOW,0o600)
   with os.fdopen(fd,'ab') as out:
    st=os.fstat(out.fileno())
    if st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o600 or not stat.S_ISREG(st.st_mode):raise ValueError('Unsafe diagnostic segment')
    out.write(raw);out.flush();os.fsync(out.fileno())
   self.total+=len(raw);self.size+=len(raw)
  return self.redactor.text(text)

class RedactedWriter(io.TextIOBase):
 def __init__(self,target,redactor):self.target=target;self.redactor=redactor;self.pending='';self.lock=threading.RLock()
 def write(self,text):
  with self.lock:
   self.pending+=str(text)
   while '\n' in self.pending:
    line,self.pending=self.pending.split('\n',1);self.target.write(self.redactor.text(line)+'\n');self.target.flush()
  return len(text)
 def flush(self):
  with self.lock:
   # Keep incomplete protocol lines buffered: flush must not leak a split secret.
   self.target.flush()
