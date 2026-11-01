#!/usr/bin/env python3
# Test transport only. It never contacts Nomad and only operates its own fixture container.
import sys,os,subprocess,json
args=sys.argv[1:];container=os.environ['FIXTURE_CONTAINER'];alloc=os.environ['FIXTURE_ALLOCATION'];job=os.environ['FIXTURE_JOB'];owner=os.environ['FIXTURE_OWNER']
def running():return subprocess.check_output(['docker','inspect','--format','{{.State.Running}}',container],text=True).strip()=='true'
if args[:2]==['alloc','exec']:
 i=args.index(alloc);os.execvp('docker',['docker','exec','-i',container,*args[i+1:]])
elif args[:2]==['alloc','status']:
 assert args[-1]==alloc;r=running();print(json.dumps({'ID':alloc,'Namespace':'default','ClientStatus':'running' if r else 'complete','DesiredStatus':'run' if r else 'stop','TaskStates':{'worker':{'State':'running' if r else 'dead','StartedAt':'2026-01-01T00:00:00Z','Restarts':0}},'Job':{'ID':job,'Meta':{'nomad_workers_owner':owner},'TaskGroups':[{'Tasks':[{'Name':'worker','Driver':'lima'}]}]}}))
elif args[:2]==['job','allocs']:print(json.dumps([{'ID':alloc,'ClientStatus':'running' if running() else 'complete'}]))
elif args[:2]==['job','stop']:
 assert args[-1]==job and '-purge' not in args
 with open(os.environ['FIXTURE_STOP_TRACE'],'a') as f:f.write('stop\n')
 subprocess.run(['docker','stop','-t','1',container],check=True,stdout=subprocess.DEVNULL)
else:raise SystemExit('unexpected mock transport command')
