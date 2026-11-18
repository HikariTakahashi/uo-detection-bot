"""Explicitly authorized data-only publication to an isolated evaluation repo."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
ROOT=Path(__file__).resolve().parents[1]
sys.path[:0]=[str(ROOT),str(ROOT.parent)]
from adaptive.common import atomic,digest,read
from adaptive.delivery import verify_repository,sync,gh,update_bundles,package
from adaptive.registry import Registry


def git(cwd,*args):
    return subprocess.run(['git','-C',str(cwd),*args],check=True,capture_output=True,text=True,timeout=60).stdout.strip()


def wait_and_sync(repo,commit,index_sha,registry,seconds=300):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        try:
            release=json.loads(gh('release','view','evaluation-'+commit,'--repo',repo,'--json','isDraft'))
        except subprocess.CalledProcessError:
            release=None
        if release and not release['isDraft']:return sync(repo,commit,index_sha,registry)
        print('Waiting for evaluation CI/release at',commit,flush=True)
        time.sleep(10)
    raise TimeoutError('release not ready; rerun receive with the same pinned commit, never republish')


def publish(run,checkout,repo):
    run,checkout=Path(run),Path(checkout)
    # Never publish to the production repository, even if a caller mistypes the destination.
    if repo!='HikariTakahashi/uo-decoder-evaluation':raise ValueError('dedicated evaluation repository required')
    remote=git(checkout,'remote','get-url','origin')
    if remote not in (f'https://github.com/{repo}',f'https://github.com/{repo}.git',f'git@github.com:{repo}.git'):raise ValueError('remote mismatch')
    if git(checkout,'status','--porcelain') or git(checkout,'branch','--show-current')!='main':raise ValueError('clean dedicated main checkout required')
    git(checkout,'fetch','origin','main')
    if git(checkout,'rev-parse','HEAD')!=git(checkout,'rev-parse','origin/main'):raise ValueError('checkout must match remote main')
    # Runtime must already have been reviewed/provisioned. This command cannot update it.
    for path in [*ROOT.glob('*.py'),*ROOT.joinpath('adaptive').glob('*.py'),ROOT.parent/'classify.py',ROOT.parent/'prompts.json',ROOT.parent/'abstention_policy.py']:
        target=checkout/'experiments/laya'/path.relative_to(ROOT.parent)
        if not target.is_file() or digest(target.read_bytes())!=digest(path.read_bytes()):raise ValueError('trusted runtime differs; reviewed runtime update required')
    image=read(run/'plan.json')['image'];registry=Registry(run/'installed',image)
    update_bundles(run,checkout)
    verify_repository(checkout,image)
    git(checkout,'add','--','index.json','registry/bundles','contracts')
    names=git(checkout,'diff','--cached','--name-only').splitlines()
    if any(n!='index.json' and not n.startswith('registry/bundles/') and not n.startswith('contracts/') for n in names):raise ValueError('unexpected staged path')
    if names:
        git(checkout,'commit','-m','Publish validated synthetic decoder bundles')
        commit=git(checkout,'rev-parse','HEAD')
        atomic(run/'publication.json',{'repository':repo,'commit':commit,'state':'push_pending'})
        git(checkout,'push','origin','HEAD:main')
    else:commit=git(checkout,'rev-parse','HEAD')
    with tempfile.TemporaryDirectory() as tmp:
        package(checkout,tmp,commit)
        index_sha=digest((Path(tmp)/'index.json').read_bytes())
    atomic(run/'publication.json',{'repository':repo,'commit':commit,'index_sha256':index_sha,'state':'waiting_release'})
    receipt=wait_and_sync(repo,commit,index_sha,registry)
    atomic(run/'publication.json',{'repository':repo,'commit':commit,'index_sha256':index_sha,'state':'complete','receipt':receipt})
    return receipt


def main():
    p=argparse.ArgumentParser();p.add_argument('--run-dir',required=True,type=Path);p.add_argument('--checkout',required=True,type=Path);p.add_argument('--repository',default='HikariTakahashi/uo-decoder-evaluation');p.add_argument('--publish-validated',required=True,action='store_true');a=p.parse_args()
    print(json.dumps(publish(a.run_dir,a.checkout,a.repository),ensure_ascii=False,indent=2))


if __name__=='__main__':main()
