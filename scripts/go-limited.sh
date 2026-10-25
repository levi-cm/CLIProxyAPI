#!/usr/bin/env sh
# Limit compilation to half the available CPUs, with a one-CPU minimum.
set -eu

case "${1:-}" in
  build|test|vet) ;;
  *) printf '%s\n' 'Usage: scripts/go-limited.sh build|test|vet [arguments...]' >&2; exit 2 ;;
esac

# Keep package serialization enforceable even with explicit Go command flags.
# Arguments after -args belong to the test binary, not the compiler.
build_expect_parallelism=false
for build_argument do
  if [ "$build_expect_parallelism" = true ]; then
    if [ "$build_argument" != 1 ]; then
      printf '%s\n' 'Package parallelism is fixed at 1.' >&2; exit 2
    fi
    build_expect_parallelism=false
    continue
  fi
  case "$build_argument" in
    -args|--) break ;;
    -p) build_expect_parallelism=true ;;
    -p=1) ;;
    -p=*) printf '%s\n' 'Package parallelism is fixed at 1.' >&2; exit 2 ;;
  esac
done
if [ "$build_expect_parallelism" = true ]; then
  printf '%s\n' 'The -p flag requires 1.' >&2; exit 2
fi

if command -v nproc >/dev/null 2>&1; then
  build_cpu_count=$(
    unset OMP_NUM_THREADS OMP_THREAD_LIMIT
    nproc
  )
elif command -v getconf >/dev/null 2>&1; then
  build_cpu_count=$(getconf _NPROCESSORS_ONLN)
elif command -v sysctl >/dev/null 2>&1; then
  build_cpu_count=$(sysctl -n hw.ncpu)
else
  build_cpu_count=${NUMBER_OF_PROCESSORS:-1}
fi
build_cpu_budget=$((build_cpu_count / 2))
if [ "$build_cpu_budget" -lt 1 ]; then build_cpu_budget=1; fi

# Serialize packages as well as limiting each Go compiler's workers.
export GOMAXPROCS="$build_cpu_budget"
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=1"

# On Linux, restrict all compiler/linker/CGO child processes to the same CPUs.
# Other platforms retain the Go concurrency limits above.
if command -v taskset >/dev/null 2>&1 && [ -r /proc/self/status ]; then
  build_cpu_list=$(awk -v limit="$build_cpu_budget" '
    /^Cpus_allowed_list:/ {
      count = split($2, ranges, ",")
      for (i = 1; i <= count && selected < limit; i++) {
        split(ranges[i], bounds, "-")
        last = bounds[2] == "" ? bounds[1] : bounds[2]
        for (cpu = bounds[1]; cpu <= last && selected < limit; cpu++) {
          printf "%s%d", selected ? "," : "", cpu
          selected++
        }
      }
    }' /proc/self/status)
  exec taskset -c "$build_cpu_list" "${GO_BINARY:-go}" "$@"
fi
exec "${GO_BINARY:-go}" "$@"
