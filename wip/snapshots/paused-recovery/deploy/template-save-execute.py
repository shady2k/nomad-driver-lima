#!/usr/bin/env python3
"""One-shot existing operator entrypoint; no source-caller identity impersonation.
Caller creates the immutable plan; existing controller transfers that plan through
its authenticated SSH, then executes this entrypoint after exact confirmation.
No daemon, credentials in plan, new dispatch endpoint or implicit host grant.
"""
import argparse,json,pathlib,re,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save
p=argparse.ArgumentParser();p.add_argument('--policy',required=True);p.add_argument('--plan',required=True);p.add_argument('--confirmation',required=True);p.add_argument('--journal',required=True)
if __name__=='__main__':
 try:
  a=p.parse_args();packet=exchange.protected(a.plan);approval=exchange.protected(a.confirmation)
  print(json.dumps(template_save.execute(packet,approval,pathlib.Path(a.journal),template_save.OperatorPipeline(a.policy))))
 except Exception as e:
  print(json.dumps({'error':str(e)if re.fullmatch('[A-Z_]{1,100}',str(e))else type(e).__name__}),file=sys.stderr);sys.exit(1)
