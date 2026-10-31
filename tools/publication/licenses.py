#!/usr/bin/env python3
"""Collect exact installed dependency notices for a platform's `go list -deps -json` output."""
import argparse,hashlib,json,re,shutil
from pathlib import Path

def documents(directory):
    return sorted(p for p in directory.iterdir() if p.is_file() and re.match(r'^(license|licence|copying|notice|copyright)(?:[. _-].*)?$', p.name, re.I))

def decode(raw):
    decoder=json.JSONDecoder();pos=0
    while pos<len(raw):
        while pos<len(raw) and raw[pos].isspace():pos+=1
        if pos>=len(raw):return
        obj,pos=decoder.raw_decode(raw,pos);yield obj

def main():
    p=argparse.ArgumentParser();p.add_argument('input',type=Path);p.add_argument('output',type=Path);a=p.parse_args()
    a.output.mkdir(parents=True,exist_ok=False);packages=[];texts={};modules={};missing=[]
    for obj in decode(a.input.read_text()):
        mod=obj.get('Module',{})
        if not mod and not obj.get('Standard'):continue
        if mod.get('Main'):continue
        directory=Path(obj['Dir'])
        if obj.get('Standard'):
            # GoFiles and vendor code are covered by Go's distribution notices.
            if '/src/' not in str(directory):continue
            boundary=Path(str(directory).split('/src/',1)[0]);module='go';version=(boundary/'VERSION').read_text().splitlines()[0].removeprefix('go')
        else:
            boundary=Path(mod['Dir']);module=mod['Path'];version=mod['Version']
        module_key=module+'@'+version
        modules.setdefault(module_key,{'module':module,'version':version,'notice_files':set()})
        found=[]
        for ancestor in [directory,*directory.parents]:
            found.extend(documents(ancestor))
            if ancestor==boundary:break
        if not found:missing.append(obj['ImportPath'])
        for source in found:
            relative=str(source.relative_to(boundary));name='licenses/'+module_key+'/'+relative;data=source.read_bytes()
            dest=a.output/name;dest.parent.mkdir(parents=True,exist_ok=True);dest.write_bytes(data)
            texts[name]=hashlib.sha256(data).hexdigest();modules[module_key]['notice_files'].add(name)
        file_records=[]
        for filename in obj.get('GoFiles',[])+obj.get('CgoFiles',[]):
            source=directory/filename;data=source.read_bytes();head=data.decode(errors='replace').splitlines()[:30]
            spdx=next((s.split('SPDX-License-Identifier:',1)[1].strip() for s in head if 'SPDX-License-Identifier:' in s),None)
            file_records.append({'path':str(source.relative_to(boundary)),'sha256':hashlib.sha256(data).hexdigest(),'spdx_header':spdx})
        packages.append({'import_path':obj['ImportPath'],'module':module_key,'files':file_records})
    for obj in modules.values():obj['notice_files']=sorted(obj['notice_files'])
    result={'modules':list(modules.values()),'packages':packages,'notice_sha256':texts,'missing_notice_packages':missing,'scope':'compiled non-test dependencies; source headers recorded separately from enclosing license texts'}
    (a.output/'inventory.json').write_text(json.dumps(result,indent=2)+'\n')
    print(json.dumps({'modules':len(modules),'packages':len(packages),'license_texts':len(texts),'missing':missing}))
    if missing:raise SystemExit(1)
if __name__=='__main__':main()
