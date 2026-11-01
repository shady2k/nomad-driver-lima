#!/usr/bin/env python3
"""Native Nomad exec with the existing caller identity; never a token installer.

The token stays at its existing configured path and is used only in the native
Nomad child environment. No token value is printed, persisted or put in argv.
"""
import argparse,json,os,pathlib,stat,sys,urllib.parse

def command(config,args):
 if set(config)!={'nomad_binary','nomad_address','token_file'}:raise ValueError('NOMAD_AUTH_REFERENCE_CONFIG_SHAPE')
 endpoint=urllib.parse.urlsplit(config['nomad_address'])
 if endpoint.scheme!='https' or not endpoint.hostname or endpoint.username or endpoint.password or endpoint.path or endpoint.query or endpoint.fragment:raise ValueError('APPROVED_HTTPS_NOMAD_ORIGIN_REQUIRED')
 if not pathlib.Path(config['nomad_binary']).is_absolute():raise ValueError('ABSOLUTE_NATIVE_NOMAD_REQUIRED')
 blocked={'-address','--address','-token','--token','-tls-skip-verify','--tls-skip-verify'}
 if any(a.split('=',1)[0] in blocked or a.startswith(('https://','http://')) for a in args):raise ValueError('NOMAD_AUTH_ENDPOINT_OVERRIDE_BLOCKED')
 path=pathlib.Path(config['token_file'])
 if not path.is_absolute():raise ValueError('EXISTING_ABSOLUTE_TOKEN_REFERENCE_REQUIRED')
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o600:raise ValueError('EXISTING_PRIVATE_CALLER_TOKEN_REQUIRED')
  token=f.read(4097).strip()
 if not token or len(token)>4096:raise ValueError('EXISTING_CALLER_TOKEN_INVALID')
 env={k:os.environ[k] for k in ['HOME','PATH'] if k in os.environ}
 env.update(NOMAD_ADDR=config['nomad_address'],NOMAD_TOKEN=token)
 return config['nomad_binary'],[config['nomad_binary'],*args],env

def main():
 p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('args',nargs=argparse.REMAINDER);a=p.parse_args()
 from exchange import protected
 cfg=protected(a.config);args=a.args[1:] if a.args[:1]==['--'] else a.args
 binary,argv,env=command(cfg,args);os.execve(binary,argv,env)
if __name__=='__main__':
 try:main()
 except Exception:print('NOMAD_CALLER_AUTH_REFERENCE_UNAVAILABLE',file=sys.stderr);raise SystemExit(1)
