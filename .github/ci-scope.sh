#!/usr/bin/env bash
# Decide whether a change needs code CI or is documentation prose only.
#
#   .github/ci-scope.sh <base> <head>
#
# Compares the two commits' trees (two-dot): on a pull request pass the merge
# commit under test as <head> and its first parent (the base tip GitHub merged
# into) as <base>, so exactly the merge result is classified; on a push pass
# the previous and the pushed tip.
#
# Prints exactly one line, `code=true` or `code=false`, on stdout (append it to
# $GITHUB_OUTPUT); the reasoning goes to stderr. It prints `code=false` only
# when it could list the changed files and every one of them is
# documentation prose. Anything it cannot decide (no base, an unknown commit,
# a failed diff, an empty diff) is `code=true`: when in doubt, run CI.
#
# Paths are read NUL-separated, so no file name can split into two. Renames
# are listed as a delete plus an add (--no-renames), so both sides are
# classified: a file moved out of docs/ into code counts as code.
set -uo pipefail

# Paths under docs/ that tests read. These are test inputs, not prose.
ci_inputs() {
  case "$1" in
    docs/evidence/* | docs/direct/fixtures/*) return 0 ;;
    # Embedded into the binary via go:embed (internal/skills/skills.go).
    internal/skills/library/*) return 0 ;;
  esac
  return 1
}

# Documentation prose: the docs/ tree and README files anywhere.
docs_prose() {
  case "$1" in
    docs/*) return 0 ;;
  esac
  case "${1##*/}" in
    README | README.md | README.txt | README.rst | readme.md) return 0 ;;
  esac
  return 1
}

run_ci() {
  echo "ci-scope: $1; running CI" >&2
  echo "code=true"
  exit 0
}

base=${1:-}
head=${2:-}
[ -n "$base" ] || run_ci "no base commit"
[ -n "$head" ] || run_ci "no head commit"
git rev-parse -q --verify "$base^{commit}" >/dev/null 2>&1 || run_ci "base $base is not a known commit"
git rev-parse -q --verify "$head^{commit}" >/dev/null 2>&1 || run_ci "head $head is not a known commit"

list=$(mktemp) || run_ci "mktemp failed"
trap 'rm -f "$list" "$list.err"' EXIT
git diff --no-renames --name-only -z "$base" "$head" -- >"$list" 2>"$list.err" ||
  run_ci "git diff $base $head failed: $(cat "$list.err")"
[ -s "$list" ] || run_ci "empty diff"

while IFS= read -r -d '' f; do
  if ci_inputs "$f"; then run_ci "$f is a CI input"; fi
  if ! docs_prose "$f"; then run_ci "$f is not documentation"; fi
done <"$list"

echo "ci-scope: documentation only; skipping code CI:" >&2
tr '\0' '\n' <"$list" | sed 's/^/  /' >&2
echo "code=false"
