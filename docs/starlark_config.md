# siso starlark config

siso uses [starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
for configuration to apply to
[ninja build rules](https://ninja-build.org/manual.html#_writing_your_own_ninja_files).

`siso ninja` subcommand will load starlark file
specified by `-load` flag at startup time.
Default is `@config//main.star` (`//build/config/siso/main.star`).

The starlark script should provide
[init function to register handlers and step configs](#Initialization).
In the starlark script, `load` can load from current directory,
[@builtin](#builtin) or [@config](#config).

The starlark script will be evaluated at
[initialization phase](#Initialization)
and [per-step phase](#per_step-config)

### Example

```
load("@builtin//json.star", "json")
load("@builtin//runtime.star", "runtime")
load("@builtin//struct.star", "module")

def _stamp(ctx, cmd):
  out = cmd.outputs[0]
  ctx.actions.write(out)
  ctx.actions.exit(exit_status = 0)

def init(ctx):
  print("runtime os:%s arch:%s num:%d " % (runtime.os, runtime.arch, runtime.num_cpu)

  step_config = {}
  step_config["platforms"] = {
    "default": {
        "OSFamily": "Linux",
        "container-image": "docker://gcr.io/chops-private-images-prod/rbe/siso-chromium/linux@sha256:d4fcda628ebcdb3dd79b166619c56da08d5d7bd43d1a7b1f69734904cc7a1bb2",
    },
  }
  input_deps = {
    "some/toolchain/clang": [
       "some/toolchain/libclang.so",
       "some/toolchain/extra-inputs",
    ],
  }
  step_config["input_deps"] = input_deps
  rules = [
     {
       "name": "cxx/compile",
       "action": "cxx",
       "inputs": [
         "some/toolchain/clang",
       ],
       "remote": True,
     },
     {
       "name": "stamp",
       "action": "stamp",
       "handler": "stamp",
     },
  ]
  step_config["rules"] = rules
  return module(
    "config",
    step_config = json.encode(step_config),
    filegroups = {
       "base:all_headers": {
          "type": "glob",
          "includes": ["*.h"],
       },
    },
    handlers = {
      "stamp": _stamp,
    }
 )
```

## Initialization

Before starting build process, `siso` calls `init` function with `ctx`
to register handlers and step configs.

`ctx` provides

  * `actions` module
    * `metadata` update metadata.
  * `metadata` dict
    * `args.gn`: contents of args.gn
    * `build.manifest`: digest of ninja build manifest
  * `flags` dict
    * command line flags. key doesn't have prefix `-` of flag.
  * `fs`: path is exec root relative.
    * `read`: read contents.
      * `fname`: filename
    * `is_dir` check path is dir.
      * `fname`: filename
    * `exists` check path exists.
      * `fname`: filename
    * `canonpath`: convert wd relative to exec root relative
      * `fname`: filename

`print` will print a message to log file.
`fail` will abort the process.

`init` should return a module.

   * `filegroups` dict
     * key: filegroup label (i.e. dir:name).
     * value: dict of a filegroup generator
       * "type" specifies filegroup generator type.
         * "glob":
           * glob match from the dir specified by the label.
           * "includes": path patterns to include the glob
           * "excludes": path patterns to exclude the glob
         * TODO(b/266759797): add other type, like "filelist".
   * `handlers` dict
     * key: handler name
     * value: handler function used in [per-step config](#per_step-config).
   * `step_config` json string
     [`StepConfig`](../build/ninjabuild/step_config.go). It will be stored in `.siso_config` in working directory.
     * `platforms`
       * key: platform reference name. "default" is used by default.
       * value: a dict of [platform properties](https://developers.google.com/remote-build-execution/docs/remote-execution-properties)
     * `input_deps`
       * key: input path, or label (label contains ':').
         If the key is a path, it will be included in the expanded inputs.
         If the key is a label, it won't be included in the expanded inputs.
        `:headers` label would be used for c++ scandeps for include dirs
         or sysroots.
       * values: other files or labels needed for the key.
     * `case_sensitive_inputs`
       * a list of filenames for case sensitive filesystem.
         if "a.txt" and "A.txt" are in this, and "a.txt" is
         used as input, "A.txt" will be added as input too.
     * `rules` list of `StepRule`.
        path is exec root relative, or cwd relative if it starts with "./"
        * identifier
          * `name`: unique name of the rule. required.
        * rule selector
          * `action`: regexp of action name. i.e. ninja rule name.
          * `action_outs`: output of the action.
          * `command_prefix`: prefix of step's command.
        * rule to apply
          * `inputs`: additional inputs
          * `exclude_input_patterns`: glob pattern to exclude from inputs
            * if it contains '/', full match to input path
            * otherwise, match basename of input path.
          * `indirect_inputs`: specify what inputs from the previous steps to use as inputs of this step.
            The matched indirect input are included recursively.
            Sibling outputs are also included. e.g. gen/{foo.stamp, foo.h, foo.cc} -> a step depending on gen/foo.stamp may need foo.h/foo.cc.
            Consider using `replace` or `accumulate` first, and use `indirect_inputs` only when they are not sufficient.
            * `includes`: glob patterns to match to indirect inputs.
              * if it contains '/', full match to input path with [path.Match](https://pkg.go.dev/path#Match).
              * otherwise, match basename of input path with [path.Match](https://pkg.go.dev/path#Match).
          * `outputs`: additional outputs
          * `outputs_map`: different deps based on outputs[0]
             * key: outputs[0]
             * value
               * `inputs`: additional inputs
               * `outputs`: additional outputs
               * `platform`: additional platform properties
               * `platform_ref`: overrides reference to platform properties
          * `restat`: true if step cmd reads output file and not write it
            (when no update needed)
          * `platform_ref`: reference to platform properties
          * `platform`: additional platform properties
          * `remote`: use remote exec or not
          * `remote_wrapper`: a wrapper command used in remote execution
          * `remote_command`: args[0] will be replaced with remote_command.
          * `remote_inputs`
             * key: path of input of remote action
             * value: path of file (local) for the key path.
          * `input_root_absolute_path`: need `InputRootAbsolutePath` or not.
          * `canonicalize_dir`: ok to canonicalize work dir or not.
          * `use_system_input`: ok to use input outside of exec root
             as it assumes those are platform container image.
          * `use_remote_exec_wrapper`: true if gomacc/rewrapper is used,
             so it runs locally without using deps/file trace.
          * `reproxy_config`: [`REProxyConfig`](../execute/cmd.go) if reproxy is used.
             the following RBE environment variables override the flags specified in the config:
             `RBE_exec_strategy`, `RBE_server_address`.
             See also https://github.com/bazelbuild/reclient/blob/main/docs/cmd-line-flags.md#rewrapper for more details.
          * `timeout`: duration of the step remote execution.
          * `handler`: handler name to use for the step
          * `deps`: deps overrides
             * `gcc`: use `gcc -M`
             * `msvc`: use `clang-cl /showIncludes`
             * `depfile`: depfile variable of the step
             * `none`: ignore deps variable in ninja
          * `output_local`: force download/outputs to local disk
          * `ignore_extra_input_pattern`: regexp to allow if it is used,
             but not listed in inputs.
          * `ignore_extra_output_pattern`: regexp to allow if it is generated,
             but not listed in outputs.
          * `impure`: mark it as not pure. i.e. not check inputs/outputs
          * `replace`: if any output of this step is used in other steps,
             those steps will use the inputs of this step as inputs
             instead of the outputs of this step.
             used for `stamp` step or so.
          * `accumulate`: if any output of this step is used in other steps,
             those steps will use the inputs and outputs of this step as inputs.
             used for thin archive or so.
             Not recursively accumulated.
          * `debug`: enable debug log in this step.

## per-step config

for each step, siso will apply the first matched rule.
If the rule has `handler`, it runs handler with `ctx`
and `cmd` when siso decides to run the step.
All inputs should be ready to use (via `ctx.fs`).

`ctx` provides

  * `actions`
    * `fix`: fix step
      * `inputs`: input pathnames
      * `tool_inputs`: input pathnames (not modified by deps)
      * `outputs`: output pathnames
      * `args`: args for the step
      * `reproxy_config`: [`REProxyConfig`](../execute/cmd.go) in json-encoded format.
    * `write`: write file
      * `fname`: filename
      * `content`: content
      * `is_executable`: is_executable flag
    * `copy`: copy file
      * `src`: src filename
      * `dst`: dst filename
      * `recursive`: recursive copy?
    * `symlink`: create symlink
      * `target`: symlnk target
      * `linkpath`: symlink path.
    * `exit`: exit. don't run locally nor remotely.
      * `exit_status`: exit status
      * `stdout`: stdout
      * `stderr`: stderr
  * `fs`: same as `fs` in `ctx.fs` used in `init`.

`cmd` provides

  * `args` tuple: command line args
  * `envs` dict: environment variables
  * `dir` string: working directory
  * `exec_root` string: exec root
  * `deps` string: deps type
  * `inputs` list: input pathnames
  * `tool_inputs` list: input pathnames (rule's inputs).
  * `expanded_inputs`: func returns list: expanded input pathnames
  * `outputs` list: output pathnames

`print` will print in log.

## @builtin

`@builtin` provides builtin modules/functions.

### [@builtin//checkout.star](../build/buildconfig/checkout.star)
provide `checkout`.

 * `git` struct:
   * `origin` string: origin URL

### [@builtin//encoding.star](../build/buildconfig/encoding.star)
provide [`json`](https://pkg.go.dev/go.starlark.net/lib/json)

### [@builtin//lib/gn.star](../build/buildconfig/lib/gn.star)
provide `gn`.

 * `parse_args: parse gnargs to dict.
   `gnargs`: a content of args.gn.

### [@builtin//path.star](../build/buildconfig/path.star)
provide `path`.

 * `base`: returns base name.
   * fname: filename
 * `dir`: return dir name.
   * fname: filename
 * `join`: join path elements.
   * args: path elements
 * `rel`: return relative path.
   * basepath: base path
   * targetpath: target path
 * `isabs`: check path is abs.
   * fname: filename

### [@builtin//runtime.star](../build/buildconfig/runtime.star)
provide `runtime`.

 * `num_cpu` int: number of cpus
 * `os` string: `GOOS` string.
 * `arch` string: `GOARCH` string

### [@builtin//struct.star](../build/buildconfig/struct.star)
provide [`struct`](https://pkg.go.dev/go.starlark.net/starlarkstruct#Struct)
and [`module`](https://pkg.go.dev/go.starlark.net/starlarkstruct#Module)

### @config

`@config` provides [samples](../build/samples).

### @config_overrides

`@config_overrides` provides access to local starlark files,
in `$exec_root/.siso_remote`.
It is expected to have a module with name (basename of starlark) that
has `rules`, `input_deps` functions.
If file doesn't exist, it provides a None for the name (basename of starlark).

## References

* [starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
* [ninja build rules](https://ninja-build.org/manual.html#_writing_your_own_ninja_files)
