"""Independent synthetic oracle. NEVER copied into model or decoder containers."""
import base64


def encode(family, text):
    if family == "z1":
        return "z1:"+text.encode().hex()[::-1]
    if family == "q2":
        return "q2:"+base64.b64encode(text.encode()).decode()[::-1]
    if family == "n3":
        return "n3:"+"/".join(str(ord(c)) for c in text)
    raise ValueError("unknown synthetic family")


FAMILIES = ("z1", "q2", "n3")


def demonstrations(family):
    return [{"input": encode(family, s), "output": s} for s in ("cat", "あい", "Hello42!")]


def contract(family):
    return {
        "z1": "z1: followed by a nonempty even-length ASCII hexadecimal payload; strict UTF-8; reject invalid data.",
        "q2": "q2: followed by reversed canonical standard Base64 with required padding; strict UTF-8; reject invalid/noncanonical data.",
        "n3": "n3: followed by slash-separated canonical ASCII decimal Unicode scalar values (0..1114111 excluding surrogates); no leading zero except 0; no empty tokens.",
    }[family]


def validation_cases(family):
    sources = ["うおw", "どわーｗｗ", "優勝おめでとう！", "Hello world!", "🙂", "A\nB", "\x00", "é", "うお どわー uo"]
    # Includes nested payloads and broad Unicode; no generated decoder supplies the oracle.
    sources += [encode(other, "うおw") for other in FAMILIES if other != family]
    sources += ["合成例"+str(i)+chr(0x3041+i)+"!" for i in range(12)]
    positives = [{"text":encode(family,s),"expected":s,"kind":"valid"} for s in sources]
    negatives = ["", "普通の文章", "00012345", "SHA256: deadbeef", "うおw"]
    negatives += [encode(other,"cat") for other in FAMILIES if other != family]
    negatives += {"z1":["z1:","z1:f","z1:gg","z1:ff","z1:00 ","z1:００"],
                  "q2":["q2:","q2:!", "q2:YQ", "q2:==RZ", "q2:=w/", "q2:==QY "],
                  "n3":["n3:","n3:01", "n3:65/", "n3:/65", "n3:-1", "n3:55296", "n3:1114112", "n3:６５"]}[family]
    return positives + [{"text":s,"expected":None,"kind":"reject"} for s in negatives]


def suite():
    sources = [("うおw","mockery"), ("どわーｗｗ、それはないよ。","mockery"),
               ("うお！合格おめでとう！","other"), ("また『うおw』って書いたの？","other")]
    rows=[]
    for family in FAMILIES:
        for i,(text,label) in enumerate(sources):
            rows.append(dict(id=f"{family}-{i}",text=encode(family,text),context="",kind="encoded",expected=text,label=label,family=family))
    for i,chain in enumerate((("z1","q2"),("q2","z1"),("z1","q2","n3"))):
        text="うおw"
        for family in reversed(chain):text=encode(family,text)
        rows.append(dict(id=f"chain-{i}",text=text,context="",kind="encoded",expected="うおw",label="mockery",family=chain[0]))
    for i,(text,label) in enumerate((("うおｗｗの意味を説明して。","other"),("彼は「うおw」と書いた。","other"),
                                   ("どわーww またやったのか。","mockery"),("00012345","other"),
                                   ("SHA256: "+"a19c47d6"*8,"other"),("明日は雨です。","other"))):
        rows.append(dict(id=f"plain-{i}",text=text,context="",kind="plain",expected=text,label=label,family=None))
    for i,text in enumerate(("z1:ff","q2:!","n3:55296")):
        rows.append(dict(id=f"invalid-{i}",text=text,context="",kind="invalid",expected=None,label="uncertain",family=text[:2]))
    return rows
