#!/usr/bin/env bash
set -euo pipefail

patterns=(
  'font-family.*[Ii]nter'
  'font-family.*Roboto'
  'purple-to-pink'
  'from-purple-'
  'bg-gradient-to-.*purple'
)

for pattern in "${patterns[@]}"; do
  if rg -n -e "$pattern" src public index.html tailwind.config.ts vite.config.ts >/dev/null; then
    echo "Aesthetic lint failed: found banned pattern '$pattern'" >&2
    rg -n -e "$pattern" src public index.html tailwind.config.ts vite.config.ts >&2 || true
    exit 1
  fi
done

echo 'aesthetic lint passed'
