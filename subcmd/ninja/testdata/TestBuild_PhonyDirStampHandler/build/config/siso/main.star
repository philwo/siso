# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __stamp(ctx, cmd):
    out = cmd.outputs[0]
    ctx.actions.write(out)
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "stamp": __stamp,
}

def init(ctx):
    return module(
        "config",
        step_config = json.encode({
            "rules": [
                {
		    "name": "stamp",
		    "action": "stamp",
		    "handler": "stamp",
		},
            ],
        }),
        filegroups = {},
        handlers = __handlers,
    )
