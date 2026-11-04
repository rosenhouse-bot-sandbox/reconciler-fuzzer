#!/usr/bin/env bash
# Prints the body of the issue or comment that reports a failed nightly hunt.
# usage: nightly-issue-body.sh <example> <status> <run-id> <run-url> <summary.md>
set -euo pipefail
example=$1 status=$2 run_id=$3 run_url=$4 summary=$5
# GitHub refuses a body over 65536 characters.
limit=60000

cat <<BODY
🤖 Created by Claude 🤖

\`make hunt-$example\` exited $status on $(date -u +%Y-%m-%d).

- Run: $run_url
- Fetch its output: \`gh run download $run_id --name reconciler-fuzzer-out-$example --dir reconciler-fuzzer-out\`
- Replay a failing seed: run \`make build $example\` and \`export KUBEBUILDER_ASSETS=\$(make -s assets-path)\`, then \`./bin/reconciler-fuzzer run --config examples/$example/config.yaml --plan reconciler-fuzzer-out/hunt-$example/seed-<n>/plan.json --out replay\`. summary.md names the failing seeds and each shrunk plan, which replays the same way.

BODY
if [ ! -s "$summary" ]; then
	echo "The hunt wrote no summary.md. The run's log shows its error."
	exit 0
fi
echo "## summary.md"
echo
head -c "$limit" "$summary"
if [ "$(wc -c <"$summary")" -gt "$limit" ]; then
	echo
	echo "summary.md is cut at $limit bytes. The run's output holds all of it."
fi
