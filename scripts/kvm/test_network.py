"""Regression checks for cleanup of the transient nested peer."""
import subprocess
import unittest
from unittest.mock import patch

from network import peer_service


class PeerServiceTest(unittest.TestCase):
    def test_peer_exit_before_readiness_preserves_boot_error(self):
        # The peer may exit before poll or between poll and systemctl stop.
        for polled in (1, None):
            with self.subTest(polled=polled), patch("network.subprocess.Popen") as popen, patch("network.subprocess.run") as stop:
                popen.return_value.__enter__.return_value.poll.return_value = polled
                stop.return_value = subprocess.CompletedProcess([], 5, "", "unit not loaded")
                with self.assertRaisesRegex(TimeoutError, "peer readiness"):
                    with peer_service(["systemd-run"], "peer.service"):
                        raise TimeoutError("peer readiness")
                self.assertEqual(stop.called, polled is None)

    def test_running_peer_must_stop_after_success(self):
        with patch("network.subprocess.Popen") as popen, patch("network.subprocess.run") as stop:
            popen.return_value.__enter__.return_value.poll.return_value = None
            stop.return_value = subprocess.CompletedProcess([], 1, "", "access denied")
            with self.assertRaisesRegex(ValueError, "access denied"):
                with peer_service(["systemd-run"], "peer.service"):
                    pass


if __name__ == "__main__":
    unittest.main()
