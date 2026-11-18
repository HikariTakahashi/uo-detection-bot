"""Content-addressed evaluation delivery. Generated files are never imported."""
from pathlib import Path
import re
import shutil
import tempfile
from .common import atomic,canonical,digest,read
from sandbox import evaluate
from pipeline import extract_code, verify_reply
from .discovery import validate_selector
from .estimation_reply import parse_reply


class Registry:
    def __init__(self,path,image):
        self.path=Path(path);self.path.mkdir(parents=True,exist_ok=True);self.image=image
        self.pointer=self.path/'active.json'

    def active(self):
        active=read(self.pointer) if self.pointer.exists() else {}
        records=[]
        for family,sha in sorted(active.items()):
            manifest,code=self.load(sha)
            if manifest['family']!=family:raise ValueError('registry family mismatch')
            record={'id':sha,'family':family,'prefix':manifest['prefix'],'code':code}
            if 'selector' in manifest:record['selector']=validate_selector(manifest['selector'])
            records.append(record)
        return records

    def load(self,sha):
        if not re.fullmatch('[a-f0-9]{64}',sha):raise ValueError('invalid bundle id')
        folder=self.path/'bundles'/sha
        raw=(folder/'manifest.json').read_bytes()
        if digest(raw)!=sha:raise ValueError('manifest hash mismatch')
        m=read(folder/'manifest.json');code=(folder/'candidate.py').read_text()
        if digest(code)!=m['code_sha256'] or digest((folder/'verification.json').read_bytes())!=m['verification_sha256']:raise ValueError('bundle content mismatch')
        verify_reply(m['reply'],m['request_id'])
        if extract_code(m['reply']['reply'])!=code:raise ValueError('reply/code mismatch')
        if 'candidate_index' in m:
            source=m['estimation_reply'];verify_reply(source,m['request_id'])
            if parse_reply(source['reply'])[m['candidate_index']]['code']!=code:raise ValueError('estimation/code mismatch')
            if m['reply'].get('normalization')!='candidate-extraction' or any(m['reply'].get(k)!=v for k,v in source.items() if k!='reply'):
                raise ValueError('estimation provenance mismatch')
        return m,code

    def stage(self,family,reply,cases,selector=None,candidate_index=None):
        if selector is not None:
            validate_selector(selector)
            if not any(c['expected'] is not None for c in cases) or not any(c['expected'] is None for c in cases):
                raise ValueError('independent positive and rejection evidence required')
        verify_reply(reply,reply['request_id'])
        code=(parse_reply(reply['reply'])[candidate_index]['code']
              if candidate_index is not None else extract_code(reply['reply']))
        # Preserve the original response and mark the extraction explicitly.
        # The ordinary code-block view keeps existing independent CI/CD readers
        # compatible; it is not represented as an unmodified Luna response.
        original=reply
        if candidate_index is not None:
            reply={**reply,'reply':'```python\n'+code+'```','normalization':'candidate-extraction'}
        result=evaluate(code,[c['text'] for c in cases],self.image)
        passed=[v['error'] is None and v['value']==c['expected'] for c,v in zip(cases,result['values'])]
        if len(passed)!=len(cases) or not all(passed):raise ValueError('independent decoder tests failed: '+str([i for i,v in enumerate(passed) if not v]))
        verification={'cases':cases,'passed':passed,'execution':result}
        with tempfile.TemporaryDirectory(dir=self.path) as tmp:
            folder=Path(tmp);(folder/'candidate.py').write_text(code)
            atomic(folder/'verification.json',verification)
            manifest={'schema':1,'scope':'synthetic-evaluation-only','family':family,'prefix':family+':','code_sha256':digest(code),
                      'verification_sha256':digest((folder/'verification.json').read_bytes()),'request_id':reply['request_id'],'reply':reply,'image':self.image}
            if selector is not None:manifest['selector']=selector
            if candidate_index is not None:
                manifest['candidate_index']=candidate_index
                manifest['estimation_reply']=original
            atomic(folder/'manifest.json',manifest)
            sha=digest((folder/'manifest.json').read_bytes());dest=self.path/'bundles'/sha
            dest.parent.mkdir(exist_ok=True)
            if not dest.exists():shutil.copytree(folder,dest)
        self.load(sha)
        return sha

    def install(self,source,expected_sha,cases):
        """CD: copy exactly three files, pin manifest, revalidate locally, then switch."""
        source=Path(source)
        if digest((source/'manifest.json').read_bytes())!=expected_sha:raise ValueError('delivery manifest mismatch')
        m=read(source/'manifest.json')
        if m.get('scope')!='synthetic-evaluation-only' or m.get('schema')!=1 or m['image']!=self.image:raise ValueError('delivery scope/image mismatch')
        dest=self.path/'bundles'/expected_sha;dest.parent.mkdir(exist_ok=True)
        if not dest.exists():
            with tempfile.TemporaryDirectory(dir=dest.parent) as tmp:
                for name in ('manifest.json','candidate.py','verification.json'):
                    src=source/name
                    if src.is_symlink() or not src.is_file() or src.stat().st_size>131072:raise ValueError('invalid delivery file')
                    shutil.copyfile(src,Path(tmp)/name)
                Path(tmp).rename(dest)
        manifest,code=self.load(expected_sha)
        result=evaluate(code,[c['text'] for c in cases],self.image)
        if len(result['values'])!=len(cases) or any(v['error'] or v['value']!=c['expected'] for c,v in zip(cases,result['values'])):raise ValueError('CD verification failed; previous registry retained')
        previous=read(self.pointer) if self.pointer.exists() else {}
        atomic(self.path/'previous.json',previous)
        atomic(self.pointer,{**previous,manifest['family']:expected_sha})
        atomic(self.path/'receipts'/f'{expected_sha}.json',{'verified':True,'bundle':expected_sha,'execution':result})

    def rollback(self):
        old=read(self.path/'previous.json')
        for sha in old.values():self.load(sha)
        atomic(self.pointer,old)

    def revision(self):
        return digest(canonical([(r['family'],r['id']) for r in self.active()]))
