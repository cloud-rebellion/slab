#!/usr/bin/env bash
# Every check CI runs, in one script you can also run yourself:
#
#   bash scripts/ci.sh
#
# Run it before opening a pull request and CI will not tell you anything new.
#
# WHY THE LOGIC LIVES HERE AND NOT IN .github/workflows/ci.yml
#
# Two reasons, and the second is not obvious.
#
# 1. A contributor can run it. Checks that only exist inside a workflow file are
#    checks you cannot reproduce locally, so you discover them after pushing.
#
# 2. It keeps .github/workflows/ci.yml STABLE, which is what makes automated
#    publishing possible at all. This repository is a filtered mirror published
#    over a repository deploy key, and GitHub does not allow a deploy key (or a
#    token without the `workflow` scope) to create or update anything under
#    .github/workflows/. A check that lived in the workflow file would break the
#    publish at the FINAL push, after every gate had already passed, with a raw
#    git error that reads like a mystery. With the steps here, changing a check
#    touches an ordinary file the deploy key may write.
#
# Knobs:
#   ATOMICSITE_CI_SKIP_TOOLS=1   skip the tool-fetching scans (gitleaks, govulncheck)
#   ATOMICSITE_CI_SKIP_FRONTEND=1 skip the frontend build (needs bun + network)
set -uo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null)" 2>/dev/null || true
[ -f go.mod ] || cd atomicsite 2>/dev/null || true
[ -f go.mod ] || { echo "error: run this from the module root (the directory holding go.mod)" >&2; exit 1; }

FAILED=()
SKIPPED=()
# Exit code 99 means "this check did not run". It is distinct from pass and from
# fail on purpose: a skipped security scan that prints ok is worse than no scan at
# all, because it looks like evidence.
step() {
  local name="$1" rc; shift
  printf '\n=== %s ===\n' "$name"
  "$@"; rc=$?
  case "$rc" in
    0)  printf 'ok: %s\n' "$name" ;;
    99) printf 'SKIPPED: %s\n' "$name"; SKIPPED+=("$name") ;;
    *)  printf 'FAIL: %s\n' "$name" >&2; FAILED+=("$name") ;;
  esac
}

tool() {
  local bin="$1" mod="$2" path
  if command -v "$bin" >/dev/null 2>&1; then command -v "$bin"; return 0; fi
  if [ "${ATOMICSITE_CI_SKIP_TOOLS:-0}" = "1" ]; then return 99; fi
  go install "$mod" >/dev/null 2>&1 || { echo "could not install $bin from $mod (offline?)" >&2; return 1; }
  path="$(go env GOPATH)/bin/$bin"
  [ -x "$path" ] || { echo "installed $bin but $path is not executable" >&2; return 1; }
  printf '%s\n' "$path"
}

# gofmt is the diff-noise gate, not a style opinion: one unformatted struct
# literal turns an unrelated review into a whitespace hunt. Tracked files only, so
# a vendored tree lying around cannot fail the run.
fmt_check() {
  local unformatted files
  if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
    files=$(git ls-files '*.go')
  else
    files=$(find . -name '*.go' -not -path './.git/*')
  fi
  [ -n "$files" ] || { echo "no Go files found" >&2; return 1; }
  unformatted=$(printf '%s\n' "$files" | xargs gofmt -l)
  if [ -n "$unformatted" ]; then
    echo "These files are not gofmt-clean:" >&2
    echo "$unformatted" >&2
    echo "Fix with: gofmt -w \$(git ls-files '*.go')" >&2
    return 1
  fi
  echo "every tracked Go file is gofmt-clean"
}

# The published binary embeds the built frontend, so a frontend that does not
# build is a server that ships a broken UI while every Go check stays green.
frontend_build() {
  [ -f frontend/package.json ] || { echo "no frontend/package.json"; return 0; }
  if [ "${ATOMICSITE_CI_SKIP_FRONTEND:-0}" = "1" ]; then return 99; fi
  command -v bun >/dev/null 2>&1 || { echo "bun is not installed (this repo is bun-only)" >&2; return 1; }
  ( cd frontend && bun install --frozen-lockfile >/dev/null 2>&1 && bun run build >/dev/null 2>&1 )
}

secret_scan() {
  local gl; gl="$(tool gitleaks github.com/zricethezav/gitleaks/v8@latest)" || return $?
  [ "$gl" = "99" ] && return 99
  "$gl" detect --no-git --redact --exit-code 1 2>&1
}

vuln_scan() {
  local gv; gv="$(tool govulncheck golang.org/x/vuln/cmd/govulncheck@latest)" || return $?
  [ "$gv" = "99" ] && return 99
  "$gv" ./...
}

step "build"                     go build ./...
step "vet"                       go vet ./...
step "gofmt"                     fmt_check
step "tests (RUN, not compiled)" go test ./... -count=1
step "frontend build (what the binary embeds)" frontend_build
step "secret scan (working tree)" secret_scan
step "vulnerability scan (govulncheck)" vuln_scan

printf '\n'
if [ ${#SKIPPED[@]} -gt 0 ]; then
  printf 'ci: %d check(s) DID NOT RUN: %s\n' "${#SKIPPED[@]}" "$(IFS=', '; echo "${SKIPPED[*]}")"
fi
if [ ${#FAILED[@]} -gt 0 ]; then
  printf 'ci: %d check(s) failed: %s\n' "${#FAILED[@]}" "$(IFS=', '; echo "${FAILED[*]}")" >&2
  exit 1
fi
if [ ${#SKIPPED[@]} -gt 0 ]; then
  printf 'ci: every check that RAN passed, but some did not run (see above)\n'
  exit 0
fi
printf 'ci: all checks passed\n'
