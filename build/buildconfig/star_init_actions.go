// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// starInitActions returns actions, which contains:
//
//	metadata(key, value): sets key=value in metadata by starlark config.
func starInitActions(kv map[string]string) starlark.Value {
	receiver := starMDReceiver{KV: kv}
	return starlarkstruct.FromStringDict(starlark.String("actions"), map[string]starlark.Value{
		"metadata": starlark.NewBuiltin("metadata", starActionsMetadata).BindReceiver(receiver),
	})
}
