# Git-attribute export benchmark (current implementation)

Measured 2026-11-17 on Linux/amd64, Intel Pentium Silver J5005 @ 1.50 GHz,
Go 1.26.11. This records before/after measurements for the prepared evaluator.

## Reproduction

From the repository root, after the normal frontend build prerequisite:

```sh
go test ./lib -run '^$' -bench '^BenchmarkExportTrackedTree$' -benchtime=3x -benchmem
go test ./lib -run '^$' -bench '^BenchmarkExportTrackedTree$' -benchtime=3x \
  -cpuprofile=/tmp/deploy-manager-export.cpu
go tool pprof -top /tmp/deploy-manager-export.cpu
```

The benchmark is `lib/deployment_export_benchmark_test.go`. It creates one
tracked commit and measures the current `exportTrackedTree` path into a fresh
directory per iteration. Input: 1,200 regular files in 128 directories, each
35 bytes, plus a tracked root `.gitattributes` with 129 applicable rules
(128 grouped rules and a recursive catch-all). Rules select `filter=other`,
so no LFS transfer or network access is involved. Limits are 2,000 files,
2 MiB and one minute. This intentionally exercises ordinary static content and
repeated parsing/matching; it is not a synthetic microbenchmark of the parser.

## Observed baseline and prepared implementation

Command above on this host (three measured iterations):

```text
BenchmarkExportTrackedTree-4  3  1547789570 ns/op
  144568040 B/op  6145534 allocs/op
```

That is about 1.55 seconds, 145 MB allocated and 6.15 million allocations per
1,200-file export on this constrained host. Treat these as measurements, not
performance thresholds. The CPU profile is available in the run's temporary
file; its largest visible costs include Linux syscalls (33.7% flat), allocator
work (15.1% cumulative), and `io.ReadAtLeast` (4.4% cumulative). Attribute
parser/matcher work is spread across small functions and does not dominate the
top individual symbols, so the profile does not support claiming a measured
parser-only percentage. Re-run locally with the documented command to capture
a fresh profile.

## Assessment

**GO: the narrowly scoped prepared evaluator has a worthwhile measured payoff.**
The old assessment's phrase "154,800 rule-file parses" was inaccurate: there
is one tracked attribute file containing 129 rules; it was parsed once for
each of 1,200 file evaluations (1,200 parses, 154,800 rule checks).

The prepared implementation, measured with the same command on this host:

```text
BenchmarkExportTrackedTree-4  3  378073724 ns/op
  14567250 B/op  262619 allocs/op
```

Elapsed time fell from 1.55 s to 0.378 s (about 75.6%), allocated bytes from
144.6 MB to 14.6 MB (about 89.9%), and allocations from 6.15 million to 263
thousand (about 95.7%). These are whole-export observations, not parser-only
attribution, and are evidence rather than thresholds. The measured payoff
warrants the narrowly scoped per-export prepared state. Do not broaden the
optimization into publication behavior changes.

LFS selection, fail-closed outcomes and their timing, resource limits, and
per-file matching remain unchanged.
