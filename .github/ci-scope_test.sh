#!/usr/bin/env bash
# Behaviour tests for .github/ci-scope.sh against throwaway git repositories.
#   bash .github/ci-scope_test.sh
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ci-scope.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0

g() { git -C "$work/repo" -c user.name=t -c user.email=t@t -c commit.gpgsign=false "$@"; }

# expect <name> <want> <change...>: start from a fresh base, apply the shell
# change, commit, and compare the classifier's verdict on base...head.
expect() {
  local name=$1 want=$2 change=$3
  rm -rf "$work/repo"
  git init -q "$work/repo"
  mkdir -p "$work/repo/docs/guide" "$work/repo/docs/evidence/x" "$work/repo/docs/direct/fixtures/v1" \
    "$work/repo/internal/skills/library/s" "$work/repo/cmd" "$work/repo/.github/workflows"
  for f in README.md docs/a.md docs/guide/b.md docs/evidence/x/raw docs/direct/fixtures/v1/f.json \
    internal/skills/library/s/SKILL.md cmd/main.go go.mod .github/workflows/ci.yml CONTRIBUTING.md; do
    echo base >"$work/repo/$f"
  done
  g add -A && g commit -qm base
  local base; base=$(g rev-parse HEAD)
  (cd "$work/repo" && eval "$change")
  g add -A && g commit -qm head --allow-empty
  local got; got=$(cd "$work/repo" && bash "$script" "$base" "$(g rev-parse HEAD)" 2>/dev/null)
  if [ "$got" = "code=$want" ]; then echo "ok   $name"; else echo "FAIL $name: got $got want code=$want"; fail=1; fi
}

expect "root README"            false 'echo x >>README.md'
expect "nested docs"            false 'echo x >>docs/guide/b.md'
expect "new nested README"      false 'mkdir -p pkg && echo x >pkg/README.md'
expect "docs deletion"          false 'git rm -q docs/a.md'
expect "docs rename in docs"    false 'git mv docs/a.md docs/guide/a.md'
expect "mixed docs + code"      true  'echo x >>README.md; echo x >>cmd/main.go'
expect "code only"              true  'echo x >>cmd/main.go'
expect "config"                 true  'echo x >>go.mod'
expect "workflow"               true  'echo x >>.github/workflows/ci.yml'
expect "root prose not README"  true  'echo x >>CONTRIBUTING.md'
expect "docs evidence fixture"  true  'echo x >>docs/evidence/x/raw'
expect "direct fixture"         true  'echo x >>docs/direct/fixtures/v1/f.json'
expect "embedded skill"         true  'echo x >>internal/skills/library/s/SKILL.md'
expect "rename docs to code"    true  'git mv docs/a.md cmd/a.go'
expect "delete code"            true  'git rm -q cmd/main.go'
expect "rename code to docs"    true  'git mv cmd/main.go docs/main.md'
expect "newline name in docs"   false 'printf x >"docs/a
b.md"'
# Split on newlines this would read as "src/README.md" + "docs/x.go", both
# docs; it is really src/<dir>/x.go, code.
expect "newline name hides code" true 'mkdir -p "src/README.md
docs" && printf x >"src/README.md
docs/x.go"'
expect "empty diff"             true  ':'

# A pull request is classified as its merge commit against the merge's first
# parent (the base tip). Code that landed on the base branch meanwhile is not
# part of the PR and must not count; the PR's own docs change must.
merge_case() {
  local name=$1 want=$2 pr_change=$3
  rm -rf "$work/repo"; git init -q -b master "$work/repo"
  mkdir -p "$work/repo/docs" "$work/repo/cmd"
  echo base >"$work/repo/docs/a.md"; echo base >"$work/repo/cmd/main.go"
  g add -A && g commit -qm base
  g checkout -qb pr && (cd "$work/repo" && eval "$pr_change") && g add -A && g commit -qm pr
  g checkout -q master && echo moved >>"$work/repo/cmd/main.go" && g commit -qam "base moved on"
  g merge -q --no-ff --no-edit pr
  local got; got=$(cd "$work/repo" && bash "$script" "$(g rev-parse HEAD^1)" "$(g rev-parse HEAD)" 2>/dev/null)
  if [ "$got" = "code=$want" ]; then echo "ok   $name"; else echo "FAIL $name: got $got want code=$want"; fail=1; fi
}
merge_case "PR merge commit, docs"  false 'echo x >>docs/a.md'
merge_case "PR merge commit, code"  true  'echo x >>docs/a.md; echo x >cmd/new.go'

# Pushes compare the old and new tip directly, never via their merge base. A
# force push that drops a code commit and adds only docs still changes code
# relative to what master was, so it must run CI; merge-base (three-dot)
# semantics would see only the docs and skip.
force_push_case() {
  local name=$1 want=$2 new_tip_change=$3
  rm -rf "$work/repo"; git init -q -b master "$work/repo"
  mkdir -p "$work/repo/docs" "$work/repo/cmd"
  echo base >"$work/repo/docs/a.md"; echo base >"$work/repo/cmd/main.go"
  g add -A && g commit -qm A
  local a; a=$(g rev-parse HEAD)
  echo code >>"$work/repo/cmd/main.go" && g commit -qam C
  local before; before=$(g rev-parse HEAD)
  g reset -q --hard "$a"
  (cd "$work/repo" && eval "$new_tip_change") && g add -A && g commit -qm D --allow-empty
  local got; got=$(cd "$work/repo" && bash "$script" "$before" "$(g rev-parse HEAD)" 2>/dev/null)
  if [ "$got" = "code=$want" ]; then echo "ok   $name"; else echo "FAIL $name: got $got want code=$want"; fail=1; fi
}
force_push_case "force push drops code, adds docs" true  'echo x >>docs/a.md'
force_push_case "force push re-adds same code + docs" false 'echo code >>cmd/main.go; echo x >>docs/a.md'

# A pushed revert of a code commit is a code change, even if the revert
# commit's message or a docs edit alongside makes it look like housekeeping.
revert_case() {
  rm -rf "$work/repo"; git init -q -b master "$work/repo"
  mkdir -p "$work/repo/docs" "$work/repo/cmd"
  echo base >"$work/repo/docs/a.md"; echo base >"$work/repo/cmd/main.go"
  g add -A && g commit -qm A
  echo code >>"$work/repo/cmd/main.go" && g commit -qam C
  local before; before=$(g rev-parse HEAD)
  g revert --no-edit HEAD >/dev/null && echo x >>"$work/repo/docs/a.md" && g commit -qam docs
  local got; got=$(cd "$work/repo" && bash "$script" "$before" "$(g rev-parse HEAD)" 2>/dev/null)
  if [ "$got" = "code=true" ]; then echo "ok   revert of code + docs"; else echo "FAIL revert of code + docs: got $got"; fail=1; fi
}
revert_case

# Fail-open cases that never reach a diff.
check() {
  local name=$1; shift
  local got; got=$(cd "$work/repo" && bash "$script" "$@" 2>/dev/null)
  if [ "$got" = "code=true" ]; then echo "ok   $name"; else echo "FAIL $name: got $got"; fail=1; fi
}
head=$(g rev-parse HEAD)
check "no base"         "" "$head"
check "zero base"       0000000000000000000000000000000000000000 "$head"
check "unknown base"    1234567890123456789012345678901234567890 "$head"
check "no head"         "$head" ""
(cd / && got=$(bash "$script" "$head" "$head" 2>/dev/null) && [ "$got" = code=true ] &&
  echo "ok   outside a repository") || { echo "FAIL outside a repository"; fail=1; }

exit $fail
