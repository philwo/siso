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
	"time"

	"github.com/maruel/subcommands"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/auth/cred"
	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
)

const flushUsage = `flush recorded files to the disk.

 $ siso fs flush -project <projectID> -C <dir> [<files>...]

It will fetch content for <files> recorded in .siso_fs_state.
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

	authOpts  cred.Options
	dir       string
	stateFile string
	projectID string
	reopt     *reapi.Option
	force     bool
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

	if c.Flags.NArg() == 0 {
		return fmt.Errorf("no files to flush: %w", flag.ErrHelp)
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
	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.stateFile})
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", c.stateFile, err)
	}
	stm := hashfs.StateMap(st)
	for _, fname := range c.Flags.Args() {
		fmt.Printf("%s ...", fname)
		_ = os.Stdout.Sync()
		fullpath := filepath.ToSlash(filepath.Join(wd, fname))
		_, err := os.Lstat(fullpath)
		if !c.force && err == nil {
			fmt.Printf("exists\n")
			continue
		}
		ent, ok := stm[fullpath]
		if !ok {
			fmt.Printf("not found\n")
			continue
		}
		if ent.Target != "" {
			err := os.Symlink(ent.Target, fullpath)
			if err != nil {
				fmt.Printf("%v\n", err)
				continue
			}
			fmt.Printf("symlink\n")
			continue
		}
		d := toDigest(ent.Digest)
		if d.IsZero() {
			// directory
			fmt.Printf("dir\n")
			continue
		}
		action := toDigest(ent.Action)
		if action.IsZero() {
			// no remote action
			fmt.Printf("local generated\n")
			continue
		}
		err = c.flush(ctx, cacheStore, fname, d, ent)
		if err != nil {
			fmt.Printf("err: %v\n", err)
			continue
		}
		fmt.Printf("done\n")
	}
	return nil
}

func (c *flushRun) flush(ctx context.Context, cacheStore reapi.CacheStore, fname string, d digest.Digest, ent *pb.Entry) error {
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
