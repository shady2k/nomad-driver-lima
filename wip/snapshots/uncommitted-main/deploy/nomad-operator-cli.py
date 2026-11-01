#!/usr/bin/env python3
"""Use the configured existing operator identity locally; never output a token."""
import json,os,pathlib,sys
config=pathlib.Path(os.environ.get('NOMAD_WORKERS_OPERATOR_CONFIG',str(pathlib.Path.home()/'.config/nomad-workers/operator.json')))
st=config.lstat()
assert config.is_file()and not config.is_symlink()and st.st_uid==os.getuid()and st.st_mode&0o077==0,'private operator config required'
s=json.loads(config.read_text());p=pathlib.Path(s['bootstrap_file']);token=json.loads(p.read_text())['SecretID']
env=dict(os.environ,NOMAD_ADDR=s['nomad_address'],NOMAD_TOKEN=token)
os.execve(s['nomad_binary'],['nomad',*sys.argv[1:]],env)
