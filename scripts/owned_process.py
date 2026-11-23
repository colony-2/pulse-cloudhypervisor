"""Supervise one acceptance subprocess tree without touching unrelated processes."""
import ctypes
import os
import pathlib
import signal
import subprocess
import sys
import time

from verification import require


def children():
    return {int(pid) for pid in pathlib.Path(f'/proc/self/task/{os.getpid()}/children').read_text().split()}


def signal_child(pid, signum):
    try:
        descriptor = os.pidfd_open(pid)
    except ProcessLookupError:
        return
    try:
        if pid in children():
            signal.pidfd_send_signal(descriptor, signum)
    except ProcessLookupError:
        pass
    finally:
        os.close(descriptor)


def sweep(child, signum, until):
    while True:
        child.poll()
        owned = children()
        if not owned and child.returncode is not None:
            return True
        if time.monotonic() >= until:
            return False
        for pid in owned:
            if pid != child.pid and child.returncode is not None:
                try:
                    if os.waitpid(pid, os.WNOHANG)[0] == pid:
                        continue
                except ChildProcessError:
                    continue
            signal_child(pid, signum)
        time.sleep(0.02)


def run(command, timeout, grace=2):
    require(hasattr(os, 'pidfd_open') and hasattr(signal, 'pidfd_send_signal') and not children())
    libc = ctypes.CDLL(None, use_errno=True)
    previous = ctypes.c_int()
    require(libc.prctl(37, ctypes.byref(previous), 0, 0, 0) == 0)
    require(libc.prctl(36, 1, 0, 0, 0) == 0)
    stop_signal = None

    def cancel(signum, _frame):
        nonlocal stop_signal
        stop_signal = signum

    previous_handlers = {signum: signal.signal(signum, cancel) for signum in (signal.SIGTERM, signal.SIGINT)}
    try:
        child = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        limit = time.monotonic() + timeout
        try:
            while stop_signal is None and time.monotonic() < limit:
                if child.poll() is not None:
                    break
                time.sleep(0.02)
            clean_exit = stop_signal is None and child.poll() == 0 and time.monotonic() < limit and not children()
        finally:
            reaped = sweep(child, signal.SIGTERM, time.monotonic() + grace)
            if not reaped:
                reaped = sweep(child, signal.SIGKILL, time.monotonic() + grace)
        return clean_exit and reaped and stop_signal is None, reaped
    finally:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)
        libc.prctl(36, previous.value, 0, 0, 0)


def isolated_run(command, timeout):
    # Keep the JobDB helper and network fixture outside the child-subreaper.
    supervisor = [sys.executable, __file__, str(timeout), *command]
    result = subprocess.run(supervisor, check=False)
    return result.returncode == 0, result.returncode in (0, 1)


if __name__ == '__main__':
    try:
        passed, cleaned = run(sys.argv[2:], float(sys.argv[1]))
    except (OSError, ValueError, subprocess.SubprocessError):
        sys.exit(2)
    sys.exit(0 if passed else 1 if cleaned else 2)
