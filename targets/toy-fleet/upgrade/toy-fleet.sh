#!/bin/sh
# Runs --version=1 on the run's first start and --version=2 on every later
# one, so that a plan's first restart upgrades the controller.
starts="$(dirname "$KUBECONFIG")/toy-fleet-starts"
echo >>"$starts"
version=2
[ "$(wc -l <"$starts")" -eq 1 ] && version=1
exec "$(dirname "$0")/../../../bin/toy-fleet" --version="$version" "$@"
