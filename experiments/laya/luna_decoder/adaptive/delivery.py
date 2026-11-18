"""Dedicated-repository CI and explicitly pinned GitHub -> evaluation CD."""
import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import io
import zipfile
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT),str(ROOT.parent)]
from adaptive.common import atomic,canonical,digest,read
from adaptive.catalog import FAMILIES,validation_cases
from adaptive.registry import Registry
from adaptive.discovery import validate_selector
from sandbox import evaluate


def entries_of(index):
    if index.get('scope')!='synthetic-evaluation-only' or index.get('schema') not in (1,2):
        raise ValueError('invalid delivery index')
    entries=index['bundles']
    if not entries or len(entries)>64 or len({r['family'] for r in entries})!=len(entries):
        raise ValueError('invalid bundle set')
    fields={'family','sha256'} | ({'contract_sha256'} if index['schema']==2 else set())
    for item in entries:
        if set(item)!=fields or not re.fullmatch(r'[A-Za-z0-9_-]{1,80}',item['family']):
            raise ValueError('invalid bundle entry')
        for key in fields-{'family'}:
            if not re.fullmatch('[a-f0-9]{64}',item[key]):raise ValueError('invalid digest')
    return entries


def contract_for(repo,index,item,manifest):
    family=item['family']
    if index['schema']==1:
        if family not in FAMILIES or 'selector' in manifest:raise ValueError('unknown legacy contract')
        cases=validation_cases(family)
    else:
        path=Path(repo)/'contracts'/(item['contract_sha256']+'.json')
        if path.is_symlink() or path.stat().st_size>131072 or digest(path.read_bytes())!=item['contract_sha256']:
            raise ValueError('contract hash mismatch')
        contract=read(path)
        if set(contract)!={'schema','scope','family','selector','cases'} or contract['schema']!=1 or contract['scope']!='synthetic-evaluation-only' or contract['family']!=family:
            raise ValueError('invalid contract scope/family')
        selector=contract['selector']
        if selector is not None:validate_selector(selector)
        if selector!=manifest.get('selector'):raise ValueError('contract selector mismatch')
        cases=contract['cases']
        if not isinstance(cases,list) or not 2<=len(cases)<=64:raise ValueError('invalid contract size')
        if any(not isinstance(c,dict) or not isinstance(c.get('text'),str) or len(c['text'])>2000 or 'expected' not in c or (c['expected'] is not None and (not isinstance(c['expected'],str) or not 1<=len(c['expected'])<=2000)) for c in cases):
            raise ValueError('invalid contract cases')
        if not any(c['expected'] is None for c in cases) or not any(c['expected'] is not None for c in cases):
            raise ValueError('positive and rejection cases required')
    if manifest.get('scope')!='synthetic-evaluation-only' or manifest.get('schema')!=1 or manifest['family']!=family or manifest['prefix']!=family+':':
        raise ValueError('invalid decoder scope')
    return cases


def verify_repository(repo,image):
    repo=Path(repo);index=read(repo/'index.json');entries=entries_of(index)
    registry=Registry(repo/'registry',image);checks=[]
    for item in entries:
        manifest,code=registry.load(item['sha256'])
        cases=contract_for(repo,index,item,manifest)
        result=evaluate(code,[r['text'] for r in cases],image)
        if len(result['values'])!=len(cases) or any(v['error'] or v['value']!=c['expected'] for c,v in zip(cases,result['values'])):
            raise ValueError('CI oracle mismatch')
        checks.append({'family':item['family'],'bundle':item['sha256'],'tests':len(cases),'passed':True,'image':image})
    return checks


def runtime_files(repo):
    repo=Path(repo)
    files=[*repo.joinpath('experiments/laya').rglob('*.py'),*repo.joinpath('experiments/laya').rglob('*.json'),
           *repo.joinpath('experiments/laya').rglob('*.md'),*repo.joinpath('experiments/laya/luna_decoder/adaptive').glob('evaluation-workflow.yml'),repo/'README.md']
    # Exclude operational data even if a local checkout has been used for a run.
    return sorted(p for p in files if p.is_file() and not any(x in ('runs','__pycache__','.venv') for x in p.relative_to(repo).parts))


def runtime_bytes(repo):
    repo=Path(repo);buf=io.BytesIO()
    with zipfile.ZipFile(buf,'w',compression=zipfile.ZIP_STORED) as archive:
        for p in runtime_files(repo):
            if p.is_symlink():raise ValueError('runtime symlink')
            name=p.relative_to(repo).as_posix();info=zipfile.ZipInfo(name,(2020,1,1,0,0,0))
            info.external_attr=0o100644<<16
            archive.writestr(info,p.read_bytes())
    return buf.getvalue()


