# Siso

Siso is a build tool that aims to significantly speed up Chromium's build.

* It is a drop-in replacement for Ninja, which means it can be easily used
  instead of Ninja without requiring a migration or change in developer's
  workflows.
* It runs build actions on RBE natively, and falls back to local.
* It avoids stat, disk and network I/O as much as possible.
* It reduces CPU usage and memory consumption by sharing in one process memory
  space.
* It collects performance metrics for each action during a build and allows to
  analyze them using cloud trace/cloud profiler.

## Key Differences from Ninja

1.  **Remote execution capabilities:**

    * **Ninja:** Primarily focuses on local build execution. Ninja supports mainly
      c/cxx remote compiles by integrating with Reclient.
    * **Siso:** Offers extensive support for various remote execution.
      You can find Chromium's remote configurations [here](https://source.chromium.org/chromium/chromium/src/+/main:build/config/siso/).
      The configurations are currently maintained by the Browser Build team.

1.  **Handling of missing output files during restat:**

    * **Ninja:** May silently ignore missing output files when performing `restat`.
    * **Siso:** Enforces stricter checks. If an output file is missing during `restat`, Siso will fail the build.

1.  **Handling of missing input files during scheduling:**

    * **Ninja:** May silently ignore missing input files when scheduling build steps.
    * **Siso:** Enforces strict input dependency checks. If an input file is missing when scheduling a build step, Siso will fail the build.

1. **Targets from special syntax `target^`**

    * **Ninja:** Specifies only the first target that contains the source file
      with [target^](https://ninja-build.org/manual.html#_running_ninja:~:text=a%20special%20syntax-,target%5E,-for%20specifying%20a).
      For example, `foo.c^` will specify `foo.o`.
    * **Siso:** Expands `target^` to all the targets that contains the source
      file. For example, `foo.java^` will get expanded to all Java steps that
      use files such as javac, errorprone, turbine etc.
    * **Siso:** For a header file, tries to find one of the source files include the
      header directly. For example, `foo.h^` will be treated as `foo.cc^`.
    * Requested in [crbug.com/396522989](https://crbug.com/396522989)

1. **Supports `phony_output` rule variable**

    * **Ninja:** Doesn't have `phony_output`. But, Android's forked Ninja has a patch for the rule variable. See also [here](https://android.googlesource.com/platform/external/ninja/+/2ddc376cc3c5531db80899ce757861fac7a531b9/doc/manual.asciidoc#819)
    * **Siso:** Supports the variable for Android builds.

1. **Concurrent builds for the same build directory**

    * **Ninja:** Allows multiple build invocations to run for the same build
      directory. There can be a race problem.
    * **Siso:** Locks the build directory so that other build invocations would
      wait until the current invocation to finish.

1. **Handling of depfile parse error**

    * **Ninja:** Ignores a depfile parse error.
    * **Siso:** Fails for a depfile parse error.

## FAQ

Please check [go/siso-faq](http://go/siso-faq).

## Status

Siso is the primary build system for builder builds, and is being rolled out to
Chrome developers. Chromium and Chrome are only supported projects.
The projects that import //build/config from Chromium might be able to use Siso.
However, they are not tested or supported, yet.

As of Nov 2024, Siso is used by default for Chromium build on gLinux machine.

As of July 2024, Siso is used in all Chromium and Chrome builders, including official
builds released to users.

As of end of 2024 Q1, Siso is used in all CQ builders in Chromium.

As of April 2023, we are dogfooding Siso with invited Chrome developers.
Please check [go/chrome-build-dogfood](http://go/chrome-build-dogfood) for more information.

## Development

Please check [go/siso-development](http://go/siso-development).

## References

* [Previous location of Siso's source](https://chrome-internal.googlesource.com/infra/infra_internal/+/refs/heads/main/go/src/infra_internal/experimental/siso)
