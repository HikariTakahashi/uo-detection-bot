"""Run generated decoders ONLY in disposable, constrained Docker containers."""
import ast
import json
import os
from pathlib import Path
import re
import selectors
import subprocess
import tempfile
import time
import uuid

ALLOWED_IMPORTS = {"re", "base64", "binascii", "codecs"}
FORBIDDEN = {"open", "exec", "eval", "compile", "globals", "locals", "vars", "getattr",
             "setattr", "delattr", "input", "print", "breakpoint", "help", "exit", "quit"}

RUNNER = '''import importlib.util,json,sys,os,resource
resource.setrlimit(resource.RLIMIT_AS,(134217728,134217728))
resource.setrlimit(resource.RLIMIT_CPU,(3,3))
resource.setrlimit(resource.RLIMIT_NPROC,(1,1))
spec=importlib.util.spec_from_file_location("candidate", "/work/candidate.py")
module=importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
inputs=json.load(sys.stdin)
values=[]
for text in inputs:
    try:
        value=module.decode(text)
        if value is not None and (type(value) is not str or len(value)>4096):
            raise ValueError("invalid output type/size")
        values.append({"value":value,"error":None})
    except Exception as error:
        values.append({"value":None,"error":type(error).__name__})
print(json.dumps({"values":values,"uid":os.geteuid(),"environment_keys":sorted(os.environ),"address_space_limit":resource.getrlimit(resource.RLIMIT_AS),"process_limit":resource.getrlimit(resource.RLIMIT_NPROC),"cpu_limit":resource.getrlimit(resource.RLIMIT_CPU)},ensure_ascii=False))
'''


def preflight(code):
    if not isinstance(code, str) or len(code.encode()) > 16000:
        raise ValueError("candidate size")
    tree = ast.parse(code)
    if not any(isinstance(n, ast.FunctionDef) and n.name == "decode" for n in tree.body):
        raise ValueError("decode function missing")
    for node in tree.body:
        if not isinstance(node, (ast.FunctionDef, ast.Import, ast.ImportFrom)) and not (
                isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant) and isinstance(node.value.value, str)):
            raise ValueError("only imports, functions and docstrings at module level")
    for node in ast.walk(tree):
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            modules = [a.name for a in node.names] if isinstance(node, ast.Import) else [node.module]
            if getattr(node, "level", 0) or any(m not in ALLOWED_IMPORTS for m in modules):
                raise ValueError("import outside allowlist")
        if isinstance(node, ast.Name) and (node.id in FORBIDDEN or node.id.startswith("__")):
            raise ValueError("forbidden name")
        if isinstance(node, ast.Attribute) and node.attr.startswith("_"):
            raise ValueError("private/introspection attribute")
        if isinstance(node, (ast.Global, ast.Nonlocal, ast.ClassDef)):
            raise ValueError("unsupported statement")
    # AST checks are a rejection filter, not a Python security sandbox.


def docker(args, **kwargs):
    return subprocess.run(["docker", *args], check=True, capture_output=True, timeout=30, **kwargs)


def bounded_run(args, payload, wall_seconds, limit=65536):
    proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = bytearray()
    try:
        proc.stdin.write(payload)
        proc.stdin.close()
        with selectors.DefaultSelector() as sel:
            sel.register(proc.stdout, selectors.EVENT_READ)
            end = time.monotonic() + wall_seconds
            while sel.get_map():
                if time.monotonic() >= end:
                    raise TimeoutError("decoder wall-time limit")
                for key, _ in sel.select(min(.2, max(0, end-time.monotonic()))):
                    chunk = os.read(key.fileobj.fileno(), 8192)
                    if not chunk:
                        sel.unregister(key.fileobj)
                        continue
                    output.extend(chunk)
                    if len(output) > limit:
                        raise ValueError("decoder output limit")
        status = proc.wait(timeout=2)
        if status:
            raise RuntimeError(f"decoder exit {status}")
        return json.loads(output)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=5)
        proc.stdout.close()


def evaluate(code, inputs, image, wall_seconds=12):
    preflight(code)
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
        raise ValueError("immutable local image ID required")
    # Docker Desktop's containerd store exposes the manifest digest as Id;
    # classic Docker on Pi exposes the config digest. Pin the DISTRIBUTION
    # manifest so both execute the identical reviewed arm64 image.
    image_ref = ('python@' + image) if image == 'sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b' else image
    payload = json.dumps(inputs, ensure_ascii=False).encode()
    if len(payload) > 16384 or not inputs or len(inputs) > 64 or any(type(s) is not str or len(s) > 2048 for s in inputs):
        raise ValueError("input bounds")
    name = "laya-decoder-" + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix="laya-decoder-") as tmp:
        directory = Path(tmp)
        directory.chmod(0o755)
        for file, content in (("candidate.py", code), ("runner.py", RUNNER)):
            (directory/file).write_text(content)
            (directory/file).chmod(0o444)
        created = False
        try:
            docker(["create", "--name", name, "--interactive", "--network", "none", "--read-only",
                    "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                    "--memory", "128m", "--memory-swap", "128m", "--cpus", "0.5", "--pids-limit", "16",
                    "--ulimit", "cpu=3:3", "--ulimit", "nofile=64:64", "--log-driver", "none",
                    "--tmpfs", "/tmp:rw,noexec,nosuid,size=1048576",
                    "--mount", f"type=bind,source={directory},target=/work,readonly",
                    "--entrypoint", "/usr/bin/env", image_ref, "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp",
                    "python3", "-I", "-B", "/work/runner.py"])
            created = True
            inspect = json.loads(docker(["inspect", name]).stdout)[0]
            host, config = inspect["HostConfig"], inspect["Config"]
            if not (host["NetworkMode"] == "none" and host["ReadonlyRootfs"] and not host["Privileged"]
                    and host["CapDrop"] == ["ALL"] and "no-new-privileges" in host["SecurityOpt"]
                    and ((host["Memory"] == 134217728 and host["MemorySwap"] == 134217728)
                         or (host["Memory"] == 0 and host["MemorySwap"] in (0,-1)))
                    and host["PidsLimit"] == 16 and config["User"] == "65534:65534"
                    and len(inspect["Mounts"]) == 1 and not inspect["Mounts"][0]["RW"]
                    and inspect["Mounts"][0]["Destination"] == "/work"):
                raise ValueError("container isolation verification failed")
            start = time.monotonic()
            result = bounded_run(["docker", "start", "--attach", "--interactive", name], payload, wall_seconds)
            if result.get("uid") != 65534 or len(result.get("values", [])) != len(inputs) or result.get("address_space_limit") != [134217728,134217728] or result.get("process_limit") != [1,1] or result.get("cpu_limit") != [3,3]:
                raise ValueError("runner output integrity")
            result["sandbox"] = {"image": image, "image_ref": image_ref, "network": host["NetworkMode"], "read_only": True,
                                 "user": config["User"], "cap_drop": host["CapDrop"], "memory": host["Memory"],
                                 "pids_limit": host["PidsLimit"], "address_space_limit": result["address_space_limit"], "process_limit": result["process_limit"], "wall_seconds": wall_seconds,
                                 "mount": "temporary runner and candidate only, read-only", "seconds": time.monotonic()-start}
            return result
        finally:
            if created:
                # This name was generated by this function, never taken from candidate output.
                docker(["rm", "--force", name])
