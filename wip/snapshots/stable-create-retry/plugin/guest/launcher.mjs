// One-shot exec gate, part of the plugin. No RPC, bridge, daemon or model loop.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
function runtimeGuard(c,dir,configHash){
 if(!c.prime_vault)return null;
 const ready=JSON.parse(fs.readFileSync(path.join(dir,'runtime-ready.json'),'utf8'));
 if(ready.phase!=='ready'||ready.config_hash!==configHash)throw Error('runtime ownership');
 if(ready.models_sha256&&crypto.createHash('sha256').update(fs.readFileSync(path.join(dir,'prime-config','models.json'))).digest('hex')!==ready.models_sha256)throw Error('native model catalog changed');
 for(const row of [...ready.processes,ready.controller]){const fields=fs.readFileSync('/proc/'+row.pid+'/stat','utf8').split(') ').slice(1).join(') ').trim().split(/\s+/);if(fields[0]==='Z'||fields[19]!==row.ticks)throw Error('runtime owner died')}
 const ss=fs.lstatSync(ready.socket);if(!ss.isSocket()||ss.uid!==process.getuid()||ss.dev!==ready.socket_dev||ss.ino!==ready.socket_ino)throw Error('runtime socket changed');
 return ready;
}

const dir=process.argv[2];
try {
 const permit=JSON.parse(fs.readFileSync(path.join(dir,'launch-permit.json'),'utf8'));
 if(!['create','initial-submit'].includes(permit.purpose)||!/^[-a-f0-9]{36}$/.test(permit.nonce))throw Error();
 fs.renameSync(path.join(dir,'launch-permit.json'),path.join(dir,`launch-used-${permit.nonce}.json`));
 const {config}=JSON.parse(fs.readFileSync(path.join(dir,'session.json'),'utf8'));
 let argv=config.agent_argv;
 if(!Array.isArray(argv)||!path.isAbsolute(argv[0])||typeof process.execve!=='function')throw Error();
 const env={...process.env};for(const k of ['LLM_API_KEY','BAO_TOKEN','VAULT_TOKEN'])delete env[k];
 const ready=runtimeGuard(config,dir,crypto.createHash('sha256').update(JSON.stringify(config)).digest('hex'));
 if(ready){
  const allowed=new Set(['PATH','HOME','PI_OFFLINE','DO_NOT_TRACK','PRIME_AGENT_CODING_AGENT_DIR','PRIME_AGENT_SESSION_DIR','PRIME_AGENT_KERNEL_PYTHON','PRIME_AGENT_DAEMON_SOCKET']);
  if(Object.keys(ready.env).some(k=>!allowed.has(k)))throw Error();
  Object.assign(env,ready.env);
  const selected=ready.coordinator??{provider:config.prime_vault.provider.name,model:config.prime_vault.provider.model};
  if(typeof selected.provider!=='string'||typeof selected.model!=='string')throw Error('coordinator identity');
  argv=[argv[0],'--mode','acp','--offline','--daemon-socket',ready.socket,'--provider',selected.provider,'--model',selected.model,'--cwd',config.workspace];
 }
 process.execve(argv[0],argv,env);
} catch { process.stderr.write('OWNER_RESTART_BLOCKED\n');process.exit(78); }
