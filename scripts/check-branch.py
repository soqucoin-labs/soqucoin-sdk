#!/usr/bin/env python3
"""Mechanical checks on a branch before its pull request opens.

Neither check is about taste. Both answer in a second a question a reading can
miss.

1. The branch is current with main, decided by comparing the patch text of
   the two diffs rather than the ancestry or their line counts. The repository
   uses merge commits (squash and rebase are off), so `git merge-base
   --is-ancestor` is meaningful again: a merge commit is an ancestor of both
   sides. The patch-text check survives the change of method because it tests
   what matters regardless of how main advances: the diff a reviewer is shown
   (three dot, from the merge base) must equal the diff that will land (two
   dot, against main's tip). When the two differ, the branch is behind and
   part of what is on screen is already on main. A branch still has to merge
   main before opening.

2. The change is one mechanism, measured in lines of non-test Go. The override
   is a line in the pull request body, `ALLOW_LARGE_DIFF: <why it is not
   separable>`. It lives there for two reasons: the reviewer reads the reason
   where the reason applies, and CI can read it too, which an environment
   variable set on a developer's machine cannot. Before the pull request
   exists there is no body, so a local run takes ALLOW_LARGE_DIFF=1 and prints
   what the body will have to carry. A marker with no reason behind it fails.

Exit status is 0 when every check passes, 1 otherwise.
"""

import argparse
import os
import pathlib
import re
import subprocess
import sys

DEFAULT_MAX_LINES = 600

# The size override, as it appears in a pull request body. A marker on its own
# records that the author reached the gate. The reason after it is what a
# reviewer can weigh, so a line with nothing behind it fails.
OVERRIDE = re.compile(r"^ALLOW_LARGE_DIFF:[ \t]*(\S.*)$", re.MULTILINE)
MIN_REASON_CHARS = 40


# Paths unquoted, and no user diff driver or textconv between the gate and the
# content it compares: the answer must come from the repository, not from
# whatever is in the reader's git config.
GIT = ["git", "-c", "core.quotePath=false"]
DIFF = ["--no-ext-diff", "--no-textconv", "--ignore-submodules=none"]


def git(*args: str) -> str:
    return subprocess.run(
        [*GIT, *args], capture_output=True, text=True, check=True
    ).stdout


def numstat(rev_range: str) -> dict[str, tuple[int, int]]:
    """Return {path: (added, removed)} for a diff range.

    Read from `-z` output because the plain form renders a detected rename as
    one record with a combined path, `dir/{old.go => new.go}`. That string is
    not a path and does not end in `.go`, so every line of a renamed file would
    be dropped from the budget: a rewrite carried in on a rename would measure
    zero. Under `-z` a rename is `added TAB removed TAB NUL old NUL new NUL`,
    and the new path is the one the change lands on.
    """
    out: dict[str, tuple[int, int]] = {}
    fields = git("diff", "--numstat", "-z", *DIFF, rev_range).split("\0")
    i = 0
    while i < len(fields):
        record, i = fields[i], i + 1
        parts = record.split("\t")
        if len(parts) != 3:
            continue
        added, removed, path = parts
        if path == "":
            if i + 1 >= len(fields):
                break
            path, i = fields[i + 1], i + 2
        if added == "-" or removed == "-":
            continue  # binary
        out[path] = (int(added), int(removed))
    return out


def patch(rev_range: str, *paths: str) -> str:
    """The full patch text for a range. Binary files appear as a marker line.

    The `index` line carries the blob hashes, so two different pre-image trees
    cannot produce the same text even where the body is only a binary marker.
    """
    return git("diff", "--no-color", *DIFF, rev_range, "--", *paths)


def touched(rev_range: str) -> set[str]:
    """Every path in a diff, including the ones numstat reports as binary."""
    return {p for p in git("diff", "--name-only", "-z", *DIFF,
                           rev_range).split("\0") if p}


