# Siso

Siso is an experimental build tool that aims to significantly speed up
Chromium's build.

* It is a drop-in replacement for Ninja, which means it can be easily used
  instead of Ninja without requiring a migration or change in developer's
  workflows.
* It runs build actions on RBE natively, and falls back to local.
* It avoids stat, disk and network I/O as much as possible.
* It reduces CPU usage and memory consumption by sharing in one process memory
  space.
* It collects performance metrics for each action during a build and allows to
  analyze them using cloud trace/cloud profiler.

## Status

Siso is under development and not yet ready for general use.

## Development

### Profiling

If you call Siso with the flag `-pprof_addr=localhost:6060`, it will start an
HTTP server on this socket that serves runtime profiling data. This is useful
for inspecting and debugging various aspects of Siso's performance. Please see
the documentation of [go tool pprof](https://pkg.go.dev/net/http/pprof) for
instructions and examples how to use it effectively.

Alternatively, you can use `-cpuprofile=cpu.prof` or `-memprofile=memory.prof`
to collect a CPU or memory profile while Siso runs and save it to disk.

## References

* [Previous location of Siso's source](https://chrome-internal.googlesource.com/infra/infra_internal/+/refs/heads/main/go/src/infra_internal/experimental/siso)
