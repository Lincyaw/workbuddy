#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

check() {
  local pattern=$1
  shift
  if rg -n --glob '!src/**/*.test.tsx' --glob '!src/**/*.test.ts' --glob '!public/fonts/*' "$pattern" "$@"; then
    fail=1
  fi
}

check 'font-family[^;]*\b[Ii]nter\b' src public
check 'font-family[^;]*\bRoboto\b' src public
check 'from-purple-' src public
check 'bg-gradient-to-.*purple' src public
if rg -n --glob '*.tsx' --glob '!**/*.test.tsx' '#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?\b' src; then
  fail=1
fi

if [[ $fail -ne 0 ]]; then
  echo 'aesthetic lint failed' >&2
  exit 1
fi

echo 'aesthetic lint passed'
