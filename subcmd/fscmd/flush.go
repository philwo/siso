// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"go.chromium.org/infra/build/siso/auth/cred"
	"go.chromium.org/infra/build/siso/hashfs"
	pb "go.chromium.org/infra/build/siso/hashfs/proto"
	"go.chromium.org/infra/build/siso/reapi"
	"go.chromium.org/infra/build/siso/reapi/digest"
)

const flushUsage = `flush recorded files to the disk.

 $ siso fs flush -project <projectID> -C <dir> [<files>...]
 $ siso fs flush -project <projectID> -C <dir> -file_list <file>

It will fetch the specified files recorded in .siso_fs_state.
`

func cmdFSFlush(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "flush",
		ShortDesc: "flush recorded files to the disk",
		LongDesc:  flushUsage,
		CommandRun: func() subcommands.CommandRun {
			c := &flushRun{
				authOpts: authOpts,
			}
			c.init()
			return c
		},
	}
}

type flushRun struct {
	subcommands.CommandRunBase

	authOpts     cred.Options
	dir          string
	stateFile    string
	projectID    string
	reopt        *reapi.Option
	force        bool
	recursive    bool
	fileListPath string
}

func (c *flushRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.stateFile, "fs_state", stateFile, "fs_state filename")
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	envs := map[string]string{
		"SISO_REAPI_ADDRESS":  os.Getenv("SISO_REAPI_ADDRESS"),
		"SISO_REAPI_INSTANCE": os.Getenv("SISO_REAPI_INSTANCE"),
	}
	c.reopt.RegisterFlags(&c.Flags, envs)
	c.Flags.BoolVar(&c.force, "f", false, "force to fetch")
	c.Flags.BoolVar(&c.recursive, "recursive", true, "flush recursively")
	c.Flags.StringVar(&c.fileListPath, "file_list", "", "path to a file containing a list of files to flush, one per line")
}

func (c *flushRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLoginRequired):
			fmt.Fprintf(os.Stderr, "need to login: run `siso login`\n")
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, flushUsage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *flushRun) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	if c.Flags.NArg() == 0 && c.fileListPath == "" {
		return fmt.Errorf("no files to flush: %w", flag.ErrHelp)
	}
	if c.Flags.NArg() != 0 && c.fileListPath != "" {
		return fmt.Errorf("can not use file arguments and -file_list at the same time")
	}
	var fnames []string
	if c.Flags.NArg() > 0 {
		fnames = c.Flags.Args()
	} else {
		fileList, err := os.ReadFile(c.fileListPath)
		if err != nil {
			return fmt.Errorf("failed to read %q: %w", c.fileListPath, err)
		}
		fnames = strings.Split(string(fileList), "\n")
	}

	projectID := c.reopt.UpdateProjectID(c.projectID)
	if projectID == "" {
		return errors.New("project ID is not specified")
	}
	credential, err := cred.New(ctx, c.authOpts)
	if err != nil {
		return err
	}

	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return err
	}
	defer client.Close()
	cacheStore := client.CacheStore()

	err = os.Chdir(c.dir)
	if err != nil {
		return fmt.Errorf("failed to chdir %s: %w", c.dir, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get wd: %w", err)
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return fmt.Errorf("failed to eval symlinks: %w", err)
	}
	st, err := hashfs.Load(hashfs.Option{StateFile: c.stateFile})
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", c.stateFile, err)
	}
	stm := hashfs.StateMap(st)

	for _, fname := range fnames {
		fmt.Printf("%s ...", fname)
		_ = os.Stdout.Sync() // to print not ended by newline immediately.

		fullpath := filepath.ToSlash(filepath.Join(wd, fname))
		ent, ok := stm[fullpath]
		var isDir bool
		if !ok {
			fi, err := os.Lstat(fullpath)
			isDir = err == nil && fi.IsDir()
		} else {
			isDir = isDirEnt(ent)
		}
		if isDir {
			// directory
			if !c.recursive {
				fmt.Printf("dir\n")
				continue
			}
			children := childEntries(stm, fullpath)
			fmt.Printf("dir - expands %d\n", len(children))
			for _, ent := range children {
				fname, err := filepath.Rel(wd, ent.Name)
				if err != nil {
					fname = ent.Name
				}
				fname = filepath.ToSlash(fname)
				fmt.Printf("%s ...", fname)
				_ = os.Stdout.Sync()
				err = c.flushEntry(ctx, cacheStore, fname, ent)
				if err != nil {
					return err
				}
			}
			continue
		}
		if !ok {
			fmt.Printf("not found\n")
			continue
		}
		err = c.flushEntry(ctx, cacheStore, fname, ent)
		if err != nil {
			return err
		}
	}
	return nil
}

