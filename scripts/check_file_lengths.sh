#!/usr/bin/env bash
# Reports tracked .go files over the repo size norm (default 500 lines),
# excluding generated files (a "Code generated ... DO NOT EDIT" header).
# Exits non-zero if any offender exists, so it can gate CI once the tree is
# clean. Usage: scripts/check_file_lengths.sh [limit]
set -uo pipefail
limit="${1:-500}"
offenders=0
while IFS= read -r f; do
  head -3 "$f" | grep -qiE 'code generated .* do not edit' && continue
  n=$(wc -l < "$f")
  if [ "$n" -gt "$limit" ]; then
    printf '%6d  %s\n' "$n" "$f"
    offenders=$((offenders + 1))
  fi
done < <(git ls-files '*.go')
if [ "$offenders" -gt 0 ]; then
  echo "-- $offenders file(s) over $limit lines" >&2
  exit 1
fi
echo "all tracked .go files within $limit lines"
