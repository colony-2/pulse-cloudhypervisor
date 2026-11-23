// Package images verifies immutable assets and creates private writable copies.
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

var ErrImage = errors.New("image: unavailable or digest mismatch")

func Verify(path, digest string) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrImage
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return ErrImage
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil || hex.EncodeToString(h.Sum(nil)) != digest {
		return ErrImage
	}
	return nil
}

// Clone never opens an existing destination or shares a writable source. When
// reflink is unsupported a full copy is used; callers reserve the full size.
func Clone(source, dest string, limit int64) error {
	src, e := os.Open(source)
	if e != nil {
		return ErrImage
	}
	defer src.Close()
	st, e := src.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return ErrImage
	}
	fd, e := unix.Open(dest, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return ErrImage
	}
	dst := os.NewFile(uintptr(fd), dest)
	ok := false
	defer func() {
		dst.Close()
		if !ok {
			os.Remove(dest)
		}
	}()
	if e = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); e != nil {
		if _, e = io.Copy(dst, src); e != nil {
			return ErrImage
		}
	}
	if dst.Sync() != nil {
		return ErrImage
	}
	ok = true
	return nil
}