def package(repo,out,commit):
    repo,out=Path(repo),Path(out)
    if not re.fullmatch('[a-f0-9]{40}',commit):raise ValueError('exact commit SHA required')
    out.mkdir(parents=True,exist_ok=True)
    index=read(repo/'index.json');entries=entries_of(index);index={**index,'commit':commit}
    registry=Registry(repo/'registry','unused')
    for item in entries:
        sha=item['sha256'];manifest,_=registry.load(sha);contract_for(repo,index,item,manifest)
        for name in ('manifest.json','candidate.py','verification.json'):
            shutil.copyfile(repo/'registry/bundles'/sha/name,out/(sha+'-'+name))
        if index['schema']==2:
            name=item['contract_sha256']+'.json'
            shutil.copyfile(repo/'contracts'/name,out/('contract-'+name))
    if index['schema']==2:
        data=runtime_bytes(repo);(out/'runtime.zip').write_bytes(data);index['runtime_sha256']=digest(data)
    atomic(out/'index.json',index)
    return index


def gh(*args):
    return subprocess.run(['gh',*args],check=True,capture_output=True,timeout=60).stdout


def sync(repo,commit,index_sha,installed):
    if not re.fullmatch('[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+',repo) or not re.fullmatch('[a-f0-9]{40}',commit) or not re.fullmatch('[a-f0-9]{64}',index_sha):raise ValueError('explicit repository, commit and index SHA required')
    tag='evaluation-'+commit
    release=json.loads(gh('release','view',tag,'--repo',repo,'--json','isDraft,isPrerelease,targetCommitish,tagName'))
    if release['isDraft'] or release['tagName']!=tag:raise ValueError('release not ready')
    ref=json.loads(gh('api',f'repos/{repo}/git/ref/tags/{tag}'))
    if ref['object']['type']!='commit' or ref['object']['sha']!=commit:raise ValueError('release tag does not identify pinned commit')
    with tempfile.TemporaryDirectory(prefix='decoder-delivery-') as tmp:
        folder=Path(tmp)
        gh('release','download',tag,'--repo',repo,'--pattern','index.json','--dir',str(folder))
        if digest((folder/'index.json').read_bytes())!=index_sha:raise ValueError('index hash mismatch')
        index=read(folder/'index.json')
        if index.get('scope')!='synthetic-evaluation-only' or index.get('commit')!=commit:raise ValueError('release scope/commit mismatch')
        entries=entries_of(index)
        # Validate ALL artifacts in a temporary registry before changing installed state.
        candidate_registry=Registry(folder/'trial',installed.image)
        for entry in entries:
            sha=entry['sha256'];family=entry['family']
            if index['schema']==2:
                asset='contract-'+entry['contract_sha256']+'.json'
                gh('release','download',tag,'--repo',repo,'--pattern',asset,'--dir',str(folder))
                (folder/'contracts').mkdir(exist_ok=True)
                shutil.move(folder/asset,folder/'contracts'/(entry['contract_sha256']+'.json'))
            bundle=folder/sha;bundle.mkdir()
            for name in ('manifest.json','candidate.py','verification.json'):
                asset=sha+'-'+name
                gh('release','download',tag,'--repo',repo,'--pattern',asset,'--dir',str(folder))
                shutil.move(folder/asset,bundle/name)
            cases=contract_for(folder,index,entry,read(bundle/'manifest.json'))
            candidate_registry.install(bundle,sha,cases)
            if candidate_registry.load(sha)[0]['family']!=family:raise ValueError('index family mismatch')
        previous=read(installed.pointer) if installed.pointer.exists() else {}
        new=read(candidate_registry.pointer)
        for sha in new.values():
            dest=installed.path/'bundles'/sha;dest.parent.mkdir(exist_ok=True)
            if not dest.exists():shutil.copytree(candidate_registry.path/'bundles'/sha,dest)
            installed.load(sha)
        atomic(installed.path/'previous.json',previous)
        atomic(installed.pointer,{**previous,**new})
        receipt={'repository':repo,'commit':commit,'tag':tag,'index_sha256':index_sha,'bundles':new,'production_connected':False}
        atomic(installed.path/'github-receipt.json',receipt)
        return receipt


