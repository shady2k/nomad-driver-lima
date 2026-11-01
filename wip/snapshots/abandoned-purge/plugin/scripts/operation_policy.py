"""Nonsecret operator policy, separate from immutable existing VM bindings."""
import json,os,pathlib,stat

def load(path):
 if not path:raise ValueError('OPERATION_POLICY_REQUIRED')
 p=pathlib.Path(path);st=p.lstat()
 if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o022:raise ValueError('UNSAFE_OPERATION_POLICY')
 v=json.loads(p.read_text());keys={'foreground_timeout_seconds','default_timeout_seconds','max_timeout_seconds','heartbeat_seconds','max_log_bytes','native_output_bytes','max_operations_per_session'}
 if set(v)!=keys or any(type(x)!=int or x<1 for x in v.values()):raise ValueError('INVALID_OPERATION_POLICY')
 if v['foreground_timeout_seconds']>v['max_timeout_seconds'] or v['default_timeout_seconds']>v['max_timeout_seconds'] or v['max_timeout_seconds']>604800 or v['heartbeat_seconds']>60 or v['max_log_bytes']>67108864 or v['native_output_bytes']>v['max_log_bytes']:raise ValueError('INVALID_OPERATION_POLICY_BOUNDS')
 # New operations use the total configured redacted log allowance. The legacy
 # native_output_bytes field remains accepted for existing policy files.
 v['native_output_bytes']=v['max_log_bytes']
 return v