func isDirEnt(ent *pb.Entry) bool {
	return toDigest(ent.Digest).IsZero() && ent.Target == ""
}

// childEntries returns entries from stm that exists under fullpath.
// We only need hashfs's entry since if file already exists locally,
// flush need nothing to do.
// Note that `siso isolate` need to collect local files to send isolate server,
// so should use hashfs Walk to collect files.
func childEntries(stm map[string]*pb.Entry, fullpath string) []*pb.Entry {
	var fnames []string
	for k := range stm {
		fnames = append(fnames, k)
	}
	sort.Strings(fnames)
	var children []*pb.Entry
	for _, fname := range fnames {
		ent := stm[fname]
		rel, err := filepath.Rel(fullpath, ent.Name)
		if err != nil {
			continue
		}
		if !filepath.IsLocal(rel) {
			continue
		}
		if isDirEnt(ent) {
			continue
		}
		children = append(children, ent)
	}
	return children
}

func (c *flushRun) flushEntry(ctx context.Context, cacheStore reapi.CacheStore, fname string, ent *pb.Entry) error {
	mtime := time.Unix(0, ent.GetId().GetModTime())
	fi, err := os.Lstat(ent.Name)
	if !c.force && err == nil {
		if fi.ModTime().Equal(mtime) {
			// disk is same.
			fmt.Printf("exists\n")
			return nil
		}
		if fi.ModTime().Before(mtime) {
			// disk is newer than state.
			fmt.Printf("new file exists\n")
			return fmt.Errorf("disk is newer than state: disk=%s state=%s. use '-f' to force update", fi.ModTime(), mtime)
		}
	}
	// force, or no disk, or disk is older than state.
	if ent.Target != "" {
		err := os.Symlink(ent.Target, ent.Name)
		if err != nil {
			fmt.Printf("%v\n", err)
		}
		fmt.Printf("symlink\n")
		return nil
	}
	d := toDigest(ent.Digest)
	action := toDigest(ent.Action)
	if action.IsZero() {
		// no remote action
		fmt.Printf("local generated\n")
		return nil
	}
	err = c.flushFile(ctx, cacheStore, fname, d, ent)
	if err != nil {
		fmt.Printf("err: %v\n", err)
		return fmt.Errorf("flush err: %w", err)
	}
	fmt.Printf("done\n")
	return nil
}

func (c *flushRun) flushFile(ctx context.Context, cacheStore reapi.CacheStore, fname string, d digest.Digest, ent *pb.Entry) error {
	w, err := os.Create(fname)
	if err != nil {
		return err
	}
	src := cacheStore.Source(ctx, d, fname)
	r, err := src.Open(ctx)
	if err != nil {
		_ = w.Close()
		_ = os.Remove(fname)
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	if err != nil {
		_ = w.Close()
		_ = os.Remove(fname)
		return err
	}
	err = w.Close()
	if err != nil {
		_ = os.Remove(fname)
		return err
	}
	if ent.IsExecutable {
		err = os.Chmod(fname, 0755)
		if err != nil {
			_ = os.Remove(fname)
			return err
		}
	}
	err = os.Chtimes(fname, time.Time{}, time.Unix(0, ent.Id.ModTime))
	if err != nil {
		_ = os.Remove(fname)
		return err
	}
	return nil
}

func toDigest(d *pb.Digest) digest.Digest {
	if d == nil {
		return digest.Digest{}
	}
	return digest.Digest{
		Hash:      d.Hash,
		SizeBytes: d.SizeBytes,
	}
}