def update_bundles(run,dest):
    """Copy validated bundles and evaluator-owned contracts; preserve old families."""
    run,dest=Path(run),Path(dest);installed=Registry(run/'installed',read(run/'plan.json')['image'])
    old=read(dest/'index.json') if (dest/'index.json').exists() else None
    byfamily={}
    if old:
        for item in entries_of(old):
            if old['schema']==1:
                family=item['family']
                contract={'schema':1,'scope':'synthetic-evaluation-only','family':family,'selector':None,'cases':validation_cases(family)}
                path=dest/'contracts'/'pending.json';atomic(path,contract);sha=digest(path.read_bytes());path.rename(path.with_name(sha+'.json'))
                item={**item,'contract_sha256':sha}
            byfamily[item['family']]=item
    independent=read(run/'validation.json') if (run/'validation.json').exists() else {}
    for r in installed.active():
        family=r['family'];sha=r['id'];m,_=installed.load(sha)
        if family in independent:
            c=independent[family]
            if c['selector']!=m.get('selector'):raise ValueError('run contract selector mismatch')
            cases=c['cases']
        elif family in FAMILIES:cases=validation_cases(family)
        elif 'selector' not in m:
            # Previously authenticated seeded prefix bundles carry their frozen
            # independent evidence; it is now reviewed and pinned as a contract.
            cases=read(installed.path/'bundles'/sha/'verification.json')['cases']
        else:raise ValueError('missing independent run contract')
        contract={'schema':1,'scope':'synthetic-evaluation-only','family':family,'selector':m.get('selector'),'cases':cases}
        path=dest/'contracts'/'pending.json';atomic(path,contract);ch=digest(path.read_bytes());path.rename(path.with_name(ch+'.json'))
        item={'family':family,'sha256':sha,'contract_sha256':ch}
        contract_for(dest,{'schema':2},item,m)
        target=dest/'registry/bundles'/sha
        if not target.exists():shutil.copytree(installed.path/'bundles'/sha,target)
        byfamily[family]=item
    index={'schema':2,'scope':'synthetic-evaluation-only','bundles':sorted(byfamily.values(),key=lambda x:x['family'])}
    entries_of(index);atomic(dest/'index.json',index)
    return index


def export(run,dest):
    run,dest=Path(run),Path(dest)
    if dest.exists() and any(dest.iterdir()):raise ValueError('export destination must be empty')
    dest.mkdir(parents=True,exist_ok=True)
    index=update_bundles(run,dest)
    paths=[*ROOT.glob('*.py'),*ROOT.joinpath('adaptive').glob('*.py'),ROOT.parent/'classify.py',ROOT.parent/'prompts.json',ROOT.parent/'abstention_policy.py']
    paths += [ROOT.parent/name for name in ('test_adaptive_decoder.py','test_discovery_decoder.py','test_unseen_decoder_eval.py','test_decoder_delivery.py')]
    paths += [ROOT/name for name in ('DISCOVERY_IMPLEMENTATION.md','DISCOVERY_RESULTS.md')]
    paths += [ROOT/'adaptive/README.md',ROOT/'adaptive/evaluation-workflow.yml']
    for path in paths:
        target=dest/'experiments/laya'/path.relative_to(ROOT.parent);target.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(path,target)
    (dest/'.github/workflows').mkdir(parents=True)
    shutil.copyfile(ROOT/'adaptive/evaluation-workflow.yml',dest/'.github/workflows/evaluation.yml')
    shutil.copyfile(ROOT/'adaptive/README.md',dest/'README.md')
    (dest/'.gitignore').write_text('__pycache__/\n*.pyc\n/runs/\n/dist/\n/.venv/\n')
    return {'directory':str(dest),'bundles':index['bundles'],'scope':'no bot binary, secrets or production configuration'}


def extract_runtime(data,destination):
    destination=Path(destination)
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        names=set();total=0
        for member in archive.infolist():
            name=member.filename;path=Path(name)
            if path.is_absolute() or '..' in path.parts or name in names or (member.external_attr>>16)&0o170000 not in (0,0o100000):
                raise ValueError('invalid runtime member')
            if name not in ('README.md','experiments/laya/luna_decoder/adaptive/evaluation-workflow.yml') and (not name.startswith('experiments/laya/') or path.suffix not in ('.py','.json','.md')):
                raise ValueError('unexpected runtime path')
            names.add(name);total+=member.file_size
            if total>10_000_000 or len(names)>256:raise ValueError('runtime archive limit')
            target=destination/path;target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(archive.read(member))
        if 'experiments/laya/luna_decoder/adaptive_service.py' not in names:raise ValueError('missing runtime entry point')


