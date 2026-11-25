"""Regression checks for the physical-host qualification results file."""
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


ENTRY = Path(__file__).resolve().parents[1] / "qualify-l0.sh"


class QualificationResults(unittest.TestCase):
    def test_each_attempt_retains_output_and_exit_status(self):
        with tempfile.TemporaryDirectory(prefix="qualification results ") as temp:
            work = Path(temp)
            go = work / "go"
            go.write_text("""#!/bin/sh
set -eu
case "$1" in
    env) echo "go1.26.8" ;;
    version) echo "test toolchain" ;;
    -C)
        echo "compiler stderr" >&2
        if [ "$BUILD_EXIT" != 0 ]; then exit "$BUILD_EXIT"; fi
        while [ "$1" != -o ]; do shift; done
        cp -p "$0" "$2" ;;
    *)
        echo "probe stdout"
        echo "probe stderr" >&2
        exit "$PROBE_EXIT" ;;
esac
""")
            go.chmod(0o700)
            saved = {}
            for build, probe in [(19, 0), (0, 23), (0, 0)]:
                with self.subTest(build=build, probe=probe):
                    env = {**os.environ, "PATH": f"{work}:{os.environ['PATH']}",
                           "BUILD_EXIT": str(build), "PROBE_EXIT": str(probe)}
                    result = subprocess.run([ENTRY], cwd=work, env=env,
                                            capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, build or probe)
                    path, = set(work.glob("pulse-l0-*-????????.log")) - saved.keys()
                    self.assertEqual(result.stdout.count(str(path)), 2)
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
                    report = path.read_text()
                    self.assertIn("compiler stderr", report)
                    if not build:
                        self.assertIn("probe stdout", report)
                        self.assertIn("probe stderr", report)
                    outcome = "FAIL" if result.returncode else "PASS"
                    self.assertIn(f"RESULT {outcome} exit={result.returncode} ", report.splitlines()[-1])
                    saved[path] = report
            result = subprocess.run([ENTRY], cwd="/proc", env={**env, "TMPDIR": str(work)},
                                    capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0)
            fallback, = set(work.glob("pulse-l0-*-????????.log")) - saved.keys()
            self.assertIn(str(fallback), result.stdout)
            for path, original in saved.items():
                self.assertEqual(path.read_text(), original)


if __name__ == "__main__":
    unittest.main()
