#define _GNU_SOURCE
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static void mark(const char *stage, int batch)
{
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    printf("BLOCK %s %d %ld.%03ld\n", stage, batch, t.tv_sec, t.tv_nsec / 1000000);
}

static void mount_fs(const char *name, const char *target)
{
    mkdir(target, 0755);
    if (mount(name, target, name, 0, NULL))
        perror(target);
}

static void load_driver(const char *name)
{
    pid_t child = fork();
    if (child == 0) {
        execl("/sbin/modprobe", "modprobe", name, (char *)NULL);
        _exit(127);
    }
    int status;
    if (child > 0 && waitpid(child, &status, 0) == child && status)
        fprintf(stderr, "modprobe %s returned %d\n", name, status);
}

static int exercise(int disk, unsigned char *page)
{
    enum { page_bytes = 4096, disk_pages = 16384 };
    for (int batch = 0; batch < 12; batch++) {
        for (int i = 0; i < 128; i++) {
            unsigned int index = ((batch * 128 + i) * 8191u) % disk_pages;
            off_t offset = (off_t)index * page_bytes;
            if (pread(disk, page, page_bytes, offset) != page_bytes)
                return -1;
            if (i % 16)
                continue;
            memset(page, batch + 1, page_bytes);
            if (pwrite(disk, page, page_bytes, offset) != page_bytes || fdatasync(disk))
                return -1;
            memset(page, 0, page_bytes);
            if (pread(disk, page, page_bytes, offset) != page_bytes ||
                page[page_bytes - 1] != batch + 1)
                return -1;
        }
        mark("batch", batch);
        usleep(100000);
    }
    return 0;
}

int main(void)
{
    if (getpid() != 1)
        return 1;
    mount_fs("devtmpfs", "/dev");
    int console = open("/dev/console", O_RDWR);
    if (console < 0)
        goto stop;
    for (int i = 0; i < 3; i++)
        dup2(console, i);
    setbuf(stdout, NULL);
    mount_fs("proc", "/proc");
    mount_fs("sysfs", "/sys");
    load_driver("virtio_pci");
    load_driver("virtio_mmio");
    load_driver("virtio_blk");
    mark("ready", 0);
    int disk = open("/dev/vda", O_RDWR | O_DIRECT);
    void *page;
    if (disk >= 0 && !posix_memalign(&page, 4096, 4096) && !exercise(disk, page))
        mark("complete", 1);
    else
        mark("io-error", 0);
stop:
    reboot(RB_POWER_OFF);
    for (;;)
        pause();
}
