#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ $# != 0 ]]; then
  echo 'FAIL: verification wrapper takes no arguments'
  exit 1
fi
exec python3 scripts/verify.py
