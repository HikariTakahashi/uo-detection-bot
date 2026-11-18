"""Public fixtures from trusted specifications, never from Discord message content.

A specification is evidence supplied independently of the generated decoder.
Arbitrary ciphertext has no automatic ground truth: unsupported selectors stay held.
"""
import base64
import json
import re
from pathlib import Path
import stat
from adaptive.common import atomic, canonical, digest, read
from adaptive.discovery import validate_selector, split_wrapper

VERSION = 2
SPECS = {
    'x9': 'Input includes the exact prefix x9:. Each UTF-8 byte b is transformed to (73*b+41)%256 and written as two UPPERCASE hex digits. Decode the inverse permutation, then strict UTF-8. Nonempty even-length payload only. Reject lowercase hex, whitespace, wrong prefix, malformed data and invalid UTF-8.',
    'hex': 'Input is nonempty even-length UPPERCASE hexadecimal UTF-8 bytes, no prefix. Strict UTF-8. Reject lowercase, whitespace, non-ASCII hex and malformed input.',
    'base64': 'Input is canonical standard Base64 of UTF-8 bytes with required padding, no prefix. Strict UTF-8. Reject whitespace, URL-safe alphabet, noncanonical padding bits and malformed input.',
    'base32': 'Input is canonical uppercase RFC4648 Base32 of UTF-8 bytes with required padding, no prefix. Strict UTF-8. Reject lowercase, whitespace, noncanonical padding bits and malformed input.',
    'percent': 'Input consists entirely of %HH tokens, UPPERCASE ASCII hex for every UTF-8 byte, no prefix. Strict UTF-8. Reject mixed plain text, lowercase hex and malformed input.',
    'unicode': 'Input is a nonempty sequence of backslash-u escapes with exactly four ASCII hex digits each, no prefix. Decode Unicode scalar values, combining valid UTF-16 surrogate pairs. Reject lone surrogates, mixed plain text and malformed input.',
}
SELECTORS = {
    'x9': {'kind':'prefix','value':'x9:'},
    'hex': {'kind':'shape','value':'hex-upper-token'},
    'base64': {'kind':'shape','value':'ascii-alnum-token'},
    'base32': {'kind':'shape','value':'upper-2to7-token'},
    'percent': {'kind':'shape','value':'percent-upper-token'},
    'unicode': {'kind':'shape','value':'unicode-escape-token'},
}

# This recipe resolves competing interpretations instead of declaring all hex x9.
SPECS['hex-or-x9'] = ('Input is a nonempty even-length uppercase ASCII hex token without prefix. '
 'Consider TWO interpretations: bytes as UTF-8, and each byte transformed with (249*(b-41))%256 then UTF-8. '
 'Strict UTF-8 for both. An eligible result is nonempty, has at least one character whose isalnum() is true, '
 'and every character is printable (str.isprintable()) or is newline or tab. '
 'Deduplicate eligible results. Return the sole distinct result; otherwise None. '
 'Do not prefer x9, Japanese, target words or a specific phrase. Reject lowercase, spaces, odd lengths and any prefix. '
 'Decode only one layer; preserve any inner encoding.')
SELECTORS['hex-or-x9'] = SELECTORS['hex']
AUTOMATIC_RECIPES = ['x9','hex-or-x9','base64','base32','percent','unicode']


def readable(value):
    return bool(value) and any(c.isalnum() for c in value) and all(c.isprintable() or c in '\n\t' for c in value)


def hex_hypotheses(text):
    if not isinstance(text,str) or not re.fullmatch(r'(?:[0-9A-F]{2})+',text):return {}
    raw=bytes.fromhex(text)
    # Lookup inversion is independent of the inverse formula supplied to Luna.
    inverse={((73*b+41)%256):b for b in range(256)}
    candidates={}
    for name,data in [('hex',raw),('x9-unwrapped',bytes(inverse[b] for b in raw))]:
        try:value=data.decode('utf-8',errors='strict')
        except UnicodeError:continue
        if readable(value):candidates[name]=value
    return candidates


def reference_decode(name,text):
    """Trusted local hypothesis check, never production classification or generated code."""
    try:
        if name=='hex-or-x9':
            values=set(hex_hypotheses(text).values())
            return next(iter(values)) if len(values)==1 else None
        if name=='x9':
            if not text.startswith('x9:') or not re.fullmatch(r'(?:[0-9A-F]{2})+',text[3:]):return None
            inverse={((73*b+41)%256):b for b in range(256)}
            return bytes(inverse[b] for b in bytes.fromhex(text[3:])).decode('utf-8')
        if name=='base64':
            raw=base64.b64decode(text,validate=True)
            if base64.b64encode(raw).decode()!=text:return None
            return raw.decode('utf-8')
        if name=='base32':
            raw=base64.b32decode(text,casefold=False)
            if base64.b32encode(raw).decode()!=text:return None
            return raw.decode('utf-8')
        if name=='percent':
            if not re.fullmatch(r'(?:%[0-9A-F]{2})+',text):return None
            return bytes.fromhex(text.replace('%','')).decode('utf-8')
        if name=='unicode':
            if not re.fullmatch(r'(?:\\u[0-9A-Fa-f]{4})+',text):return None
            return bytes.fromhex(text.replace('\\u','')).decode('utf-16-be')
        if name=='hex':
            if not re.fullmatch(r'(?:[0-9A-F]{2})+',text):return None
            return bytes.fromhex(text).decode('utf-8')
    except (ValueError,UnicodeError):return None
    return None


