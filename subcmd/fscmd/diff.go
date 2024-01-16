// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/maruel/subcommands"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/luci/common/cli"

	"infra/build/siso/hashfs"
	pb "infra/build/siso/hashfs/proto"
)

func cmdFSDiff() *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "diff",
		ShortDesc: "diff siso hashfs data",
		LongDesc: `show difference between two siso hashfs data.

 $ siso fs diff -C <dir>

It will print mismatched file entries between .siso_fs_state (--fs_state)
and .siso_fs_state.0 (--fs_state_base).
`,
		CommandRun: func() subcommands.CommandRun {
			c := &diffRun{}
			c.init()
			return c
		},
	}
}

type diffRun struct {
	subcommands.CommandRunBase
	dir           string
	stateFile     string
	stateFileBase string
	// TODO: options to compare mtime
}

func (c *diffRun) init() {
	c.Flags.StringVar(&c.dir, "C", ".", "ninja running directory")
	c.Flags.StringVar(&c.stateFile, "fs_state", stateFile, "fs_state filename")
	c.Flags.StringVar(&c.stateFileBase, "fs_state_base", stateFile+".0", "fs_state filename for diff base")
}

func (c *diffRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)

	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}

	st, err := hashfs.Load(ctx, c.stateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFile, err)
		return 1
	}
	// sort files in build order.
	sort.Slice(st.Entries, func(i, j int) bool {
		if st.Entries[i].UpdatedTime != st.Entries[j].UpdatedTime {
			return st.Entries[i].UpdatedTime < st.Entries[j].UpdatedTime
		}
		return st.Entries[i].Id.GetModTime() < st.Entries[j].Id.GetModTime()
	})
	stm := hashfs.StateMap(st)
	stBase, err := hashfs.Load(ctx, c.stateFileBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFileBase, err)
		return 1
	}
	stBaseM := hashfs.StateMap(stBase)

	for _, s := range st.Entries {
		cur := stm[s.Name]
		base := stBaseM[s.Name]

		if entryEqual(cur, base) {
			continue
		}
		var diff = struct {
			Name string    `json:"name"`
			Cur  *pb.Entry `json:"cur"`
			Base *pb.Entry `json:"base,omitempty"`
		}{
			Name: s.Name,
			Cur:  cur,
			Base: base,
		}
		buf, err := json.MarshalIndent(diff, "", " ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal error: %v\n", err)
			return 1
		}
		fmt.Printf("%s\n", buf)
	}
	return 0
}

func entryEqual(a, b *pb.Entry) bool {
	// a never nil
	if b == nil {
		return false
	}
	if !proto.Equal(a.Digest, b.Digest) {
		return false
	}
	if a.IsExecutable != b.IsExecutable {
		return false
	}
	if a.Target != b.Target {
		return false
	}
	if !bytes.Equal(a.CmdHash, b.CmdHash) {
		return false
	}
	return proto.Equal(a.Action, b.Action)
}
