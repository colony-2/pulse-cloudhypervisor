package state

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAtomicBoundariesAndCrashes(t *testing.T) {
	if dir := os.Getenv("PULSE_STORAGE_CRASH_DIR"); dir != "" {
		point := os.Getenv("PULSE_STORAGE_CRASH_POINT")
		_ = Atomic(filepath.Join(dir, "record.json"), map[string]int{"reserved": 2}, func(s string) error {
			if s == point {
				os.Exit(91)
			}
			return nil
		})
		os.Exit(92)
	}
	for _, point := range []string{"write", "file-sync", "rename", "directory-sync"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "record.json")
			if e := Atomic(path, map[string]int{"reserved": 1}, nil); e != nil {
				t.Fatal(e)
			}
			executable, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(executable, "-test.run=^TestAtomicBoundariesAndCrashes$")
			cmd.Env = []string{"PULSE_STORAGE_CRASH_DIR=" + dir, "PULSE_STORAGE_CRASH_POINT=" + point}
			e = cmd.Run()
			var x *exec.ExitError
			if !errors.As(e, &x) || x.ExitCode() != 91 {
				t.Fatal("fault subprocess did not crash at boundary")
			}
			var record map[string]int
			if e = Read(path, &record); e != nil {
				t.Fatal(e)
			}
			want := 1
			if point == "rename" || point == "directory-sync" {
				want = 2
			}
			if record["reserved"] != want {
				t.Fatal("atomic reservation lost")
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				info, _ := entry.Info()
				if info.Mode().Perm() != 0600 {
					t.Fatal("interrupted temporary exposed")
				}
			}
		})
	}
}
func TestFaultsPreserveReadablePriorRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if Atomic(path, map[string]int{"n": 1}, nil) != nil {
		t.Fatal("initial record")
	}
	for _, point := range []string{"write", "file-sync"} {
		e := Atomic(path, map[string]int{"n": 2}, func(s string) error {
			if point == s {
				return errors.New("injected")
			}
			return nil
		})
		if e == nil {
			t.Fatal("injection missed")
		}
		var v map[string]int
		if Read(path, &v) != nil || v["n"] != 1 {
			t.Fatal("previous record damaged")
		}
	}
}
func TestPrivateModesUnderPermissiveUmask(t *testing.T) {
	root := t.TempDir()
	old := unix.Umask(0)
	defer unix.Umask(old)
	dir := filepath.Join(root, "owner")
	if PrivateDir(dir) != nil {
		t.Fatal("private dir")
	}
	f, e := Lock(filepath.Join(dir, "lock"))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if Atomic(filepath.Join(dir, "record"), map[string]int{"n": 1}, nil) != nil {
		t.Fatal("atomic")
	}
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, e error) error {
		if e != nil {
			t.Fatal(e)
		}
		info, _ := d.Info()
		want := os.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal("unsafe mode")
		}
		return nil
	})
}
func TestRejectSymlinkHardlinkForeignOwnershipAndCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	Atomic(path, map[string]int{"n": 1}, nil)
	link := filepath.Join(dir, "link")
	os.Symlink(path, link)
	var v map[string]int
	if Read(link, &v) == nil || Atomic(link, v, nil) == nil {
		t.Fatal("symlink accepted")
	}
	hard := filepath.Join(dir, "hard")
	os.Link(path, hard)
	if Read(hard, &v) == nil {
		t.Fatal("hardlink accepted")
	}
	os.Remove(hard)
	if os.Geteuid() == 0 {
		if os.Chown(path, 12345, 12345) != nil {
			t.Fatal("foreign fixture")
		}
		if Read(path, &v) == nil {
			t.Fatal("foreign owner accepted")
		}
		os.Chown(path, 0, 0)
	}
	os.WriteFile(path, []byte(`{"n":`), 0600)
	if !errors.Is(Read(path, &v), ErrCorrupt) {
		t.Fatal("corrupt record accepted")
	}
	symlink := filepath.Join(dir, "owner-link")
	os.Symlink(dir, symlink)
	if PrivateDir(symlink) == nil {
		t.Fatal("symlink owner accepted")
	}
}
func TestFenceAndOwnerLockNeverReenter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fence")
	if Fence(path) != nil || Fence(path) == nil {
		t.Fatal("fence replay")
	}
	f, e := Lock(filepath.Join(dir, "owner"))
	if e != nil {
		t.Fatal(e)
	}
	if second, e := Lock(filepath.Join(dir, "owner")); e == nil {
		second.Close()
		t.Fatal("owner lock duplicated")
	}
	f.Close()
	f, e = ExistingLock(filepath.Join(dir, "owner"))
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = ExistingLock(filepath.Join(dir, "missing")); e == nil {
		t.Fatal("observation created lock")
	}
	if _, e = os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(e) {
		t.Fatal("missing lock mutated")
	}
}
