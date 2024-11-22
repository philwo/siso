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
