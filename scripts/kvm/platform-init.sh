#!/bin/sh
# PID 1 in the scratch L1 only. The kernel opens /dev/console before starting init.
set -eu
export PATH=/bin:/sbin:/usr/bin:/usr/sbin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
exec </dev/console >/dev/console 2>&1
echo "L1 boot arguments: $(cat /proc/cmdline)"
case " $(cat /proc/cmdline) " in
    *" pulse.test_gic_traps=1 "*)
        if ! dmesg | grep -F 'GICv3 sysreg trapping enabled' | grep -F '[G0G1C]'; then
            echo 'L1 kernel did not confirm all three GIC traps'
            poweroff -f
            exit 1
        fi
        ;;
esac
mkdir -p /dev/shm /tmp
mount -t tmpfs tmpfs /dev/shm
modprobe kvm || test -c /dev/kvm
test -c /dev/kvm
if /bin/sh /kit/nested-boots.sh /kit /tmp; then
    echo 'PULSE_CH_PLATFORM_DISK_PASS'
fi
poweroff -f