def infer_chain(text,max_depth=8):
    """Bounded candidate inference for preparation only; observations never leave Pi."""
    from adaptive.discovery import candidate
    plans=[];seen=set()
    for depth in range(max_depth):
        if text in seen:return {'state':'cycle','plans':plans}
        seen.add(text)
        c=explicit_candidate(text) or candidate(text)
        if not c:return {'state':'terminal','plans':plans}
        name='hex-or-x9' if c['selector']==SELECTORS['hex'] else identify(c['selector'])
        if name not in AUTOMATIC_RECIPES:return {'state':'needs_format_spec','plans':plans}
        payload=text if c['selector']['kind']=='prefix' else c.get('input',text)
        hypotheses=hex_hypotheses(payload) if name=='hex-or-x9' else {}
        output=reference_decode(name,payload)
        if output is None or not readable(output):
            return {'state':'ambiguous_candidates' if len(set(hypotheses.values()))>1 else 'needs_format_spec','plans':plans}
        methods=sorted(hypotheses) if hypotheses else [name]
        plans.append({'family':c['family'],'selector':c['selector'],'recipe':name,
                      'evidence':{'methods':methods,'depth':depth,'unique_result':True}})
        text=output
    return {'state':'budget','plans':plans}


def identify(selector):
    validate_selector(selector)
    return next((name for name, value in SELECTORS.items() if selector == value), None)


def explicit_candidate(text):
    # A declared spec is scoped to its exact wrapper, never applied to bare hex.
    prefix, _ = split_wrapper(text)
    if prefix != 'x9:': return None
    selector = SELECTORS['x9']
    return {'family':'discovered-'+digest(canonical(selector))[:16], 'selector':selector}


def encode(name, text):
    data = text.encode('utf-8')
    if name == 'x9': return 'x9:'+''.join(f'{(73*b+41)%256:02X}' for b in data)
    if name == 'hex': return data.hex().upper()
    if name == 'hex-or-x9': return encode('x9',text)[3:]
    if name == 'base64': return base64.b64encode(data).decode()
    if name == 'base32': return base64.b32encode(data).decode()
    if name == 'percent': return ''.join(f'%{b:02X}' for b in data)
    if name == 'unicode':
        raw=text.encode('utf-16-be')
        return ''.join('\\u'+raw[i:i+2].hex() for i in range(0,len(raw),2))
    raise ValueError('no independent encoding specification')


def build(name):
    if name=='hex-or-x9':return build_hex_candidates()
    selector=SELECTORS[name]
    family='discovered-'+digest(canonical(selector))[:16]
    # Shared examples and hidden gold cases are disjoint and fixed before Luna.
    samples=[encode(name,s) for s in ('Cat 42!', 'あいう', 'Hello, world!')]
    sources=['うおw','uo、それはないよww','どわーｗｗ','うお！ありがとう！',
             '「うおw」の意味','ordinary message','🙂','é','A\nB','\x00',
             'うおw。どわーww','x9:NOT_HEX',base64.b64encode(b'uoww').decode()]
    sources += ['合成'+str(i)+chr(0x3041+i)+'!' for i in range(12)]
    # An independent mathematical inverse sanity check prevents a broken oracle.
    if name=='x9':
        assert len({(73*b+41)%256 for b in range(256)})==256
    positives=[{'text':encode(name,s),'expected':s} for s in sources]
    negatives=['','not an encoded payload','普通の文章']
    negatives += {
        'x9':['x9:','x9:A','x9:GG','X9:1234','x9:１Ａ','x9:00 ', 'x9:'+encode('x9','cat')[3:].lower(), 'x9:E0', 'x9:E9'],
        'hex':['A','GG','e38186','E38186 ', 'FF','１Ａ'],
        'base64':['YQ','YQ===','YR==','/w==','YQ== ','!!!!'],
        'base32':['MY','MZ======','my======','74======','MY====== ','!!!!'],
        'percent':['%A','%GG','plain%41','%e3%81%86','%FF','%41 '],
        'unicode':['\\u123','\\uZZZZ','plain\\u0041','\\uD800','\\uDC00','\\uD800\\u0041'],
    }[name]
    return {'family':family,'selector':selector,'samples':samples,
            'cases':positives+[{'text':s,'expected':None} for s in negatives],
            'public_synthetic_approved':True,'specification':SPECS[name]}