def is_non_test_go(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go")


def check_current(base: str) -> list[str]:
    """The rendered diff must be the diff that lands.

    Compared as patch text. Line counts are not enough: a branch that edits a
    line main has also edited shows 1 added and 1 removed on both sides while
    the content differs, and landing it reverts main. Counts also drop the
    files git reports as binary, which hides a branch stale only in those.
    """
    if patch(f"{base}...HEAD") == patch(f"{base}..HEAD"):
        return []
    three, two = numstat(f"{base}...HEAD"), numstat(f"{base}..HEAD")
    three_p, two_p = touched(f"{base}...HEAD"), touched(f"{base}..HEAD")
    extra = sorted(three_p - two_p)
    changed = sorted(
        p for p in three_p & two_p
        if patch(f"{base}...HEAD", p) != patch(f"{base}..HEAD", p)
    )
    lines = [
        f"the branch is behind {base}: what a reviewer sees is not what will land.",
        f"  rendered (three dot): {len(three_p)} files, "
        f"+{sum(a for a, _ in three.values())}/-{sum(r for _, r in three.values())}"
        " text lines",
        f"  landing  (two dot):   {len(two_p)} files, "
        f"+{sum(a for a, _ in two.values())}/-{sum(r for _, r in two.values())}"
        " text lines",
    ]
    if extra:
        lines.append(f"  files in the rendered diff only: {', '.join(extra[:8])}"
                     + (" ..." if len(extra) > 8 else ""))
    missing = sorted(two_p - three_p)
    if missing:
        lines.append(f"  files in the landing diff only: {', '.join(missing[:8])}"
                     + (" ..." if len(missing) > 8 else ""))
    if changed:
        lines.append(f"  files whose content differs: {', '.join(changed[:8])}"
                     + (" ..." if len(changed) > 8 else ""))
    lines.append(f"  fix: git merge {base}   (the trees are what matter,"
                 " not the ancestry)")
    return lines


def check_size(base: str, max_lines: int, body: str | None) -> list[str]:
    """One mechanism per pull request, measured in non-test Go.

    `body` is the pull request body when one exists (CI passes it through the
    PR_BODY environment variable, a local run through --body-file) and None
    otherwise. When it exists it is the only thing that can override the
    budget: the environment variable is for the run that happens before the
    pull request does.
    """
    two = numstat(f"{base}..HEAD")
    total = sum(a + r for p, (a, r) in two.items() if is_non_test_go(p))
    if total <= max_lines:
        return []
    if body is not None:
        m = OVERRIDE.search(body)
        reason = m.group(1).strip() if m else ""
        if len(reason) >= MIN_REASON_CHARS:
            print(f"note: {total} lines of non-test Go, over the {max_lines} "
                  f"budget; the body gives a reason: {reason}")
            return []
        if m:
            return [
                f"{total} lines of non-test Go, over the {max_lines} budget, and the "
                "body's ALLOW_LARGE_DIFF line carries no reason.",
                f"  say in at least {MIN_REASON_CHARS} characters why the change "
                "does not separate.",
            ]
    elif os.environ.get("ALLOW_LARGE_DIFF") == "1":
        print(f"note: {total} lines of non-test Go, over the {max_lines} budget; "
              "ALLOW_LARGE_DIFF=1 set. The pull request body must carry "
              "`ALLOW_LARGE_DIFF: <why it is not separable>` or this check fails "
              "in CI.")
        return []
    biggest = sorted(
        ((a + r, p) for p, (a, r) in two.items() if is_non_test_go(p)), reverse=True
    )
    return [
        f"{total} lines of non-test Go, over the {max_lines} budget: split before opening.",
        "  largest: " + ", ".join(f"{p} ({n})" for n, p in biggest[:5]),
        "  or put `ALLOW_LARGE_DIFF: <why it is not separable>` in the pull request body.",
    ]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="origin/main")
    ap.add_argument("--max-lines", type=int, default=DEFAULT_MAX_LINES)
    ap.add_argument("--body-file", help="the pull request body; CI passes PR_BODY instead")
    args = ap.parse_args()

    body = os.environ.get("PR_BODY")
    if args.body_file:
        body = pathlib.Path(args.body_file).read_text()

    try:
        git("rev-parse", "--verify", args.base)
    except subprocess.CalledProcessError:
        print(f"check-branch: {args.base} not found; fetch it first", file=sys.stderr)
        return 1

    problems = check_current(args.base) + check_size(args.base, args.max_lines, body)
    if not problems:
        print(f"check-branch: OK (current with {args.base}, within the line budget)")
        return 0
    print(f"check-branch: {len([p for p in problems if not p.startswith('  ')])} problem(s)\n")
    for line in problems:
        print(line)
    return 1


if __name__ == "__main__":
    sys.exit(main())
