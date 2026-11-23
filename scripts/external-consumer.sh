#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
events=""
if [[ $# -eq 2 && $1 == --events ]]; then events=$2; elif [[ $# != 0 ]]; then exit 1; fi
export GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0
collector="$PWD/scripts/capture-events.py"
scratch=$(mktemp -d)
cleanup() {
  GOMODCACHE="$scratch/modcache" go clean -modcache >/dev/null 2>&1
  python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1])' "$scratch"
}
trap cleanup EXIT
python3 scripts/candidate-proxy.py "$scratch/proxy"
mkdir "$scratch/consumer"
cp tests/consumer/* "$scratch/consumer/"
cd "$scratch/consumer"
export GOPROXY="file://$scratch/proxy,https://proxy.golang.org" GONOSUMDB=github.com/colony-2/pulse-cloudhypervisor
export GOMODCACHE="$scratch/modcache"
go mod tidy >/dev/null 2>&1 || { echo 'FAIL: external published dependency resolution'; exit 1; }
go list -m -json all > modules.json
python3 - <<'PY'
import json
text=open('modules.json').read();decoder=json.JSONDecoder();modules={}
while text.strip():
    m,n=decoder.raw_decode(text.lstrip());text=text.lstrip()[n:];assert 'Replace' not in m;modules[m['Path']]=m
for path in ['github.com/colony-2/pulse-provider-api','github.com/colony-2/pulse-provider-api/plugin']:
    assert modules[path]['Version']=='v0.0.0-20261120061856-2919f1ed5e42'
    assert 'replace ' not in open(modules[path]['GoMod']).read()
PY
go build -o "$scratch/provider" github.com/colony-2/pulse-cloudhypervisor/cmd/pulse-cloudhypervisor >/dev/null 2>&1 || { echo 'FAIL: external consumer provider build'; exit 1; }
if [[ -n "$events" ]]; then
  PROVIDER_BINARY="$scratch/provider" python3 "$collector" --events "$events" -- go test -json -count=1 -timeout 90s ./...
else
  PROVIDER_BINARY="$scratch/provider" python3 "$collector" --events "$scratch/events.json" -- go test -json -count=1 -timeout 90s ./...
fi
echo 'PASS: clean external consumer; published API/SDK pins; process and authenticated outbound hosting'
