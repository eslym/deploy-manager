# Git-attribute export benchmark (current implementation)

Measured 2026-11-17 on Linux/amd64, Intel Pentium Silver J5005 @ 1.50 GHz,
Go 1.26.11. This is a baseline of the unmodified export implementation; it
does not compare a hypothetical prepared-rule implementation.

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

## Observed baseline

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

**GO for the narrowly scoped follow-up (#56): lazily parse tracked attribute
rules once per export, while retaining per-file matching.** The current path
re-evaluates the same 129-rule input for each of 1,200 files (154,800
rule-file parses per export) and the representative export consumes roughly
145 MB of transient allocations. Reuse is justified as a focused reduction in
repeated work, but this baseline alone cannot quantify its speedup; #56 should
benchmark the prepared implementation against this benchmark before claiming
one. Do not broaden the optimization into publication behavior changes.

No publication code or behavior is changed by this benchmark. In particular,
LFS selection, fail-closed errors and their timing, resource limits, and
per-file matching remain untouched.
