# -*- bazel-starlark -*-
# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.
load("@builtin//struct.star", "module")

def __parse_args(gnargs):
    lines = gnargs.splitlines()
    args = {}
    for line in lines:
        line = line.strip()
        if line.startswith("#"):
            continue
        s = line.partition("=")
        if len(s) != 3:
            continue
        name = s[0].strip()
        value = s[2].strip()
        args[name] = value
    return args

gn = module(
    "gn",
    parse_args = __parse_args,
)
