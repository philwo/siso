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
	"runtime"
	"time"

	"github.com/pkg/xattr"

	"infra/build/siso/o11y/clog"
	"infra/build/siso/o11y/iometrics"
	"infra/build/siso/reapi/digest"
)

// defaultDigestXattr is default xattr for digest. http://shortn/_8GHggPD2vw
const defaultDigestXattr = "google.digest.sha256"

// OSFS provides OS Filesystem access.
// It counts metrics by iometrics.
// It would be an interface to communicate local filesystem server,
// in addition to local filesystem.
type OSFS struct {
	*iometrics.IOMetrics

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
	flagSet.StringVar(&o.DigestXattrName, "fs_digest_xattr", xattrname, "xatr for sha256 digest")
}

// New creates new OSFS.
func New(ctx context.Context, name string, opt Option) *OSFS {
	if !xattr.XATTR_SUPPORTED {
		opt.DigestXattrName = ""
	}
	if opt.DigestXattrName != "" {
		clog.Infof(ctx, "use xattr %s for file digest", opt.DigestXattrName)
	}
	return &OSFS{
		IOMetrics:       iometrics.New(name),
		digestXattrName: opt.DigestXattrName,
	}
}

func logSlow(ctx context.Context, name string, dur time.Duration, err error) {
	buf := make([]byte, 4*1024)
	n := runtime.Stack(buf, false)
	clog.Warningf(ctx, "slow op %s: %s %v\n%s", name, dur, err, buf[:n])
}

// Chmod changes the mode of the named file to mode.
func (fs *OSFS) Chmod(ctx context.Context, name string, mode fs.FileMode) error {
	started := time.Now()
	err := os.Chmod(name, mode)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// Chtimes changes the access and modification times of the named file.
func (fs *OSFS) Chtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	started := time.Now()
	err := os.Chtimes(name, atime, mtime)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
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
func (fs *OSFS) FileSource(name string, size int64) FileSource {
	return FileSource{Fname: name, size: size, fs: fs}
}

// Lstat returns a FileInfo describing the named file.
func (fs *OSFS) Lstat(ctx context.Context, fname string) (fs.FileInfo, error) {
	started := time.Now()
	fi, err := os.Lstat(fname)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, fname, dur, err)
	}
	return fi, err
}

// MkdirAll creates a directory named path, along with any necessary parents.
func (fs *OSFS) MkdirAll(ctx context.Context, dirname string, perm fs.FileMode) error {
	started := time.Now()
	err := os.MkdirAll(dirname, perm)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, dirname, dur, err)
	}
	return err
}

// Readlink returns the destination of the named symbolic link.
func (fs *OSFS) Readlink(ctx context.Context, name string) (string, error) {
	started := time.Now()
	target, err := os.Readlink(name)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return target, err
}

// Remove removes the named file or directory.
func (fs *OSFS) Remove(ctx context.Context, name string) error {
	started := time.Now()
	err := os.Remove(name)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// Symlink creates newname as a symbolic link to oldname.
func (fs *OSFS) Symlink(ctx context.Context, oldname, newname string) error {
	started := time.Now()
	err := os.Symlink(oldname, newname)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, newname, dur, err)
	}
	return err
}

// WriteFile writes data to the named file, creating it if necessary.
func (fs *OSFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	started := time.Now()
	err := os.WriteFile(name, data, perm)
	fs.WriteDone(len(data), err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (fs *OSFS) FileDigestFromXattr(ctx context.Context, name string, size int64) (digest.Digest, error) {
	if fs.digestXattrName == "" {
		return digest.Digest{}, errors.ErrUnsupported
	}
	d, err := xattr.LGet(name, fs.digestXattrName)
	fs.OpsDone(err)
	if err != nil {
		return digest.Digest{}, err
	}
	if size < 0 {
		fi, err := os.Lstat(name)
		fs.OpsDone(err)
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
func (fs FileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := os.Open(fs.Fname)
	return &file{ctx: ctx, file: r, started: time.Now(), fs: fs.fs}, err
}

func (fs FileSource) String() string {
	return fmt.Sprintf("file://%s", fs.Fname)
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (fs FileSource) FileDigestFromXattr(ctx context.Context) (digest.Digest, error) {
	return fs.fs.FileDigestFromXattr(ctx, fs.Fname, fs.size)
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
	name := f.file.Name()
	err := f.file.Close()
	f.fs.ReadDone(f.n, err)
	if dur := time.Since(f.started); dur > 1*time.Minute {
		logSlow(f.ctx, name, dur, err)
	}
	return err
}
