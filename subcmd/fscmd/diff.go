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

		diff, found := checkDiff(s.Name, cur, base)
		if !found {
			continue
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

type entryDiff struct {
	Name     string    `json:"name"`
	DiffType string    `json:"diff_type"`
	Cur      *pb.Entry `json:"cur"`
	Base     *pb.Entry `json:"base,omitempty"`
}

func checkDiff(name string, cur, base *pb.Entry) (entryDiff, bool) {
	var diffType string
	switch {
	case proto.Equal(cur, base):
		return entryDiff{}, false
		// cur should not be nil.
	case base == nil:
		diffType = "new"
	case !proto.Equal(cur.Digest, base.Digest):
		diffType = "content_modified"
	case cur.IsExecutable != base.IsExecutable:
		diffType = "mode_modified"
	case cur.Target != base.Target:
		diffType = "symlink_modified"

		// followings will be restattable?
	case !bytes.Equal(cur.CmdHash, base.CmdHash):
		diffType = "cmdline_modified"
	case !proto.Equal(cur.Action, base.Action):
		diffType = "action_modified"
	case cur.Id.GetModTime() != base.Id.GetModTime():
		diffType = "mtime_modified"
	case cur.UpdatedTime != base.UpdatedTime:
		diffType = "clean_updated"
	default:
		diffType = "unknown"
	}
	return entryDiff{
		Name:     name,
		DiffType: diffType,
		Cur:      cur,
		Base:     base,
	}, true
}
