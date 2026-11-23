//go:build live

package live

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
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
	"github.com/colony-2/pulse-cloudhypervisor/internal/runner"
	"github.com/colony-2/pulse-cloudhypervisor/internal/state"
	api "github.com/colony-2/pulse-provider-api"
)

type preparedMarker struct {
	Version                   int
	Fingerprint, Path, Digest string
	Proof                     guest.Proof
}

type imageObservation struct {
	fixture fixture
	base    ch.Base
	device  string
	marker  preparedMarker
}

func observePreparedImage(t *testing.T) imageObservation {
	t.Helper()
	f, cfg, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil || len(cfg.Bases) != 1 || !preparedDisk(cfg, cfg.Bases[0]) {
		t.Fatal("measured, stopped, immutable source required")
	}
	b := cfg.Bases[0]
	var marker preparedMarker
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
	return imageObservation{f, b, marker.Path + "?offset=" + strconv.FormatUint(first*512, 10), marker}
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
	if o.base.Tools["c2j"] != c2jSHA || o.fileHash(t, c2jPath) != c2jSHA || !reflect.DeepEqual(o.marker.Proof.Tools, o.base.Tools) ||
		o.marker.Proof.Ubuntu != "24.04" || o.marker.Proof.Architecture != "arm64" || o.marker.Proof.Recipe != o.base.Recipe ||
		!o.marker.Proof.Docker || o.marker.Proof.DockerImage != o.base.DockerFixtureDigest || o.marker.Proof.CPUMillis < o.base.UsableCPUMillis ||
		o.marker.Proof.MemoryBytes < o.base.UsableMemoryBytes {
		t.Fatal("measured source failed pinned guest tool, Docker or usable-resource qualification")
	}
}

func TestS12(t *testing.T) {
	// Each failed builder has its own writable clone. The agent synchronizes a
	// unique marker there before the injected fault, so unrelated boot failures
	// cannot satisfy the rejection assertions.
	_, cfg, err := loadFixture(os.Getenv("CLOUDHYPERVISOR_FIXTURE"))
	if err != nil || len(cfg.Bases) != 1 {
		t.Fatal("measured native fixture required")
	}
	original := cfg.Bases[0]
	o := observePreparedImage(t)
	source := o.marker.Path
	sources := filepath.Join(cfg.StateDir, "sources")
	for _, fault := range []string{"tool", "image", "usable-memory", "worker-intent"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if !preparedDisk(cfg, original) {
				t.Fatal("prior usable source unavailable")
			}
			b := original
			b.Tools = maps.Clone(original.Tools)
			var prior preparedMarker
			if state.Read(filepath.Join(sources, ch.Fingerprint(original)+".json"), &prior) != nil || prior.Path != source || prior.Digest != o.marker.Digest {
				t.Fatal("read qualified source identity")
			}
			b.Disk = ch.Asset{Path: source, SHA256: prior.Digest}
			marker := "/var/lib/pulse-guest/native-negative-" + fault
			script := "set -eu\nprintf observed > " + marker + "\nsync -f " + marker + "\n"
			switch fault {
			case "tool":
				b.Tools["nix"] = strings.Repeat("0", 64)
			case "image":
				b.DockerFixtureDigest = "sha256:" + strings.Repeat("0", 64)
			case "usable-memory":
				b.UsableMemoryBytes = cfg.MemoryBytes - 1
			case "worker-intent":
				script += "touch /var/lib/pulse-guest/worker-intent\nsync -f /var/lib/pulse-guest/worker-intent\n"
			}
			sum := sha256.Sum256([]byte(script))
			recipe := filepath.Join(t.TempDir(), "prepare.sh")
			if os.WriteFile(recipe, []byte(script), 0600) != nil {
				t.Fatal("write negative preparation")
			}
			b.Recipe = hex.EncodeToString(sum[:])
			b.Preparation = ch.Asset{Path: recipe, SHA256: b.Recipe}
			alternate := cfg
			alternate.Bases = []ch.Base{b}
			provider, err := ch.New(alternate)
			if err != nil {
				t.Fatal("open test builder owner")
			}
			defer provider.Close()
			if err := provider.Setup(ctx); !errors.Is(err, ch.ErrPreparation) || ctx.Err() != nil {
				t.Fatal("fault did not reject a terminated builder")
			}
			entries, err := filepath.Glob(filepath.Join(sources, "build-*"))
			if err != nil || len(entries) != 1 {
				t.Fatal("failed builder evidence unavailable")
			}
			spec, err := runner.ReadSpec(entries[0])
			if err != nil || spec.Owner != cfg.Owner || spec.LaunchID != "prepare" || spec.SourceDigest != prior.Digest {
				t.Fatal("negative builder ownership")
			}
			disk := filepath.Join(entries[0], "disk.raw")
			var actual bytes.Buffer
			imageObservation{device: disk + "?offset=1048576"}.read(t, "cat "+marker, &actual)
			if actual.String() != "observed" {
				t.Fatal("fault was not reached inside the guest")
			}
			promoted, err := filepath.Glob(filepath.Join(sources, ch.Fingerprint(b)+"*"))
			if err != nil || len(promoted) != 0 || !preparedDisk(cfg, original) {
				t.Fatal("invalid preparation affected source publication")
			}
			if provider.Reconcile(ctx) != nil {
				t.Fatal("failed builder reconciliation")
			}
			if _, err := os.Lstat(entries[0]); !os.IsNotExist(err) {
				t.Fatal("failed builder still reserved")
			}
		})
	}
}

func TestS05(t *testing.T) {
	e := openExecution(t)
	l, before := e.launch(t, "prepared-cold-boot", "valid", "prepared-tools")
	// First deliver to an independently locked, empty owner. Admission must
	// decline immediately and leave this same lease usable on the qualified owner.
	cfg := e.config
	cfg.Owner = "missing-prepared-source"
	dir, err := os.MkdirTemp("/tmp", "ch-unprepared-")
	if err != nil {
		t.Fatal("create cold owner")
	}
	cfg.StateDir = dir
	cold, err := ch.New(cfg)
	if err != nil {
		t.Fatal("open cold owner")
	}
	defer cold.Close()
	results, sub, err := cold.Submit(e.ctx, []api.Launch{l}, api.SubmitOptions{})
	if err != nil || sub != nil || len(results) != 1 || results[0].Status != api.NoCapacity || results[0].LaunchID != l.LaunchID {
		t.Fatal("unprepared owner did not decline this assignment")
	}
	for _, name := range []string{"jobs", "sources"} {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		if (err != nil && !os.IsNotExist(err)) || len(entries) != 0 {
			t.Fatal("admission attempted startup or image repair")
		}
	}
	if cold.Close() != nil || os.RemoveAll(dir) != nil {
		t.Fatal("close empty owner")
	}
	out := new(guestOutput)
	observation := e.submit(t, l, out.listen(e.ctx, l.LaunchID))
	after := e.finish(t, l, observation, out, true)
	if before.Job != after.Job || !after.OutputOK || after.Outcome != "success" {
		t.Fatal("cold guest did not use its installed, pinned tools")
	}
}
