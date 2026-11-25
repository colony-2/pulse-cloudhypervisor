// Package state stores only credential-free ownership and execution fences.
package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrUnsafe = errors.New("state: unsafe ownership, permissions or path")
var ErrCorrupt = errors.New("state: corrupt or unsupported record")
var ErrStorage = errors.New("state: durable write failed")

// PrivateDir creates only missing owned directories. Existing ancestors are
// inspected, never chmodded; symlinks and writable nonsticky ancestors fail.
func PrivateDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return ErrUnsafe
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, e := os.Lstat(current)
		if os.IsNotExist(e) {
			if e = os.Mkdir(current, 0700); e != nil && !os.IsExist(e) {
				return ErrUnsafe
			}
			info, e = os.Lstat(current)
			// Persist every new ancestor before a launch record can authorize dispatch.
			parent, err := os.Open(filepath.Dir(current))
			if err != nil {
				return ErrStorage
			}
			err = parent.Sync()
			parent.Close()
			if err != nil {
				return ErrStorage
			}
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafe
		}
		var s unix.Stat_t
		if unix.Lstat(current, &s) != nil {
			return ErrUnsafe
		}
		if s.Uid != 0 && s.Uid != uint32(os.Geteuid()) {
			return ErrUnsafe
		}
		if current == path {
			if s.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0700 {
				return ErrUnsafe
			}
		} else if info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return ErrUnsafe
		}
	}
	return nil
}

func OpenPrivate(path string, flags int) (*os.File, error) {
	fd, e := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, ErrUnsafe
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0600 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		f.Close()
		return nil, ErrUnsafe
	}
	return f, nil
}
func Lock(path string) (*os.File, error) {
	f, e := OpenPrivate(path, unix.O_CREAT|unix.O_RDWR)
	if e != nil {
		return nil, e
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("state: owner already locked")
	}
	return f, nil
}

// ExistingLock observes an existing runner lock without creating filesystem state.
func ExistingLock(path string) (*os.File, error) {
	f, e := OpenPrivate(path, unix.O_RDWR)
	if e != nil {
		return nil, e
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, ErrUnsafe
	}
	return f, nil
}

// Atomic writes with durable rename. Hook supports boundary fault/crash tests;
// an error after rename is uncertain and callers must retain the reservation.
func Atomic(path string, value any, hook func(string) error) error {
	data, e := json.Marshal(value)
	if e != nil {
		return ErrStorage
	}
	return AtomicBytes(path, data, hook)
}

func AtomicBytes(path string, data []byte, hook func(string) error) error {
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() {
			return ErrUnsafe
		}
		f, e := OpenPrivate(path, unix.O_RDONLY)
		if e != nil {
			return e
		}
		f.Close()
	} else if !os.IsNotExist(e) {
		return ErrStorage
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".write-")
	if e != nil {
		return ErrStorage
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	boundary := func(s string) error {
		if hook != nil && hook(s) != nil {
			return ErrStorage
		}
		return nil
	}
	if e = f.Chmod(0600); e != nil {
		return ErrStorage
	}
	if _, e = f.Write(data); e != nil {
		return ErrStorage
	}
	if boundary("write") != nil {
		return ErrStorage
	}
	if e = f.Sync(); e != nil {
		return ErrStorage
	}
	if boundary("file-sync") != nil {
		return ErrStorage
	}
	if e = f.Close(); e != nil {
		return ErrStorage
	}
	if e = os.Rename(name, path); e != nil {
		return ErrStorage
	}
	if boundary("rename") != nil {
		return ErrStorage
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return ErrStorage
	}
	defer d.Close()
	if e = d.Sync(); e != nil {
		return ErrStorage
	}
	return boundary("directory-sync")
}
func Read(path string, value any) error {
	f, e := OpenPrivate(path, unix.O_RDONLY)
	if e != nil {
		return e
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 1<<20 {
		return ErrCorrupt
	}
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return ErrCorrupt
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrCorrupt
	}
	return nil
}

// Fence uses O_EXCL and fsync. A failed creation is never permission to retry.
func Fence(path string) error {
	f, e := OpenPrivate(path, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY)
	if e != nil {
		return e
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrStorage
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return ErrStorage
	}
	defer d.Close()
	if d.Sync() != nil {
		return ErrStorage
	}
	return nil
}
