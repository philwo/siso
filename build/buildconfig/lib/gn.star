# -*- bazel-starlark -*-
# Copyright 2023 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.
load("@builtin//path.star", "path")
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

# TODO(https://bugs.chromium.org/p/gn/issues/detail?id=346): use gn if possible.
# similar with https://chromium.googlesource.com/chromium/tools/depot_tools/+/422ba5b9a58c764572478b2c3d948b35ef9c2811/autoninja.py#30
def __load_args(ctx, fname, gnargs):
    lines = gnargs.splitlines()
    args = {}
    for line in lines:
        line = line.strip()
        if len(line) == 0:
            continue
        if line.startswith("#"):
            continue
        print("line %s" % line)
        if line.startswith("import(\""):
            raw_import_path = line.removeprefix("import(\"")
            i = raw_import_path.find("\"")
            if i < 0:
                print("wrong import line %s" % line)
                continue
            raw_import_path = raw_import_path[:i]
            if raw_import_path.startswith("//"):
                import_path = raw_import_path[2:]
            else:
                import_path = path.join(path.dir(fname), raw_import_path)
            print("import_path %s" % import_path)
            content = ctx.fs.read(import_path)
            args.update(__load_args(ctx, import_path, str(content)))
            continue
        s = line.partition("=")
        if len(s) != 3:
            continue
        name = s[0].strip()
        value = s[2].strip()
        args[name] = value
    return args

def __args(ctx):
    return __load_args(ctx, ctx.fs.canonpath("./args.gn"), ctx.metadata["args.gn"])

gn = module(
    "gn",
    # deprecated. use args(ctx) to support import().
    # TODO(b/303338337): remove parse_args.
    parse_args = __parse_args,
    args = __args,
)
