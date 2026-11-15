#!/usr/bin/env bash
set -euo pipefail

base="${1:?usage: check-migrations-immutable.sh <base-commit> [head-commit]}"
head="${2:-HEAD}"

mapfile -t shipped < <(git ls-tree -r --name-only "$base" -- lib/migrations/ | sort)
if ((${#shipped[@]} == 0)); then
	printf 'no previously shipped migration history in %s; forward-history check begins with this revision\n' "$base"
	exit 0
fi

for path in "${shipped[@]}"; do
	if ! git cat-file -e "$head:$path" 2>/dev/null; then
		printf 'published migration was removed: %s\n' "$path" >&2
		exit 1
	fi
	if ! git diff --quiet "$base" "$head" -- "$path"; then
		printf 'published migration was modified: %s\n' "$path" >&2
		exit 1
	fi
done
printf 'published migration history is unchanged (%d files)\n' "${#shipped[@]}"