def deploy(repo,commit,index_sha,destination,image):
    """Activate runtime AND registry together only after pinned CD succeeds."""
    if repo!='HikariTakahashi/uo-decoder-evaluation':raise ValueError('dedicated evaluation repository required')
    if not re.fullmatch('[a-f0-9]{40}',commit) or not re.fullmatch('[a-f0-9]{64}',index_sha):raise ValueError('exact commit/index required')
    destination=Path(destination);destination.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.trial-',dir=destination) as tmp:
        trial=Path(tmp);registry=Registry(trial/'installed',image)
        receipt=sync(repo,commit,index_sha,registry)
        gh('release','download','evaluation-'+commit,'--repo',repo,'--pattern','index.json','--dir',str(trial))
        if digest((trial/'index.json').read_bytes())!=index_sha:raise ValueError('index hash mismatch')
        index=read(trial/'index.json')
        if index.get('schema')!=2 or not re.fullmatch('[a-f0-9]{64}',index.get('runtime_sha256','')):raise ValueError('runtime release required')
        gh('release','download','evaluation-'+commit,'--repo',repo,'--pattern','runtime.zip','--dir',str(trial))
        data=(trial/'runtime.zip').read_bytes()
        if digest(data)!=index['runtime_sha256']:raise ValueError('runtime hash mismatch')
        extract_runtime(data,trial/'runtime')
        atomic(trial/'receipt.json',{**receipt,'runtime_sha256':index['runtime_sha256']})
        release=destination/'releases'/(commit+'-'+index_sha);release.parent.mkdir(exist_ok=True)
        if release.exists():
            # Verify immutable existing contents rather than trusting a previous path.
            expected_names={p.relative_to(trial/'runtime') for p in (trial/'runtime').rglob('*') if p.is_file()}
            actual_names={p.relative_to(release/'runtime') for p in (release/'runtime').rglob('*') if p.is_file() and '__pycache__' not in p.parts}
            if actual_names!=expected_names:raise ValueError('existing runtime file set drift')
            for src in trial.rglob('*'):
                if src.is_file() and src.relative_to(trial).parts[0] in ('runtime','index.json','runtime.zip'):
                    old=release/src.relative_to(trial)
                    if not old.is_file() or old.is_symlink() or digest(old.read_bytes())!=digest(src.read_bytes()):raise ValueError('existing deployment drift')
            if read(release/'installed/active.json')!=read(registry.pointer):raise ValueError('existing registry drift')
            existing=Registry(release/'installed',image)
            for r in registry.active():existing.load(r['id'])
        else:shutil.copytree(trial,release)
        pointer={'commit':commit,'index_sha256':index_sha,'path':str(release.resolve()),'production_connected':False}
        current=destination/'current.json'
        if current.exists() and read(current)!=pointer:atomic(destination/'previous.json',read(current))
        atomic(current,pointer)
        return {**pointer,'runtime_sha256':index['runtime_sha256'],'bundles':receipt['bundles']}


def main():
    p=argparse.ArgumentParser();sub=p.add_subparsers(dest='command',required=True)
    v=sub.add_parser('verify');v.add_argument('--repo-dir',required=True,type=Path);v.add_argument('--image',required=True)
    x=sub.add_parser('export');x.add_argument('--run-dir',required=True,type=Path);x.add_argument('--destination',required=True,type=Path)
    b=sub.add_parser('package');b.add_argument('--repo-dir',required=True,type=Path);b.add_argument('--output',required=True,type=Path);b.add_argument('--commit',required=True)
    s=sub.add_parser('sync');s.add_argument('--repository',required=True);s.add_argument('--commit',required=True);s.add_argument('--index-sha256',required=True);s.add_argument('--installed',required=True,type=Path);s.add_argument('--image',required=True)
    d=sub.add_parser('deploy');d.add_argument('--repository',required=True);d.add_argument('--commit',required=True);d.add_argument('--index-sha256',required=True);d.add_argument('--destination',required=True,type=Path);d.add_argument('--image',required=True)
    a=p.parse_args()
    if a.command=='verify':result=verify_repository(a.repo_dir,a.image)
    elif a.command=='export':result=export(a.run_dir,a.destination)
    elif a.command=='package':package(a.repo_dir,a.output,a.commit);result={'packaged':True}
    elif a.command=='deploy':result=deploy(a.repository,a.commit,a.index_sha256,a.destination,a.image)
    else:result=sync(a.repository,a.commit,a.index_sha256,Registry(a.installed,a.image))
    print(json.dumps(result,ensure_ascii=False,indent=2))


if __name__=='__main__':main()
