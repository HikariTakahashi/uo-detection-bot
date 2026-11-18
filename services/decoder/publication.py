#!/usr/bin/env python3
"""Prepare a data-only Pi checkout; publish only an exact, explicitly approved diff."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
ROOT=Path(__file__).resolve().parents[2]
sys.path[:0]=[str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'experiments/laya')]
from adaptive.common import atomic,read,digest
from adaptive.delivery import verify_repository,package
from adaptive.publish import git

REPO='HikariTakahashi/uo-decoder-evaluation'

def paths(checkout):
    raw=subprocess.run(['git','-C',str(checkout),'status','--porcelain','--untracked-files=all'],check=True,capture_output=True,text=True,timeout=30).stdout
    names=raw.splitlines()
    result={}
    for line in names:
        name=line[3:]
        if name!='index.json' and not name.startswith(('contracts/','registry/bundles/')):raise ValueError('non-data publication')
        path=Path(checkout)/name
        if not path.is_file() or path.is_symlink():raise ValueError('deletions/symlinks not allowed')
        result[name]=digest(path.read_bytes())
    return result


def prepare(state,checkout,family,image):
    state,checkout=Path(state),Path(checkout)
    if git(checkout,'remote','get-url','origin') not in (f'https://github.com/{REPO}',f'https://github.com/{REPO}.git',f'git@github.com:{REPO}.git'):raise ValueError('dedicated repo only')
    if git(checkout,'status','--porcelain') or git(checkout,'branch','--show-current')!='main':raise ValueError('clean main required')
    git(checkout,'fetch','origin','main')
    if git(checkout,'rev-parse','HEAD')!=git(checkout,'rev-parse','origin/main'):raise ValueError('checkout behind remote')
    job=read(state/'jobs'/(family+'.json'))
    if job['state']!='awaiting_publication_approval':raise ValueError('unvalidated proposal')
    proposal=state/'proposals'/family
    sha=job['bundle'];contract_sha=job['contract_sha256']
    if digest((proposal/'contract.json').read_bytes())!=contract_sha:raise ValueError('contract changed')
    dest=checkout/'registry/bundles'/sha
    if not dest.exists():shutil.copytree(proposal/'registry/bundles'/sha,dest)
    (checkout/'contracts').mkdir(exist_ok=True)
    shutil.copyfile(proposal/'contract.json',checkout/'contracts'/(contract_sha+'.json'))
    index=read(checkout/'index.json')
    index['bundles']=[r for r in index['bundles'] if r['family']!=family]+[{'family':family,'sha256':sha,'contract_sha256':contract_sha}]
    index['bundles'].sort(key=lambda r:r['family']);atomic(checkout/'index.json',index)
    verify_repository(checkout,image)
    plan={'repository':REPO,'base_commit':git(checkout,'rev-parse','HEAD'),'files':paths(checkout),'title':'Add validated decoder '+family}
    atomic(state/'publication-plan.json',plan)
    return plan


def publish(state,checkout,approval):
    state,checkout=Path(state),Path(checkout)
    plan=read(state/'publication-plan.json')
    if approval!=plan:raise ValueError('exact title/base/files approval required')
    if git(checkout,'rev-parse','HEAD')!=plan['base_commit'] or paths(checkout)!=plan['files']:raise ValueError('approved diff changed')
    git(checkout,'add','--',*sorted(plan['files']))
    git(checkout,'commit','-m',plan['title'])
    commit=git(checkout,'rev-parse','HEAD')
    # Record BEFORE push. If interrupted here, resume the same commit using git;
    # do not call publish again or create another commit.
    with tempfile.TemporaryDirectory() as tmp:
        package(checkout,tmp,commit);index_sha=digest((Path(tmp)/'index.json').read_bytes())
    receipt={'repository':REPO,'commit':commit,'index_sha256':index_sha}
    atomic(state/'publication-pending.json',receipt)
    git(checkout,'push','origin','HEAD:main')
    atomic(state/'publication-pushed.json',receipt)
    return receipt


def main():
    p=argparse.ArgumentParser();p.add_argument('command',choices=['prepare','publish']);p.add_argument('--state',type=Path,required=True);p.add_argument('--checkout',type=Path,required=True);p.add_argument('--family');p.add_argument('--image');p.add_argument('--approval',type=Path);a=p.parse_args()
    if os.environ.get('CREDENTIALS_DIRECTORY'):os.environ['GH_TOKEN']=(Path(os.environ['CREDENTIALS_DIRECTORY'])/'github-token').read_text().strip()
    if a.command=='prepare':result=prepare(a.state,a.checkout,a.family,a.image)
    else:result=publish(a.state,a.checkout,read(a.approval))
    print(json.dumps(result,ensure_ascii=False,indent=2))
if __name__=='__main__':main()
