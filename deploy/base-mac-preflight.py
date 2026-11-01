#!/usr/bin/env python3
"""Read-only native Mac builder guard. No memory percentage is a reservation."""
import argparse,json,pathlib,platform,re,shutil,subprocess,sys
p=argparse.ArgumentParser();p.add_argument('--path',required=True);p.add_argument('--disk-gib',type=int,default=8);p.add_argument('--memory-mib',type=int,default=2048);p.add_argument('--reserve-gib',type=int,default=20);a=p.parse_args()
assert platform.system()=='Darwin'and platform.machine()=='arm64','Native ARM64 Mac required'
assert 8<=a.disk_gib<=100 and 1024<=a.memory_mib<=4096 and a.reserve_gib>=20
path=pathlib.Path(a.path);assert path.is_absolute()and path.is_dir()and not path.is_symlink()
free=shutil.disk_usage(path).free;required=(6*a.disk_gib+a.reserve_gib)*2**30
physical=int(subprocess.check_output(['/usr/sbin/sysctl','-n','hw.memsize'],text=True,timeout=10));pressure=subprocess.check_output(['/usr/bin/memory_pressure','-Q'],text=True,timeout=20);m=re.search(r'System-wide memory free percentage:\s*(\d+)%',pressure);assert m,'Memory pressure metadata unavailable'
percentage=int(m[1]);assert percentage>=25+100*a.memory_mib*2**20/physical,'Insufficient memory-pressure headroom'
assert free>=required,'Insufficient disk budget including full-size builder/export/two retained clones/cache/temp and host reserve'
print(json.dumps({'native_arm64':True,'free_disk_bytes':free,'required_disk_bytes':required,'disk_slots':6,'host_reserve_gib':a.reserve_gib,'requested_memory_mib':a.memory_mib,'memory_free_percentage':percentage,'physical_memory_bytes':physical,'advisory_not_reservation':True,'live_nomad_workload_guard_still_required':True,'mutations':0}))
