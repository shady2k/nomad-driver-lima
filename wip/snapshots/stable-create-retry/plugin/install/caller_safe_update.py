"""Native Claude >=2.1.296 same-name source update; never remove/uninstall.
No owner adoption/migration or current-session reload. Existing records stay byte-identical.
"""
import hashlib,json,os,pathlib,subprocess,sys

def snapshot(root):
 root=pathlib.Path(root)
 if not root.exists():return {}
 if root.is_symlink():raise RuntimeError('STATE_ROOT_SYMLINK')
 out={}
 for p in sorted(root.rglob('*')):
  if p.is_symlink():raise RuntimeError('STATE_SYMLINK')
  if p.is_file():out[str(p.relative_to(root))]=hashlib.sha256(p.read_bytes()).hexdigest()
 return out

def source_matches(installed,source):
 expected={k:v for k,v in snapshot(source).items() if '__pycache__' not in pathlib.PurePosixPath(k).parts};actual=snapshot(installed)
 return all(actual.get(k)==v for k,v in expected.items()) and all(k in expected or '__pycache__' in pathlib.PurePosixPath(k).parts or k=='.orphaned_at' for k in actual)

def update(home,cli,release,version):
 home=pathlib.Path(home);release=pathlib.Path(release);config=home/'.claude';plugin='nomad-workers@nomad-workers-local'
 registry=config/'plugins/installed_plugins.json';before=json.loads(registry.read_text())['plugins'];rows=before.get(plugin,[])
 if len(rows)!=1 or rows[0].get('scope')!='user':raise RuntimeError('EXISTING_USER_INSTALL_REQUIRED')
 old=pathlib.Path(rows[0]['installPath']);old_files=snapshot(old)
 protected=[home/'.local/state/nomad-workers/initiators',config/'plugins/data/nomad-workers-nomad-workers-local']
 saved=[snapshot(p) for p in protected]
 settings=json.loads((config/'settings.json').read_text());enabled=settings.get('enabledPlugins',{})
 current=json.loads((old/'.claude-plugin/plugin.json').read_text())['version']
 if current==version:
  if not source_matches(old,release/'plugin'):raise RuntimeError('SAME_VERSION_SOURCE_DIFFERS')
  return {'changed':False,'version':version,'owner_state_preserved':True,'old_cache_preserved':True}
 def run(args):
  p=subprocess.run([str(cli),*args],env={**os.environ,'HOME':str(home),'CLAUDE_CONFIG_DIR':str(config)},capture_output=True,text=True,timeout=45)
  if p.returncode:raise RuntimeError('NATIVE_UPDATE_FAILED_NO_REMOVE_OR_RETRY')
 # Supported same-name replacement keeps installed-plugin and plugin-managed data.
 try:
  run(['plugin','marketplace','add',str(release),'--scope','user'])
  run(['plugin','marketplace','update','nomad-workers-local'])
  run(['plugin','update',plugin,'--scope','user','--json'])
 finally:
  if any(snapshot(p)!=v for p,v in zip(protected,saved)):raise RuntimeError('OWNER_STATE_CHANGED_STOP')
  after_old=snapshot(old)
  if any(after_old.get(k)!=v for k,v in old_files.items()) or set(after_old)-set(old_files)-{'.orphaned_at'}:raise RuntimeError('OLD_CACHE_CHANGED_STOP')
 after=json.loads(registry.read_text())['plugins']
 if {k:v for k,v in after.items() if k!=plugin}!={k:v for k,v in before.items() if k!=plugin}:raise RuntimeError('OTHER_PLUGIN_CHANGED')
 if json.loads((config/'settings.json').read_text()).get('enabledPlugins',{})!=enabled:raise RuntimeError('PLUGIN_ENABLEMENT_CHANGED')
 installed=pathlib.Path(after[plugin][0]['installPath'])
 if json.loads((installed/'.claude-plugin/plugin.json').read_text())['version']!=version:raise RuntimeError('UPDATE_VERSION_MISMATCH')
 if not source_matches(installed,release/'plugin'):raise RuntimeError('UPDATE_SOURCE_MISMATCH')
 return {'changed':True,'version':version,'owner_state_preserved':True,'old_cache_preserved':True,'restart_for_new_sessions_only':True}
if __name__=='__main__':
 try:print(json.dumps(update(*sys.argv[1:])))
 except Exception as e:raise SystemExit(str(e) if isinstance(e,RuntimeError) else 'SAFE_UPDATE_FAILED_RAW_OUTPUT_SUPPRESSED')
