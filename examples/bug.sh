#!/bin/sh
# Runs a plan of a known real bug up to <attempts> times, and passes once a
# run gives a finding of the named check whose section matches the pattern.
# It exits 1 when no attempt does, and 2 on a harness error.
#
#   bug.sh <example> <plan> <check> <pattern> <attempts>
set -u
cd "$(dirname "$0")/.."
example=$1 plan=$2 check=$3 pattern=$4 attempts=$5
dir=examples/$example
out=reconciler-fuzzer-out/$example-bug
for i in $(seq "$attempts"); do
  echo "==> $example $plan, attempt $i"
  rm -rf "$out"
  bin/reconciler-fuzzer run --config "$dir/config.yaml" --plan "$dir/$plan" --out "$out" >/dev/null
  status=$?
  [ $status -eq 2 ] && { cat "$out/report.md" 2>/dev/null; exit 2; }
  if [ $status -eq 1 ] && awk -v check="## $check:" '
      /^## / { inside = index($0, check) == 1 }
      inside' "$out/report.md" | grep -q -- "$pattern"; then
    echo "$example: $plan found the bug on attempt $i."
    exit 0
  fi
done
cat "$out/report.md" 2>/dev/null
echo "$example: $plan gave no $check finding matching '$pattern' in $attempts attempts."
exit 1