def build_hex_candidates():
    selector=SELECTORS['hex-or-x9'];family='discovered-'+digest(canonical(selector))[:16]
    samples=[encode('hex','Cat 42!'),encode('hex-or-x9','あいう'),encode('hex-or-x9','Hello, world!')]
    # Both forward encoders are independent of the generated inverse. Ambiguous
    # fixtures are explicit rejection gold, never selected by a model score.
    sources=['うおw','uo、それはないよww','どわーｗｗ','うお！ありがとう！','「うおw」の意味',
             'ordinary message','é','A\nB','うおw。どわーww','x9:NOT_HEX',
             encode('base64','うおw'),encode('percent','どわーww'),encode('x9','うおw')]
    cases=[]
    for name in ['hex','hex-or-x9']:
        for source in sources:
            encoded=encode(name,source);expected=reference_decode('hex-or-x9',encoded)
            assert expected is None or expected==source
            cases.append({'text':encoded,'expected':expected})
    for text in ['', 'not encoded','普通の文章','A','GG','e38186','E38186 ', '１Ａ','x9:E4F25FE4F28318',
                 '4141','41414141','0000','FFFF','E4F25FE4F28318 '.lower()]:
        cases.append({'text':text,'expected':reference_decode('hex-or-x9',text)})
    assert any(c['expected'] is None and len(hex_hypotheses(c['text']))==2 for c in cases)
    return {'family':family,'selector':selector,'samples':samples,'cases':cases,
            'public_synthetic_approved':True,'specification':SPECS['hex-or-x9']}


def template_recipe(template):
    for name in SPECS:
        if SELECTORS[name]==template.get('selector') and template==build(name):return name
    raise ValueError('untrusted generated template')


def prompt(template):
    # This branch is used only with an exact regenerated trusted template.
    name=template_recipe(template)
    value=('公開仕様に従う汎用Python関数 decode(text: str) -> str | None を生成してください。\n'
           'Specification: '+template['specification']+'\n'
           'Public synthetic observations: '+json.dumps(template['samples'],ensure_ascii=False)+'\n'
           '仕様の入力全体をdecodeへ渡します。接頭辞を含む仕様は接頭辞も受け取り検証。復元は1段だけ、内層はそのまま返す。'
           '不正・別形式はNone。例の対応表・特定語置換・文章創作は禁止。Pythonコードブロック1個のみ。'
           '標準ライブラリre,base64,binascii,codecsのみ使用可能。'
           'モジュール直下はimportと関数定義のみ。ファイル、通信、プロセス、eval/exec、動的属性参照、__属性、print、テスト呼出しは禁止。')
    if len(value)>1700 or '@' in value:raise ValueError('public prompt bounds')
    return value


def policy(path='/etc/laya-decoder/automatic.json'):
    path=Path(path)
    if not path.exists():return None
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=0 or info.st_mode&0o022:raise ValueError('root-owned automatic policy required')
    value=read(path)
    if (set(value)!={'schema','synthetic_recipes','publish_validated','repository','daily_jobs'}
        or value['schema']!=1 or type(value['publish_validated']) is not bool
        or value['repository']!='HikariTakahashi/uo-decoder-evaluation' or value['daily_jobs']!=3
        or not isinstance(value['synthetic_recipes'],list) or not value['synthetic_recipes']
        or any(name not in SPECS for name in value['synthetic_recipes'])
        or len(set(value['synthetic_recipes']))!=len(value['synthetic_recipes'])):
        raise ValueError('unsupported automatic policy')
    return value


def prepare(state, allowed):
    state=Path(state);folder=state/'auto-templates';folder.mkdir(exist_ok=True)
    for p in sorted((state/'discovery').glob('*.json')):
        item=read(p)
        if item.get('state')=='known_rejection':continue
        name=item.get('recipe') or identify(item['selector'])
        if name in SELECTORS and SELECTORS[name]!=item['selector']:raise ValueError('recipe selector mismatch')
        family='discovered-'+digest(canonical(item['selector']))[:16]
        if item['family']!=family or p.name!=family+'.json':raise ValueError('discovery identity mismatch')
        status=state/'preparation'/(family+'.json')
        if name not in allowed:
            atomic(status,{'state':'needs_format_spec','family':family});continue
        template=build(name);prompt(template)
        dest=folder/(family+'.json')
        if dest.exists() and read(dest)!=template:raise ValueError('immutable synthetic template drift')
        if not dest.exists():atomic(dest,template)
        atomic(status,{'state':'ready','family':family,'recipe':name,'version':VERSION,
                       'template_sha256':digest(canonical(template)),'source':'trusted_specification_no_discord_content'})
    return folder
