//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	ch "github.com/colony-2/pulse-cloudhypervisor"
	"github.com/colony-2/pulse-cloudhypervisor/internal/guest"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
)

type imageObservation struct {
	fixture fixture
	base    ch.Base
	device  string
	proof   guest.Proof
}

func observePreparedImage(t *testing.T) imageObservation {
	t.Helper()
	f, cfg, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil || len(cfg.Bases) != 1 || !preparedDisk(cfg, cfg.Bases[0]) {
		t.Fatal("measured, stopped, immutable source required")
	}
	b := cfg.Bases[0]
	var marker struct {
		Version                   int
		Fingerprint, Path, Digest string
		Proof                     guest.Proof
	}
	if state.Read(filepath.Join(cfg.StateDir, "sources", ch.Fingerprint(b)+".json"), &marker) != nil {
		t.Fatal("read source proof")
	}
	file, err := os.Open(marker.Path)
	if err != nil {
		t.Fatal("open prepared disk read-only")
	}
	defer file.Close()
	var headers [1536]byte
	if _, err := io.ReadFull(file, headers[:]); err != nil || string(headers[512:520]) != "EFI PART" ||
		binary.LittleEndian.Uint64(headers[584:592]) != 2 || binary.LittleEndian.Uint32(headers[596:600]) != 128 {
		t.Fatal("prepared disk partition table")
	}
	first := binary.LittleEndian.Uint64(headers[1056:1064])
	last := binary.LittleEndian.Uint64(headers[1064:1072])
	if first != 2048 || last <= first || last >= uint64(cfg.DiskBytes/512) {
		t.Fatal("unexpected prepared root partition")
	}
	// libext2fs opens a partition inside a regular file using ?offset=.
	// debugfs defaults to read-only and needs no loop device or mount.
	return imageObservation{f, b, marker.Path + "?offset=" + strconv.FormatUint(first*512, 10), marker.Proof}
}

func (o imageObservation) read(t *testing.T, command string, output io.Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var diagnostic bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/sbin/debugfs", "-R", command, o.device)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.Stdout, cmd.Stderr = output, &diagnostic
	// debugfs can return zero even on lookup failure. Require only its version banner.
	if err := cmd.Run(); err != nil || !strings.HasPrefix(diagnostic.String(), "debugfs ") || strings.Count(diagnostic.String(), "\n") != 1 {
		t.Fatal("read prepared ext4 contents")
	}
}

func (o imageObservation) fileHash(t *testing.T, path string) string {
	t.Helper()
	h := sha256.New()
	o.read(t, "cat "+path, h)
	return hex.EncodeToString(h.Sum(nil))
}

func TestS04(t *testing.T) {
	o := observePreparedImage(t)
	unit, err := os.ReadFile("../../guest/pulse-cloudhypervisor-guest.service")
	if err != nil {
		t.Fatal("read candidate guest unit")
	}
	hash := sha256.Sum256(unit)
	if o.fileHash(t, "/usr/local/bin/pulse-cloudhypervisor-guest") != o.fixture.BootstrapAgent.SHA256 ||
		o.fileHash(t, "/etc/systemd/system/pulse-cloudhypervisor-guest.service") != hex.EncodeToString(hash[:]) {
		t.Fatal("installed agent or unit differs from the candidate")
	}
	var data bytes.Buffer
	o.read(t, "cat /etc/passwd", &data)
	if !strings.Contains("\n"+data.String(), "\nworker:!:1000:1000:Pulse worker:/home/worker:/bin/sh\n") {
		t.Fatal("locked worker account missing from stopped source")
	}
	data.Reset()
	o.read(t, "ls -p /home", &data)
	if !strings.Contains(data.String(), "/040700/1000/1000/worker//") {
		t.Fatal("private worker home ownership")
	}
	data.Reset()
	o.read(t, "ls -p /var/lib/pulse-guest", &data)
	var entries []string
	for _, line := range strings.Split(strings.TrimSpace(data.String()), "\n") {
		fields := strings.Split(line, "/")
		if len(fields) < 7 {
			t.Fatal("read worker intent directory")
		}
		entries = append(entries, fields[5])
	}
	if !reflect.DeepEqual(entries, []string{".", ".."}) {
		t.Fatal("prepared source retained worker state")
	}
}

func TestS11(t *testing.T) {
	o := observePreparedImage(t)
	var pins struct {
		Common struct {
			C2JRevision string `json:"c2j_revision"`
		} `json:"common"`
	}
	bytes, err := os.ReadFile(os.Getenv("CLOUDHYPERVISOR_PINS"))
	if err != nil || json.Unmarshal(bytes, &pins) != nil || pins.Common.C2JRevision != "3991db6930ec62fb0bf85e4da824d6a8fc4fdfa7" {
		t.Fatal("required full c2j revision unavailable")
	}
	const c2jPath = "/nix/store/lfnk1pwadjcalsk19ayl8f333lmp5jvm-c2j-unstable-3991db6/bin/c2j"
	const c2jSHA = "8fbf660e09492daac03ed6f753bfeb345e5c235b52c8ba37788c7740a3cc89af"
	if o.base.Tools["c2j"] != c2jSHA || o.fileHash(t, c2jPath) != c2jSHA || !reflect.DeepEqual(o.proof.Tools, o.base.Tools) ||
		o.proof.Ubuntu != "24.04" || o.proof.Architecture != "arm64" || o.proof.Recipe != o.base.Recipe ||
		!o.proof.Docker || o.proof.DockerImage != o.base.DockerFixtureDigest || o.proof.CPUMillis < o.base.UsableCPUMillis ||
		o.proof.MemoryBytes < o.base.UsableMemoryBytes {
		t.Fatal("measured source failed pinned guest tool, Docker or usable-resource qualification")
	}
}
