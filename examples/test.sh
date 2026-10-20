#!/bin/sh
# Runs an example's plans and the generated seeds its seeds file lists,
# which must give no findings, then its negative control, which must give a
# Clean finding that names the control's Secret. EXAMPLE_SEEDS replaces
# the seeds file; set empty, it runs no seeds. It exits 1 on a wrong verdict
# and 2 on a harness error.
#
#   test.sh <example> <control plan> <control Secret> [control run flag]...
set -u
cd "$(dirname "$0")/.."
example=$1 control=$2 secret=$3
shift 3
dir=examples/$example
seeds=${EXAMPLE_SEEDS-$(cat "$dir/seeds")}
out=reconciler-fuzzer-out/$example
rf=bin/reconciler-fuzzer
rm -rf "$out"

run() {
  name=$1
  shift
  echo "==> $example $name"
  "$rf" run --config "$dir/config.yaml" --out "$out/$name" "$@" >/dev/null
}

for plan in "$dir"/plans/*.json; do
  name=$(basename "$plan" .json)
  run "$name" --plan "$plan"
  status=$?
  if [ $status -ne 0 ]; then cat "$out/$name/report.md" 2>/dev/null; exit $status; fi
done

for seed in $seeds; do
  run "seed-$seed" --seed "$seed"
  status=$?
  if [ $status -ne 0 ]; then cat "$out/seed-$seed/report.md" 2>/dev/null; exit $status; fi
done

run control --plan "$dir/$control" "$@"
status=$?
if [ $status -ne 1 ] || ! sed -n '/^## Clean:/,/^## [^C]/p' "$out/control/report.md" | grep -q "secrets .*/$secret "; then
  cat "$out/control/report.md" 2>/dev/null
  echo "$example: the negative control exited $status without a Clean finding on Secret $secret, so the tier proves nothing."
  [ $status -eq 2 ] && exit 2
  exit 1
fi
echo "$example: the plans and seeds passed, and the negative control failed Clean."
