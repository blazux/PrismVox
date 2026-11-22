#!/usr/bin/env bash
# Only a complete stable version promotes Docker latest.
set -euo pipefail
[[ $# == 1 ]] || exit 2
stable='^refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if [[ "$1" =~ $stable ]]; then
  echo true
else
  echo false
fi
