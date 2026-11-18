import time
from .common import digest
from sandbox import evaluate
from .discovery import dispatch


def explore(text,decoders,image,max_depth=8,max_nodes=32,max_seconds=60,execute=evaluate):
    """Bounded graph search; terminal text, not decoder claims, is reclassified."""
    started=time.monotonic();queue=[(text,[])];seen={digest(text)};leaves={};edges=[];errors=[]
    while queue:
        current,chain=queue.pop(0)
        if time.monotonic()-started>=max_seconds:return {'status':'budget','edges':edges,'errors':errors}
        matched=[(r,dispatch(r,current)) for r in decoders if dispatch(r,current) is not None]
        if not matched:
            leaves[current]=chain;continue
        if len(chain)>=max_depth:return {'status':'budget','edges':edges,'errors':errors}
        advanced=False
        for decoder,payload in matched:
            remaining=max_seconds-(time.monotonic()-started)
            if remaining<=0:return {'status':'budget','edges':edges,'errors':errors}
            try:
                result=execute(decoder['code'],[payload],image,wall_seconds=min(12,remaining))['values'][0]
                if result['error']:raise ValueError(result['error'])
                value=result['value']
                if value is None:continue
                if not isinstance(value,str) or not value.strip() or len(value)>2000:raise ValueError('decoded body outside model limits')
                h=digest(value)
                edges.append({'decoder':decoder['id'],'family':decoder['family'],'input_sha256':digest(current),'output_sha256':h})
                advanced=True
                if h in seen:continue
                if len(seen)>=max_nodes:return {'status':'budget','edges':edges,'errors':errors}
                seen.add(h);queue.append((value,chain+[decoder['id']]))
            except Exception as exc:
                errors.append({'decoder':decoder['id'],'error':type(exc).__name__})
        if not advanced:
            # A broad shape can contain another encoding. Rejection leaves it
            # available to the discovery controller; legacy explicit formats fail.
            if all(r.get('selector',{}).get('kind')=='shape' for r,_ in matched):leaves[current]=chain
            else:errors.append({'input_sha256':digest(current),'error':'no_decoder_accepted'})
    if errors:return {'status':'invalid','edges':edges,'errors':errors}
    if len(leaves)!=1:return {'status':'ambiguous' if leaves else 'cycle','edges':edges,'errors':errors,'candidates':list(leaves)}
    body,chain=next(iter(leaves.items()))
    return {'status':'terminal','text':body,'chain':chain,'edges':edges,'errors':errors}
