"""Regression tests for setup's host-dependent defaults and reservations."""

import gzip
import hashlib
import ipaddress
import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent / "setup"))
import host
import network
import assets


class HostSetup(unittest.TestCase):
    def test_assigns_the_first_address_to_a_new_bridge(self):
        gateway = "10.64.0.1/24"
        for links in [[], [{"addr_info": []}]]:
            with (
                mock.patch.object(network, "ips", return_value=links),
                mock.patch.object(network, "run") as run,
            ):
                network.ensure_gateway("testbr", gateway)
                run.assert_called_once_with("ip", "addr", "add", gateway, "dev", "testbr")
        links = [{"addr_info": [{"local": "10.64.0.1", "prefixlen": 24}]}]
        with (
            mock.patch.object(network, "ips", return_value=links),
            mock.patch.object(network, "run") as run,
        ):
            network.ensure_gateway("testbr", gateway)
            with self.assertRaisesRegex(ValueError, "different address"):
                network.ensure_gateway("testbr", "10.64.0.2/24")
            run.assert_not_called()

    def test_both_native_images_are_pinned_by_the_imported_oci_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            layer = root / "rootfs.tar.gz"
            with tarfile.open(layer, "w:gz") as archive:
                archive.addfile(tarfile.TarInfo("etc/fixture"))
            for arch in ("arm64", "amd64"):
                image = root / (arch + ".tar")
                pinned = assets.docker_image(arch, layer, image)
                with tarfile.open(image) as archive:

                    def read_blob(descriptor):
                        digest = descriptor["digest"].removeprefix("sha256:")
                        data = archive.extractfile("blobs/sha256/" + digest).read()
                        self.assertEqual(hashlib.sha256(data).hexdigest(), digest)
                        self.assertEqual(len(data), descriptor["size"])
                        return data

                    (descriptor,) = json.load(archive.extractfile("index.json"))["manifests"]
                    self.assertEqual(pinned, descriptor["digest"])
                    manifest = json.loads(read_blob(descriptor))
                    config = json.loads(read_blob(manifest["config"]))
                    self.assertEqual(config["architecture"], arch)
                    layer_data = gzip.decompress(read_blob(manifest["layers"][0]))
                    self.assertEqual(
                        config["rootfs"]["diff_ids"], ["sha256:" + hashlib.sha256(layer_data).hexdigest()]
                    )

    def test_machine_limits_and_explicit_overrides(self):
        gib = host.GiB
        for cpu, memory, free, slots, disk in [(16, 20, 100, 4, 8), (4, 8, 9, 1, 3), (2, 4, 18, 1, 4)]:
            c = {}
            self.assertEqual(host.size(c, set(), cpu, memory * gib, free * gib), slots)
            self.assertLess(c["host_memory_bytes"], memory * gib)
            self.assertEqual(c["vcpus"], min(cpu, 4))
            self.assertEqual(c["disk_bytes"], disk * gib)
        c = dict(
            count=8,
            total_capacity=2,
            default_worker_cpu_millis=6000,
            default_worker_memory_bytes=5 * gib,
            host_cpu_millis=12000,
        )
        self.assertEqual(host.size(c, set(c), 16, 32 * gib, 100 * gib), 2)
        self.assertEqual(c["host_cpu_millis"], 12000)
        self.assertGreaterEqual(c["vcpus"], 6)
        self.assertGreaterEqual(c["memory_bytes"] * 3 // 4, 5 * gib)
        with self.assertRaisesRegex(ValueError, "fit"):
            host.size(dict(host_cpu_millis=0), {"host_cpu_millis"}, 4, 8 * gib, 32 * gib)
        for c in [dict(count=0), dict(total_capacity=0)]:
            self.assertEqual(host.size(c, set(), 4, 8 * gib, 32 * gib), 0)
            self.assertGreater(c["host_disk_bytes"], 0)

    def test_bare_subnet_avoids_existing_routes_and_addresses(self):
        first = network.choose_subnet([], [], "192a0d00", 1, "test")
        addr = dict(
            ifname="foreign",
            addr_info=[
                dict(local=str(first.network_address + 6), prefixlen=first.prefixlen, family="inet")
            ],
        )
        second = network.choose_subnet([], [addr], "192a0d00", 1, "test")
        self.assertFalse(first.overlaps(second))
        many = network.choose_subnet([dict(dev="other", dst=str(second))], [addr], "192a0d00", 700, "test")
        self.assertGreaterEqual(many.num_addresses, 703)
        self.assertFalse(first.overlaps(many) or second.overlaps(many))

    def test_allocation_taps_reuse_addresses_across_owners(self):
        links = {}

        def create(name, _kind, _uid, alias):
            if name in links:
                self.assertEqual(links[name]["ifalias"], alias)
            links[name] = dict(ifname=name, ifalias=alias)

        args = list(map(ipaddress.IPv4Address, ["10.0.0.2", "10.0.0.4", "10.0.0.1"]))
        with (
            mock.patch.object(network, "ips", side_effect=lambda *_: list(links.values())),
            mock.patch.object(network, "owned_link", side_effect=create),
            mock.patch.object(network, "run"),
        ):
            first = network.allocation_taps("pulsebr0", "00000001", 1000, 1, *args)
            second = network.allocation_taps("pulsebr0", "00000002", 1000, 2, *args)
            self.assertFalse(set(first) & set(second))
            self.assertEqual(second, network.allocation_taps("pulsebr0", "00000002", 1000, 2, *args))
            with self.assertRaisesRegex(ValueError, "no free"):
                network.allocation_taps("pulsebr0", "00000003", 1000, 1, *args)


if __name__ == "__main__":
    unittest.main()
