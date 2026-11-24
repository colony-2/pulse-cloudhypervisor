#!/bin/sh
# Exercise the nested VMM from the assigned c2j worker.
set -eu
kit=$1
work=$2
case "$(uname -m)" in aarch64) console=ttyAMA0 ;; x86_64) console=ttyS0 ;; *) exit 1 ;; esac
for attempt in 1 2 3; do
    disk="$work/$attempt.raw"
    serial="$work/$attempt.serial"
    dd if=/dev/zero of="$disk" bs=1 count=0 seek=67108864 2>/dev/null
    set -- "$kit/cloud-hypervisor" --kernel "$kit/Image" --initramfs "$kit/inner.initrd" \
        --cpus boot=2 --memory size=1536M --disk "path=$disk,image_type=raw" \
        --serial "file=$serial" --console off --cmdline "console=$console loglevel=4 panic=0"
    timeout -k 5 90 "$@" || { tail -20 "$serial" >&2 || true; exit 1; }
    if ! grep -q '^BLOCK complete 1 ' "$serial"; then
        tail -20 "$serial" >&2
        exit 1
    fi
    echo "PASS L2 virtio disk boot $attempt"
    rm "$disk"
done
