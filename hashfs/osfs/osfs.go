// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package osfs provides OS Filesystem access.
package osfs

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/charmbracelet/log"
	"github.com/pkg/xattr"

	"go.chromium.org/infra/build/siso/reapi/digest"
	"go.chromium.org/infra/build/siso/reapi/retry"
	"go.chromium.org/infra/build/siso/runtimex"
	"go.chromium.org/infra/build/siso/sync/semaphore"
)

// LstatSemaphore is a semaphore to control concurrent lstat,
// to protect from thread exhaustion. b/365856347
var LstatSemaphore = semaphore.New("osfs-lstat", runtimex.NumCPU()*2)

// defaultDigestXattr is default xattr for digest. http://shortn/_8GHggPD2vw
const defaultDigestXattr = "google.digest.sha256"

// OSFS provides OS Filesystem access.
// It would be an interface to communicate local filesystem server,
// in addition to local filesystem.
type OSFS struct {
	digestXattrName string
}

// Option is an option for osfs.
type Option struct {
	// DigestXattrName is xattr name for digest. When it is set, try to retrieve digest from the xattr.
	DigestXattrName string
}

func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	var xattrname string
	if xattr.XATTR_SUPPORTED {
		xattrname = defaultDigestXattr
	}
	flagSet.StringVar(&o.DigestXattrName, "fs_digest_xattr", xattrname, "xattr for sha256 digest")
}

// New creates new OSFS.
func New(name string, opt Option) *OSFS {
	if !xattr.XATTR_SUPPORTED {
		opt.DigestXattrName = ""
	}
	if opt.DigestXattrName != "" {
		log.Infof("use xattr %s for file digest", opt.DigestXattrName)
	}
	return &OSFS{
		digestXattrName: opt.DigestXattrName,
	}
}

// Chmod changes the mode of the named file to mode.
func (ofs *OSFS) Chmod(name string, mode fs.FileMode) error {
	return os.Chmod(name, mode)
}

// Chtimes changes the access and modification times of the named file.
func (ofs *OSFS) Chtimes(name string, atime, mtime time.Time) error {
	return os.Chtimes(name, atime, mtime)
}

// AsFileSource asserts digest.Source value holds FileSource type,
// and return bool whether it holds or not.
func (*OSFS) AsFileSource(ds digest.Source) (FileSource, bool) {
	s, ok := ds.(FileSource)
	return s, ok
}

// FileSource creates new FileSource for name.
// For FileDigestFromXattr, if size is non-negative, it will be used.
// If size is negative, it will check file info.
func (ofs *OSFS) FileSource(name string, size int64) FileSource {
	return FileSource{Fname: name, size: size, fs: ofs}
}

// Lstat returns a FileInfo describing the named file.
func (ofs *OSFS) Lstat(ctx context.Context, fname string) (fs.FileInfo, error) {
	var fi fs.FileInfo
	err := LstatSemaphore.Do(ctx, func() error {
		var err error
		fi, err = os.Lstat(fname)
		return err
	})
	return fi, err
}

// MkdirAll creates a directory named path, along with any necessary parents.
func (ofs *OSFS) MkdirAll(dirname string, perm fs.FileMode) error {
	return os.MkdirAll(dirname, perm)
}

// Readlink returns the destination of the named symbolic link.
func (ofs *OSFS) Readlink(name string) (string, error) {
	return os.Readlink(name)
}

// Remove removes the named file or directory.
func (ofs *OSFS) Remove(name string) error {
	return os.Remove(name)
}

// Rename renames oldpath to newpath.
func (ofs *OSFS) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// Symlink creates newname as a symbolic link to oldname.
func (ofs *OSFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

// WriteFile writes data to the named file, creating it if necessary.
func (ofs *OSFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return writeFile(name, data, perm)
}

// WriteDigestData writes digest source into the named file.
func (ofs *OSFS) WriteDigestData(ctx context.Context, name string, src digest.Source, perm fs.FileMode) error {
	err := retry.Do(ctx, func() error {
		r, err := src.Open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := openForWrite(name, perm)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		cerr := w.Close()
		if err == nil {
			err = cerr
		}
		return err
	})
	return err
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (ofs *OSFS) FileDigestFromXattr(name string, size int64) (digest.Digest, error) {
	if ofs.digestXattrName == "" {
		return digest.Digest{}, errors.ErrUnsupported
	}
	d, err := xattr.LGet(name, ofs.digestXattrName)
	if err != nil {
		return digest.Digest{}, err
	}
	if size < 0 {
		fi, err := os.Lstat(name)
		if err != nil {
			return digest.Digest{}, err
		}
		size = fi.Size()
	}
	return digest.Digest{
		Hash:      string(d),
		SizeBytes: size,
	}, nil
}

// FileSource is a file source.
type FileSource struct {
	Fname string
	size  int64
	fs    *OSFS
}

// IsLocal indicates FileSource is local file source.
func (FileSource) IsLocal() {}

// Open opens the named file for reading.
func (fsc FileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := os.Open(fsc.Fname)
	return &file{ctx: ctx, file: r, started: time.Now(), fs: fsc.fs}, err
}

func (fsc FileSource) String() string {
	return fmt.Sprintf("file://%s", fsc.Fname)
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (fsc FileSource) FileDigestFromXattr(ctx context.Context) (digest.Digest, error) {
	return fsc.fs.FileDigestFromXattr(fsc.Fname, fsc.size)
}

type file struct {
	ctx     context.Context
	file    *os.File
	started time.Time
	fs      *OSFS
	n       int
}

func (f *file) Read(buf []byte) (int, error) {
	n, err := f.file.Read(buf)
	f.n += n
	return n, err
}

func (f *file) Close() error {
	return f.file.Close()
}
