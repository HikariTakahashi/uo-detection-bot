"""Pi-only, policy-gated publication of independently validated data bundles.

No production activation here. Exact-commit CI and Discord DM adoption remain
separate. Persist the diff before committing; recover the same commit after a crash.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import subprocess
import tempfile
import publication
import synthetic
from adaptive.common import atomic, read, digest
from adaptive.delivery import package, verify_repository

REPO=publication.REPO


def git(checkout,*args):
    return subprocess.run(['git','-C',str(checkout),'-c','credential.helper=',
        '-c','credential.helper=!gh auth git-credential',*args],check=True,
        capture_output=True,text=True,timeout=90).stdout.strip()


def ready_for_next(state,shared):
    if not (state/'publication-pushed.json').exists():return True
    previous=read(state/'publication-pushed.json')
    live=read(state/'current/active-production.json')
    if all(live.get(k)==v for k,v in previous.items()):return True
    pointer=shared/'requests/current.json'
    if not pointer.exists():return False
    rid=read(pointer)['id']
    if not isinstance(rid,str) or len(rid)!=32 or any(c not in '0123456789abcdef' for c in rid):raise ValueError('invalid approval pointer')
    request=read(shared/'requests'/(rid+'.json'))
    result=shared/'results'/(rid+'.json')
    return request['approval']==previous and result.exists() and read(result)['state'] in ('rejected','expired','failed','superseded','active')


def ensure_checkout(checkout):
    if not checkout.exists():
        checkout.parent.mkdir(parents=True,exist_ok=True)
        # Failed clones remain inspectable; never delete unknown operator files.
        git(checkout.parent,'clone','--branch','main','--single-branch',f'https://github.com/{REPO}.git',str(checkout))
    if git(checkout,'remote','get-url','origin')!=f'https://github.com/{REPO}.git':raise ValueError('wrong publication remote')
    if git(checkout,'branch','--show-current')!='main':raise ValueError('publication requires main')
    git(checkout,'config','user.name','Laya decoder publisher')
    git(checkout,'config','user.email','laya-decoder@users.noreply.github.com')


def receipt(checkout,commit):
    with tempfile.TemporaryDirectory() as tmp:
        package(checkout,tmp,commit)
        return {'repository':REPO,'commit':commit,'index_sha256':digest((Path(tmp)/'index.json').read_bytes())}


def finish(state,checkout,tx):
    plan=tx['plan'];head=git(checkout,'rev-parse','HEAD')
    if head==plan['base_commit']:
        if publication.paths(checkout)!=plan['files']:raise ValueError('prepared publication drift')
        git(checkout,'add','--',*sorted(plan['files']))
        git(checkout,'commit','-m',plan['title'])
        head=git(checkout,'rev-parse','HEAD')
    # Accept a post-commit crash only if this is exactly the recorded one-parent diff.
    if git(checkout,'rev-list','--parents','-n','1',head).split()!=[head,plan['base_commit']]:raise ValueError('unexpected commit ancestry')
    if git(checkout,'log','-1','--format=%s')!=plan['title']:raise ValueError('unexpected commit title')
    names=set(git(checkout,'diff-tree','--no-commit-id','--name-only','-r',head).splitlines())
    if names!=set(plan['files']) or git(checkout,'status','--porcelain'):raise ValueError('unexpected committed files')
    for name,sha in plan['files'].items():
        p=checkout/name
        if p.is_symlink() or digest(p.read_bytes())!=sha:raise ValueError('committed data drift')
    value=receipt(checkout,head)
    atomic(state/'publication-pending.json',value)
    # Repeating the same push is idempotent. Non-fast-forward is never forced.
    git(checkout,'push','origin',head+':refs/heads/main')
    atomic(state/'publication-pushed.json',value)
    job_path=state/'jobs'/(tx['family']+'.json');job=read(job_path)
    job.update(state='published',publication=value);atomic(job_path,job)
    tx.update(state='published',receipt=value);atomic(state/'auto-publication.json',tx)
    return value


def advance(state,checkout,shared,image,policy):
    if not policy or not policy['publish_validated']:return
    state,checkout,shared=Path(state),Path(checkout),Path(shared)
    with (state/'publication.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        tx_path=state/'auto-publication.json'
        tx=read(tx_path) if tx_path.exists() else None
        if tx and tx['state']=='prepared':
            if tx['policy_sha256']!=digest(synthetic.canonical(policy)):raise ValueError('publication policy changed')
            ensure_checkout(checkout)
            return finish(state,checkout,tx)
        if not ready_for_next(state,shared):return
        jobs=sorted((state/'jobs').glob('*.json'))
        for p in jobs:
            job=read(p)
            if job['state']!='awaiting_publication_approval':continue
            family=p.stem;template=state/'auto-templates'/(family+'.json')
            if not template.exists():continue  # manual proposals still require manual approval
            t=read(template);name=synthetic.identify(t['selector'])
            if name not in policy['synthetic_recipes'] or t!=synthetic.build(name) or job['template_sha256']!=digest(synthetic.canonical(t)):
                raise ValueError('proposal not from approved automatic recipe')
            # Require the immutable contract to be the independently generated gold.
            contract=read(state/'proposals'/family/'contract.json')
            if contract['cases']!=t['cases'] or contract['selector']!=t['selector']:raise ValueError('proposal oracle drift')
            ensure_checkout(checkout)
            if git(checkout,'status','--porcelain'):raise ValueError('dirty checkout without pending transaction')
            git(checkout,'fetch','origin','main')
            git(checkout,'merge','--ff-only','origin/main')
            # publication.prepare never executes checkout code; it uses the trusted verifier.
            plan=publication.prepare(state,checkout,family,image)
            if not plan['files']:raise ValueError('empty publication')
            allowed={'index.json','contracts/'+job['contract_sha256']+'.json'}|{
                'registry/bundles/'+job['bundle']+'/'+name for name in ('manifest.json','candidate.py','verification.json')}
            if not set(plan['files'])<=allowed:raise ValueError('publication exceeds one validated bundle')
            tx={'state':'prepared','family':family,'plan':plan,'policy_sha256':digest(synthetic.canonical(policy))}
            atomic(tx_path,tx)
            return finish(state,checkout,tx)


def main():
    p=argparse.ArgumentParser();p.add_argument('--state',type=Path,required=True);p.add_argument('--checkout',type=Path,required=True);p.add_argument('--shared',type=Path,required=True);p.add_argument('--image',required=True);a=p.parse_args()
    policy=synthetic.policy()
    if not policy or not policy['publish_validated']:return
    os.environ['GH_TOKEN']=(Path(os.environ['CREDENTIALS_DIRECTORY'])/'github-token').read_text().strip()
    # Also applies to publication.prepare's subprocesses, without token-in-URL storage.
    os.environ.update(GIT_CONFIG_COUNT='2',GIT_CONFIG_KEY_0='credential.helper',GIT_CONFIG_VALUE_0='',GIT_CONFIG_KEY_1='credential.helper',GIT_CONFIG_VALUE_1='!gh auth git-credential')
    try:advance(a.state,a.checkout,a.shared,a.image,policy)
    except Exception:
        print('Publication deferred; keep the same transaction, no force push or new commit.');raise SystemExit(1)

if __name__=='__main__':main()
