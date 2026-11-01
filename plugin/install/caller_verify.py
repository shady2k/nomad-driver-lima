"""Read only native registry + templates/capacity as the caller, no VM/model."""
import json,pathlib,subprocess,sys,hashlib
home=pathlib.Path(sys.argv[1]);claude=sys.argv[2]
settings=json.loads((home/'.claude/settings.json').read_text())
assert settings.get('enabledPlugins',{}).get('nomad-workers@nomad-workers-local') is True
registry=json.loads((home/'.claude/plugins/installed_plugins.json').read_text())['plugins']
installed=registry['nomad-workers@nomad-workers-local'];assert len(installed)==1
row=installed[0];root=pathlib.Path(row['installPath']);manifest=json.loads((root/'.claude-plugin/plugin.json').read_text())
assert manifest['name']=='nomad-workers'
assert (root/'hooks/hooks.json').is_file()
source=pathlib.Path(sys.argv[3])/'plugin'
assert manifest['version']==json.loads((source/'.claude-plugin/plugin.json').read_text())['version']
for name in ['scripts/file_transfer.py','scripts/upload_guest.py','scripts/exec_checkpoint.py','scripts/vm_interface.py','scripts/workerctl.py','scripts/catalog_cli.py','scripts/nomad_cli.py','scripts/guest_exec.py','scripts/capacity_view.py','scripts/operations.py','scripts/operation_guest.py','scripts/operation_policy.py','scripts/session_hook.py','scripts/template_draft.py','scripts/activity.py','scripts/placement.py','scripts/model_catalog.py','skills/nomad-workers/SKILL.md','hooks/hooks.json']:
 assert hashlib.sha256((root/name).read_bytes()).digest()==hashlib.sha256((source/name).read_bytes()).digest(),'Installed source differs from frozen release'
config=json.loads((home/'.config/nomad-workers/catalog-client.json').read_text());catalog=pathlib.Path(config['catalog_file']).read_bytes()
assert hashlib.sha256(catalog).hexdigest()==config['catalog_sha256']
sys.path.insert(0,str(root/'scripts'))
from operation_policy import load as load_operation_policy
policy=load_operation_policy(home/'.config/nomad-workers/operations.json')
for command in ['operation-start','operation-status','operation-logs','operation-wait','operation-cancel','prepare-template','template-draft','model-select']:
 check=subprocess.run([str(home/'.local/bin/nomad-workers'),command,'--help'],capture_output=True,text=True,timeout=10)
 assert check.returncode==0,'Operation help unavailable'
results={}
for command in ['templates','capacity']:
 p=subprocess.run([str(home/'.local/bin/nomad-workers'),command],capture_output=True,text=True,timeout=25)
 if p.returncode:raise SystemExit('Caller '+command+' failed; raw output suppressed')
 value=json.loads(p.stdout)
 if command=='templates':results[command]=[{k:e[k] for k in ['template_id','version','architecture'] if k in e} for e in value]
 else:results[command]={'eligible_nodes':[{'name':n.get('name'),'architecture':n.get('architecture'),'free_cpu_mhz':n.get('free_cpu_mhz'),'free_reservation_mib':n.get('free_reservation_mib')} for n in value['nodes']],'advisory':True}
# This verifies the actual native CLI view, not just our hand-written files.
p=subprocess.run([claude,'plugin','list','--json'],capture_output=True,text=True,timeout=15)
assert p.returncode==0,'Native plugin list failed'
assert 'nomad-workers@nomad-workers-local' in p.stdout or '"nomad-workers"' in p.stdout
legacy_plugin_id=config.get('legacy_plugin_id')
print(json.dumps({'installed_plugin':manifest,'scope':row['scope'],'native_plugin_list_verified':True,'old_plugin_preserved':(legacy_plugin_id in registry)if legacy_plugin_id else None,'old_plugin_setting_unchanged':settings.get('enabledPlugins',{}).get(legacy_plugin_id)if legacy_plugin_id else None,'guest_model_override':config['guest'].get('model'),'credentials_copied':False,'new_vms':0,'model_calls':0,'operation_policy':policy,'managed_operation_help_verified':True,'read_only_results':results}))
