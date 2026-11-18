#!/usr/bin/env python3
"""Package explicit production files, never evaluation runs or machine secrets."""
import argparse
import hashlib
from pathlib import Path
import shutil
import tarfile
import tempfile

ROOT=Path(__file__).resolve().parents[1]
LIBRARY=['fixtures.py','pipeline.py','sandbox.py','adaptive/common.py','adaptive/discovery.py','adaptive/engine.py','adaptive/registry.py','adaptive/catalog.py','adaptive/delivery.py','adaptive/publish.py','adaptive/estimation_reply.py']
FILES=[
 'scripts/install-adaptive.sh','adaptive.config.example.json','estimation.config.example.json',
 'experiments/laya/serve_shadow.py','experiments/laya/production_policy.py',
 *['experiments/laya/luna_decoder/'+p for p in LIBRARY],
 *['services/decoder/'+p for p in ['runtime.py','release.py','generate.py','synthetic.py','autopublish.py','publication.py','approval.py','estimation.py','initial-release.json']],
 *['deploy/'+p for p in ['laya-decoder.service','laya-decoder-update.service','laya-decoder-update.timer','laya-decoder-generate.service','laya-decoder-generate.timer','laya-decoder-approval.service','laya-decoder-approval.timer','laya-decoder-publication.service','laya-decoder-publication.timer']],
]

def package(root,binary,output):
    root,binary,output=Path(root),Path(binary),Path(output)
    with tempfile.TemporaryDirectory() as tmp:
        stage=Path(tmp)/'decoder-production';stage.mkdir()
        for name in FILES:
            src=root/name
            if src.is_symlink() or not src.is_file():raise ValueError('missing or symlinked runtime file: '+name)
            dst=stage/name;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(src,dst);dst.chmod(0o644)
        shutil.copyfile(binary,stage/'discord-bot-linux-arm64');(stage/'discord-bot-linux-arm64').chmod(0o755)
        sums=[]
        for path in sorted(p for p in stage.rglob('*') if p.is_file()):
            sums.append(hashlib.sha256(path.read_bytes()).hexdigest()+'  '+str(path.relative_to(stage)))
        (stage/'DEPLOY-SHA256SUMS').write_text('\n'.join(sums)+'\n')
        output.parent.mkdir(parents=True,exist_ok=True)
        with tarfile.open(output,'w:gz') as archive:archive.add(stage,arcname='decoder-production')

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--binary',required=True);p.add_argument('--output',required=True);a=p.parse_args();package(ROOT,a.binary,a.output)
