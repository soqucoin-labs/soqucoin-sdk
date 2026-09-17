#!/usr/bin/env bash
# Run the base corpus against the checker a pull request ships.
#
# The corpus resolves the checker from its own directory, so the work is to place the base
# corpus and the head checker together. The directory is created here rather than in the
# head's tree: a head that chose the destination could make the corpus path a symlink to
# /dev/null, take the write, and leave an empty file to run.
#
#   $1  the head checkout
#   $2  a directory this script creates, which must not already exist
#   $3  the base scripts directory
#
# The Register workflow calls this, and so does its corpus, so the cases in
# register-crossrun-selftest.py exercise this file and not a copy of its logic.
set -eu

head=$1
work=$2
base=$3

if [ ! -f "$head/.github/scripts/register-lint.py" ]; then
  echo "::error title=Register check::this pull request removes or renames the register checker, so the base corpus has nothing to run against, and this fails closed."
  exit 1
fi

mkdir "$work"
cp "$head/.github/scripts/register-lint.py" "$work/"
cp "$base/register-lint-selftest.py" "$work/"

if ! python3 "$work/register-lint-selftest.py" > "$work/out" 2>&1; then
  cat "$work/out"
  echo "::error title=Register check::the base corpus does not behave against the checker this pull request ships."
  exit 1
fi
cat "$work/out"

# An exit status on its own is not a result. A run that reached no verdict line did not get
# to the end of the corpus, and its zero says nothing about the checker.
if ! grep -qE '^[0-9]+/[0-9]+ fixtures behave$' "$work/out"; then
  echo "::error title=Register check::the base corpus reached no verdict, so its exit status says nothing about the checker this pull request ships."
  exit 1
fi
