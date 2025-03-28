// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"context"
	"fmt"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"infra/build/siso/execute"
)

const (
	// cmd args. tuple
	cmdFieldArgs = "args"
	// cmd envs. dict
	cmdFieldEnvs = "envs"
	// cmd dir. string
	cmdFieldDir = "dir"
	// cmd exec_root. string
	cmdFieldExecRoot = "exec_root"
	// cmd deps. string
	cmdFieldDeps = "deps"
	// cmd inputs. list
	cmdFieldInputs = "inputs"
	// cmd tool_inputs. list
	cmdFieldToolInputs = "tool_inputs"
	// cmd outputs. list
	cmdFieldOutputs = "outputs"
)

// packCmd packs cmd into Starlark struct.
func packCmd(ctx context.Context, cmd *execute.Cmd) (*starlarkstruct.Struct, error) {
	envs, err := packEnvmap(cmd.Env)
	if err != nil {
		return nil, err
	}
	return starlarkstruct.FromStringDict(starlark.String("cmd"), map[string]starlark.Value{
		cmdFieldArgs:       packTuple(cmd.Args),
		cmdFieldEnvs:       envs,
		cmdFieldDir:        starlark.String(cmd.Dir),
		cmdFieldExecRoot:   starlark.String(cmd.ExecRoot),
		cmdFieldDeps:       starlark.String(cmd.Deps),
		cmdFieldInputs:     packList(cmd.Inputs),
		cmdFieldToolInputs: packList(cmd.ToolInputs),
		cmdFieldOutputs:    packList(cmd.Outputs),
	}), nil

}

func packTuple(list []string) starlark.Value {
	values := make([]starlark.Value, 0, len(list))
	for _, elem := range list {
		values = append(values, starlark.String(elem))
	}
	return starlark.Tuple(values)
}

func packEnvmap(envs []string) (starlark.Value, error) {
	dict := starlark.NewDict(len(envs))
	for _, env := range envs {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			k = env
			v = ""
		}
		err := dict.SetKey(starlark.String(k), starlark.String(v))
		if err != nil {
			return nil, fmt.Errorf("set %s=%s: %w", k, v, err)
		}
	}
	return dict, nil
}

func packList(list []string) starlark.Value {
	values := make([]starlark.Value, 0, len(list))
	for _, elem := range list {
		values = append(values, starlark.String(elem))
	}
	return starlark.NewList(values)
}

func unpackList(v starlark.Value) ([]string, error) {
	iterator := starlark.Iterate(v)
	if iterator == nil {
		return nil, fmt.Errorf("got %v; want iterator", v.Type())
	}
	defer iterator.Done()
	var elem starlark.Value
	var list []string
	for iterator.Next(&elem) {
		s, ok := starlark.AsString(elem)
		if !ok {
			return nil, fmt.Errorf("got %v in %v; want string", elem.Type(), v.Type())
		}
		list = append(list, s)
	}
	return list, nil
}

// uniqueList returns a list that doesn't contain duplicate items.
func uniqueList(inputsList ...[]string) []string {
	seen := make(map[string]bool)
	var inputs []string
	for _, ins := range inputsList {
		for _, in := range ins {
			if in == "" {
				continue
			}
			if seen[in] {
				continue
			}
			seen[in] = true
			inputs = append(inputs, in)
		}
	}
	// make a backing array of slice small enough.
	r := make([]string, len(inputs))
	copy(r, inputs)
	return r
}
