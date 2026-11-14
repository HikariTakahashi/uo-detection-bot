#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
export HF_HOME="$ROOT/cache/huggingface"
export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 HF_HUB_DISABLE_IMPLICIT_TOKEN=1
export HF_HUB_DISABLE_TELEMETRY=1 USE_TF=0 TOKENIZERS_PARALLELISM=false
export OMP_NUM_THREADS=2 OPENBLAS_NUM_THREADS=2 MKL_NUM_THREADS=2
unset LAYA_CPU_AMP
# This Pi has no memory cgroup controller. Bound the entire process address space
# instead (includes mapped files and allocations, not just resident RAM).
ulimit -v 4194304
# Fail rather than silently running without the resource limits.
exec flock -n "$ROOT/run.lock" systemd-run --user --scope --quiet --unit=laya-sandbox \
    -p CPUQuota=100% \
    nice -n 10 "$ROOT/.venv/bin/python" "$ROOT/classify.py" "$@"
