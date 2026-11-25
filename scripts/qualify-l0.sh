#!/usr/bin/env bash
set -uo pipefail
umask 077

machine=$(uname -m)
case "$machine" in
    aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) arch=unknown ;;
esac

run() (
    set -e
    root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
    if [[ $(uname -s) != Linux || "$arch" == unknown ]]; then
        echo "L0 qualification requires ARM64 or AMD64 Linux" >&2
        exit 1
    fi
    if [[ ${1:-} != --help && ${1:-} != -h ]]; then
        printf 'Qualification checkout '
        git -C "$root" rev-parse HEAD
        go version
    fi
    work=$(mktemp -d)
    trap 'rm -rf -- "$work"' EXIT
    export GOOS=linux GOARCH="$arch"
    go -C "$root/tests/fixture" build -mod=readonly -tags=l0 -o "$work/qualify" .
    "$work/qualify" --repo "$root" "$@"
)

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    run "$@"
    exit $?
fi

name="pulse-l0-$arch-XXXXXXXX.log"
if ! results=$(mktemp -- "$PWD/$name" 2>/dev/null); then
    tmpdir=$(cd -- "${TMPDIR:-/tmp}" && pwd -P) || exit 1
    results=$(mktemp -- "$tmpdir/$name") || exit 1
fi
printf 'Qualification results: %s\n' "$results"
finish() {
    local rc=$? outcome=FAIL
    if (( rc == 0 )); then outcome=PASS; fi
    local footer="RESULT $outcome exit=$rc finished=$(date -u +%FT%TZ)"
    if ! printf '%s\n' "$footer" >> "$results"; then
        echo "Could not finish writing $results" >&2
        rc=1
    fi
    printf '%s\nQualification results: %s\n' "$footer" "$results"
    exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

{
    printf 'L0 qualification started=%s host=%s\n' "$(date -u +%FT%TZ)" "$(uname -srm)"
    run "$@"
} 2>&1 | tee -p -- "$results"
status=("${PIPESTATUS[@]}")
if (( status[1] != 0 )); then
    echo "Could not write the full qualification output to $results" >&2
    exit "${status[1]}"
fi
exit "${status[0]}"
