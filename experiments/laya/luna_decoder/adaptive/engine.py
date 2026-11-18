import time
from .common import digest
from sandbox import evaluate
from .discovery import dispatch


def cyclic(edges):
    adjacency={}
    for edge in edges:adjacency.setdefault(edge['input_sha256'],set()).add(edge['output_sha256'])
    visited=set();active=set()
    def visit(node):
        if node in active:return True
        if node in visited:return False
        visited.add(node);active.add(node)
        if any(visit(n) for n in adjacency.get(node,())):return True
        active.remove(node);return False
    return any(visit(n) for n in adjacency)


def explore(text,decoders,image,max_depth=8,max_nodes=32,max_seconds=60,execute=evaluate,view=None):
    """Bounded graph search; terminal text, not decoder claims, is reclassified."""
    started=time.monotonic();queue=[(text,[])];seen={digest(text)};leaves={};edges=[];errors=[]
    while queue:
        current,chain=queue.pop(0)
        if time.monotonic()-started>=max_seconds:return {'status':'budget','edges':edges,'errors':errors}
        fixed=view(current) if view else {'candidates':[], 'literal_units':0}
        if time.monotonic()-started>=max_seconds:return {'status':'budget','edges':edges,'errors':errors}
        if fixed.get('blocked'):return {'status':'ambiguous','edges':edges,'errors':errors,'candidates':[current]}
        if fixed['literal_units']:
            leaves[current]=chain;continue
        matched=[(r,dispatch(r,current)) for r in decoders if dispatch(r,current) is not None]
        # An explicitly scoped format owns its wrapper; broad lexical decoders
        # must not reinterpret that wrapper's payload as a different encoding.
        explicit=[(r,p) for r,p in matched if r.get('selector',{}).get('kind')=='prefix' or 'selector' not in r]
        if explicit:matched=explicit
        values=[('fixed', 'fixed', v) for v in fixed['candidates']] if not explicit else []
        if not matched and not values:
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
                values.append((decoder['id'],decoder['family'],value))
            except Exception as exc:
                errors.append({'decoder':decoder['id'],'error':type(exc).__name__})
        for decoder_id,family,value in values:
            if not isinstance(value,str) or not value.strip() or len(value)>2000:
                errors.append({'decoder':decoder_id,'error':'invalid_fixed_view'});continue
            h=digest(value)
            edges.append({'decoder':decoder_id,'family':family,'input_sha256':digest(current),'output_sha256':h})
            advanced=True
            if h in seen:continue
            if len(seen)>=max_nodes:return {'status':'budget','edges':edges,'errors':errors}
            seen.add(h);queue.append((value,chain+[decoder_id]))
        if not advanced:
            # A broad shape can contain another encoding. Rejection leaves it
            # available to the discovery controller; legacy explicit formats fail.
            if all(r.get('selector',{}).get('kind')=='shape' for r,_ in matched):leaves[current]=chain
            else:errors.append({'input_sha256':digest(current),'error':'no_decoder_accepted'})
    if cyclic(edges):return {'status':'cycle','edges':edges,'errors':errors}
    if errors:return {'status':'invalid','edges':edges,'errors':errors}
    if len(leaves)!=1:return {'status':'ambiguous' if leaves else 'cycle','edges':edges,'errors':errors,'candidates':list(leaves)}
    body,chain=next(iter(leaves.items()))
    return {'status':'terminal','text':body,'chain':chain,'edges':edges,'errors':errors}
