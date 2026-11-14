#!/usr/bin/env python3
"""Download only the explicitly pinned public model into the experiment folder."""
import json
import os
from pathlib import Path

from classify import MODEL_DIR, MODEL_ID, REVISION, ROOT

os.environ["HF_HOME"] = str(ROOT / "cache" / "huggingface")
os.environ["HF_HUB_DISABLE_IMPLICIT_TOKEN"] = "1"
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"

from huggingface_hub import snapshot_download

snapshot_download(
    MODEL_ID, revision=REVISION, local_dir=str(MODEL_DIR), token=False,
    allow_patterns=["*.json", "model.safetensors", "encoder/*.json", "tokenizer/*.json"],
    max_workers=2,
)
(ROOT / "model-receipt.json").write_text(
    json.dumps({"model": MODEL_ID, "revision": REVISION, "path": str(MODEL_DIR)}, indent=2) + "\n",
    encoding="utf-8",
)
print(f"Model ready: {MODEL_DIR}")
