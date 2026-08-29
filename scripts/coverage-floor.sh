#!/usr/bin/env bash
#
# Enforce a per-package test coverage floor on internal/plugins/*.
#
# Why plugins and not everything: a plugin is where this bot decides to delete
# a channel, jail a member or remove a message, it is the unit CONTRIBUTING
# tells you to add one of per milestone, and it is the thing somebody will
# write next. Holding the shared libraries to the same bar is a separate
# argument with a separate cost, and picking one number for both would mean
# picking the lower one.
#
# Why a baseline rather than one flat number: the floor that would pass every
# existing plugin today is low enough to be meaningless. So the floor applies
# in full to anything not listed in the baseline below, which is every plugin
# written from here on, and the listed ones are held to what they already
# manage. That makes this a ratchet: existing coverage can only go up, and a
# new plugin arrives at the real bar with no way to opt out except editing
# this file in a diff somebody reviews.
#
# Reads a coverage profile rather than running the tests itself, so CI pays
# for one test run and this is arithmetic on the result.
#
# Usage: scripts/coverage-floor.sh <coverage-profile>

set -euo pipefail

readonly FLOOR=70

# Packages already below the floor when it was introduced, at the coverage
# they had. These are ceilings on regression, not permission to stay: raising
# one of these means lowering the number here in the same PR, and CI says so
# when you do. Delete an entry once its package clears FLOOR on its own.
#
# ping is a two-function example plugin with no test file. It is listed at 0
# rather than exempted, so the day it grows real behaviour the floor is what
# it has to clear.
baseline() {
  case "$1" in
    github.com/6586x57890143/merlin/internal/plugins/adminconfig) echo 37 ;;
    github.com/6586x57890143/merlin/internal/plugins/roles)       echo 40 ;;
    github.com/6586x57890143/merlin/internal/plugins/rotation)    echo 51 ;;
    github.com/6586x57890143/merlin/internal/plugins/ping)        echo 0  ;;
    *) echo "" ;;
  esac
}

profile="${1:-}"
if [[ -z "$profile" || ! -f "$profile" ]]; then
  echo "usage: $0 <coverage-profile>" >&2
  exit 2
fi

# go tool cover reports per function; sum the statement counts per package
# from the profile directly instead, which is what `go test -cover` prints and
# is the number a reader will recognise.
#
# Profile lines are "pkg/file.go:startLine.col,endLine.col numStatements count".
# Coverage is covered statements over total statements, per package.
declare -A total covered
while read -r line; do
  [[ "$line" == mode:* ]] && continue
  loc="${line%% *}"
  rest="${line#* }"
  stmts="${rest%% *}"
  count="${rest##* }"
  pkg="${loc%/*}"
  total["$pkg"]=$(( ${total["$pkg"]:-0} + stmts ))
  if [[ "$count" != "0" ]]; then
    covered["$pkg"]=$(( ${covered["$pkg"]:-0} + stmts ))
  fi
done < "$profile"

fail=0
stale=0

for pkg in $(printf '%s\n' "${!total[@]}" | sort); do
  case "$pkg" in
    */internal/plugins/*) ;;
    *) continue ;;
  esac

  t=${total["$pkg"]}
  c=${covered["$pkg"]:-0}
  (( t == 0 )) && continue
  pct=$(( c * 100 / t ))

  base="$(baseline "$pkg")"
  short="${pkg#github.com/6586x57890143/merlin/}"

  if [[ -n "$base" ]]; then
    if (( pct < base )); then
      echo "::error::$short coverage ${pct}% is below its recorded baseline of ${base}%. Coverage here may go up, never down."
      fail=1
    elif (( pct >= FLOOR )); then
      echo "::error::$short coverage is now ${pct}%, at or above the ${FLOOR}% floor. Remove its baseline entry in scripts/coverage-floor.sh."
      stale=1
    elif (( pct > base )); then
      echo "::error::$short coverage rose to ${pct}% from a baseline of ${base}%. Raise the baseline in scripts/coverage-floor.sh so it cannot slip back."
      stale=1
    else
      echo "ok  $short ${pct}% (held at its baseline of ${base}%)"
    fi
    continue
  fi

  if (( pct < FLOOR )); then
    echo "::error::$short coverage ${pct}% is below the ${FLOOR}% floor for plugins. Add tests, or, if this is deliberate and reviewed, a baseline entry in scripts/coverage-floor.sh."
    fail=1
  else
    echo "ok  $short ${pct}%"
  fi
done

if (( fail )); then
  echo "::error::plugin coverage floor not met"
  exit 1
fi
if (( stale )); then
  echo "::error::scripts/coverage-floor.sh is out of date; see above"
  exit 1
fi
echo "plugin coverage floor (${FLOOR}%) met"
