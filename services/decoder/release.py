#!/usr/bin/env python3
"""Download an explicitly approved GitHub release on Pi; validate before activation."""
import argparse
import os
from pathlib import Path
import sys
import tempfile
import shutil
import fcntl
ROOT=Path(__file__).resolve().parents[2]
sys.path[:0]=[str(ROOT/'experiments/laya/luna_decoder'),str(ROOT/'experiments/laya')]
from adaptive.common import read,atomic,digest
from adaptive.registry import Registry
from adaptive.delivery import sync

REPOSITORY='HikariTakahashi/uo-decoder-evaluation'

def credential(name,env):
    path=Path(os.environ['CREDENTIALS_DIRECTORY'])/name
    os.environ[env]=path.read_text().strip()


def activate(state,approval,image):
    state=Path(state);state.mkdir(parents=True,exist_ok=True)
    if set(approval)!={'repository','commit','index_sha256'} or approval['repository']!=REPOSITORY:raise ValueError('explicit production approval required')
    # Single publisher. Readers see a complete installed directory through an
    # atomic symlink, with approval stored inside that same snapshot.
    with (state/'release.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX)
        if (state/'current/active-production.json').exists():
            current=read(state/'current/active-production.json')
            if all(current.get(k)==v for k,v in approval.items()):
                registry=Registry(state/'current/installed',image)
                if registry.revision()!=current['registry_revision']:raise ValueError('active registry drift')
                return current['verification']
        with tempfile.TemporaryDirectory(dir=state,prefix='.release-') as tmp:
            stage=Path(tmp);registry=Registry(stage/'installed',image)
            receipt=sync(REPOSITORY,approval['commit'],approval['index_sha256'],registry)
            atomic(stage/'active-production.json',{**approval,'registry_revision':registry.revision(),'verification':receipt})
            dest=state/'releases'/(approval['commit']+'-'+approval['index_sha256'])
            dest.parent.mkdir(exist_ok=True)
            # Revalidation happens even for a retry; keep immutable prior release.
            if not dest.exists():shutil.copytree(stage,dest)
            else:
                for p in stage.rglob('*'):
                    if p.is_file() and p.name not in ('github-receipt.json','active-production.json') and 'receipts' not in p.parts and (not (dest/p.relative_to(stage)).is_file() or digest(p.read_bytes())!=digest((dest/p.relative_to(stage)).read_bytes())):raise ValueError('existing release differs')
            current=state/'current'
            previous=os.readlink(current) if current.is_symlink() else None
            if previous:atomic(state/'previous.json',{'path':previous})
            next_link=state/'.next'
            next_link.unlink(missing_ok=True);next_link.symlink_to(dest.resolve(),target_is_directory=True);os.replace(next_link,current)
            directory=os.open(state,os.O_RDONLY)
            try:os.fsync(directory)
            finally:os.close(directory)
            return receipt


def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--approval',type=Path,required=True);p.add_argument('--image',required=True);a=p.parse_args()
    if os.environ.get('CREDENTIALS_DIRECTORY'):credential('github-token','GH_TOKEN')
    activate(a.state,read(a.approval),a.image)

if __name__=='__main__':main()
