#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
if [[ $(uname -s) != Linux ]]; then echo "L0 qualification requires Linux" >&2; exit 1; fi
case $(uname -m) in
    aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) echo "L0 qualification requires ARM64 or AMD64" >&2; exit 1 ;;
esac
umask 077
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
export GOOS=linux GOARCH="$arch"
go -C "$root/tests/fixture" build -mod=readonly -tags=l0 -o "$work/qualify" .
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    "$work/qualify" --help
else
    log=$(mktemp "${TMPDIR:-/tmp}/pulse-l0-$arch.XXXXXXXX.log")
    echo "Qualification log: $log"
    "$work/qualify" --repo "$root" "$@" 2>&1 | tee "$log"
fi
