#!/bin/sh
# Shared by the disposable L1 platform probe and the actual c2j worker.
set -eu
kit=$1
work=$2
for attempt in 1 2 3; do
    disk="$work/$attempt.raw"
    serial="$work/$attempt.serial"
    dd if=/dev/zero of="$disk" bs=1 count=0 seek=67108864 2>/dev/null
    set -- "$kit/cloud-hypervisor" --kernel "$kit/Image" --initramfs "$kit/inner.initrd" \
        --cpus boot=2 --memory size=1536M --disk "path=$disk,image_type=raw" \
        --serial "file=$serial" --console off --cmdline 'console=ttyAMA0 loglevel=4 panic=0'
    # PID 1 in the diagnostic L1 also has an external deadline on the L0 host.
    if command -v timeout >/dev/null 2>&1; then set -- timeout -k 5 90 "$@"; fi
    "$@" || { tail -20 "$serial" >&2 || true; exit 1; }
    if ! grep -q '^BLOCK complete 1 ' "$serial"; then
        tail -20 "$serial" >&2
        exit 1
    fi
    echo "PASS L2 virtio disk boot $attempt"
    rm "$disk"
done
