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

// Clone never opens an existing destination or shares a writable source.
// The fallback preserves sparse extents. Callers reserve the full virtual size.
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
		if e = copySparse(dst, src, st.Size()); e != nil {
			return ErrImage
		}
	}
	if dst.Sync() != nil {
		return ErrImage
	}
	ok = true
	return nil
}

func copySparse(dst, src *os.File, size int64) error {
	for pos := int64(0); pos < size; {
		start, err := unix.Seek(int(src.Fd()), pos, unix.SEEK_DATA)
		if err == unix.ENXIO {
			break
		}
		if err == unix.EINVAL || err == unix.EOPNOTSUPP {
			_, err = io.CopyN(dst, io.NewSectionReader(src, pos, size-pos), size-pos)
			return err
		}
		if err != nil {
			return err
		}
		end, err := unix.Seek(int(src.Fd()), start, unix.SEEK_HOLE)
		if err != nil || end <= start || end > size {
			return ErrImage
		}
		if _, err = dst.Seek(start, io.SeekStart); err != nil {
			return err
		}
		if _, err = io.CopyN(dst, io.NewSectionReader(src, start, end-start), end-start); err != nil {
			return err
		}
		pos = end
	}
	return dst.Truncate(size)
}
