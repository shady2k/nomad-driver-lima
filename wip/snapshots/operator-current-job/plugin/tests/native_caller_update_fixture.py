import pathlib,tempfile,json,subprocess,os,sys,base64
root=pathlib.Path(tempfile.mkdtemp(prefix='nomad-safe-update-'));home=root/'home';home.mkdir();config=home/'.claude';env={**os.environ,'HOME':str(home),'CLAUDE_CONFIG_DIR':str(config),'DISABLE_AUTOUPDATER':'1','CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC':'1'};os.environ.update(env);cli=sys.argv[1]
for n,v in [('old','0.1.5'),('new','0.1.6')]:
 p=root/n;(p/'.claude-plugin').mkdir(parents=True);(p/'plugin/.claude-plugin').mkdir(parents=True)
 (p/'plugin/.claude-plugin/plugin.json').write_text(json.dumps({'name':'nomad-workers','version':v,'description':'synthetic only'}));(p/'.claude-plugin/marketplace.json').write_text(json.dumps({'name':'nomad-workers-local','owner':{'name':'Fixture'},'plugins':[{'name':'nomad-workers','source':'./plugin','version':v}]}))
p=subprocess.run([cli,'plugin','install','nomad-workers','--marketplace',str(root/'old'),'--scope','user','--json'],env=env,capture_output=True,text=True,timeout=30);assert p.returncode==0
for p in [home/'.local/state/nomad-workers/initiators',config/'plugins/data/nomad-workers-nomad-workers-local']:
 p.mkdir(parents=True);(p/'owner.json').write_text('synthetic-exact-generation');(p/'owner.lock').write_text('')
ns={'__name__':'fixture'};exec((pathlib.Path(__file__).parents[1]/'install/caller_safe_update.py').read_text(),ns)
a=ns['update'](str(home),cli,str(root/'new'),'0.1.6');b=ns['update'](str(home),cli,str(root/'new'),'0.1.6');assert a['changed'] and not b['changed'];print(json.dumps({'native_version':subprocess.check_output([cli,'--version'],text=True).strip(),'first':a,'second':b,'fixture_root':str(root),'live_config_used':False}))
