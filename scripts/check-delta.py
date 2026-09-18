#!/usr/bin/env python3
"""Every function a branch adds or changes is named in its pull request body.

The rule this enforces is gate 12 of the engineering quality gates: a fix
commit gets the same enumeration pass as the design it fixes. Four platform
reviews on pull request 69 each landed at a site a fix commit had just touched,
where a rule was stated at one site and not at its sibling, after two review
rounds had passed the head. The body's site lists covered the rules the design
named and not the rules the fixes introduced. Writing every function the delta
touches into the body is the step that makes the author look at each of them
once more with the sibling question in hand; this check refuses a body that
skipped one.

What is measured. For every non-test Go file the branch changes, each top-level
function or method whose doc comment, signature or body has a changed line,
read from both sides of the diff so that a change made only by deletion counts.
A removed function is reported but not required, since its callers change with
it and are counted there. Files marked `Code generated` are skipped.

How a name satisfies the check. The bare name (`commitRefresh`, `Host`) as a
whole word anywhere in the body, in prose or in backticks. When the repository
declares that bare name more than once (`Close` on several types, `dial` in two
packages), `Type.Method` for a method or `package.Func` for a function is
required, since the bare word cannot tell them apart.

Parsing. The source is read as gofmt leaves it: a declaration starts at a
`func` in column 0 and its body ends at the first `}` in column 0. A raw string
holding a `}` at column 0 would end a function early; none of the library's
non-test files carries one, and the effect is a function counted as two, which
over-reports and never under-reports.

Usage:
  check-delta.py --list                        print the enumeration, exit 0
  check-delta.py --body-file BODY              refuse a body that misses a name
  PR_BODY=... check-delta.py                   the same, from the environment
Ranges: --base (origin/main) and --head (HEAD); the diff is from their merge
base, the change a reviewer is shown.

Exit status is 0 when every name is present, 1 otherwise. check-branch.py runs
this as its third check whenever a body exists, so CI and the read-back before
`gh pr ready` see it without a separate call.
"""

import argparse
import os
import pathlib
import re
import subprocess
import sys

GIT = ["git", "-c", "core.quotePath=false"]
DIFF = ["--no-ext-diff", "--no-textconv", "--ignore-submodules=none"]

# A declaration as gofmt prints it: `func Name(` or `func (r *Type) Name(`,
# with an optional type parameter list on the receiver. Group 1 is the receiver
# type, group 2 the name.
FUNC = re.compile(
    r"^func\s+(?:\(\s*(?:\w+\s+)?\*?\s*(\w+)(?:\[[^\]]*\])?\s*\)\s*)?(\w+)"
)
GENERATED = re.compile(r"^// Code generated .* DO NOT EDIT\.$", re.MULTILINE)
HUNK = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")


class Function:
    __slots__ = ("package", "receiver", "name", "start", "end")

    def __init__(self, package, receiver, name, start, end):
        self.package, self.receiver, self.name = package, receiver, name
        self.start, self.end = start, end  # 1-based, inclusive, doc comment included

    @property
    def qualified(self) -> str:
        return f"{self.receiver}.{self.name}" if self.receiver else f"{self.package}.{self.name}"

    def touches(self, lines: set[int]) -> bool:
        return any(self.start <= n <= self.end for n in lines)


def git(*args: str) -> str:
    return subprocess.run([*GIT, *args], capture_output=True, text=True, check=True).stdout


def is_non_test_go(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go")


def package_of(path: str) -> str:
    parent = pathlib.PurePosixPath(path).parent.name
    return parent or "main"


def functions(source: str, package: str) -> list[Function]:
    """Top-level functions and methods with their line ranges.

    The range runs from the first line of the doc comment (contiguous `//`
    lines directly above the declaration) to the closing `}` in column 0, or to
    the declaration line itself for a one-line body or a bodyless declaration.
    """
    if GENERATED.search(source[:2000]):
        return []
    lines = source.split("\n")
    out: list[Function] = []
    i = 0
    while i < len(lines):
        m = FUNC.match(lines[i])
        if not m:
            i += 1
            continue
        start = i
        while start > 0 and lines[start - 1].startswith("//"):
            start -= 1
        line = lines[i]
        if "{" not in line or line.rstrip().endswith("}"):
            end = i  # bodyless, or the whole body on the declaration line
        else:
            end = i + 1
            while end < len(lines) and not lines[end].startswith("}"):
                end += 1
            end = min(end, len(lines) - 1)
        out.append(Function(package, m.group(1), m.group(2), start + 1, end + 1))
        i = end + 1
    return out


def changed_lines(base: str, head: str) -> list[tuple[str | None, str | None, set[int], set[int]]]:
    """[(old_path, new_path, old changed lines, new changed lines)] for non-test Go.

    From a zero-context diff, so every hunk is exactly the changed lines. A
    path is None on the side where the file does not exist: old_path for a file
    the branch adds, new_path for one it deletes.
    """
    text = git("diff", "-U0", "--no-color", *DIFF, f"{base}...{head}", "--")
    out: list[tuple[str | None, str | None, set[int], set[int]]] = []
    old_path = None
    for line in text.split("\n"):
        if line.startswith("--- "):
            old_path = None if line == "--- /dev/null" else line[6:]
        elif line.startswith("+++ "):
            new_path = None if line == "+++ /dev/null" else line[6:]
            out.append((old_path, new_path, set(), set()))
        elif line.startswith("@@") and out:
            m = HUNK.match(line)
            if not m:
                continue
            a, b = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
            c, d = int(m.group(3)), int(m.group(4) if m.group(4) is not None else 1)
            out[-1][2].update(range(a, a + b))
            out[-1][3].update(range(c, c + d))
    return [r for r in out if is_non_test_go(r[1] if r[1] is not None else r[0])]


def show(rev: str, path: str) -> str:
    return git("show", f"{rev}:{path}")


def enumerate_delta(base: str, head: str):
    """(added, changed, removed): lists of (path, Function).

    A function counts as changed when a changed line on either side of the diff
    falls inside its range: the new side catches additions and edits, the old
    side catches a change made only by deleting lines.
    """
    merge_base = git("merge-base", base, head).strip()
    added, changed, removed = [], [], []
    for old_path, new_path, old_lines, new_lines in changed_lines(base, head):
        new_funcs = functions(show(head, new_path), package_of(new_path)) if new_path else []
        old_funcs = functions(show(merge_base, old_path), package_of(old_path)) if old_path else []
        new_by_name = {f.qualified: f for f in new_funcs}
        old_by_name = {f.qualified: f for f in old_funcs}
        touched: dict[str, Function] = {}
        for f in new_funcs:
            if f.touches(new_lines):
                touched[f.qualified] = f
        for f in old_funcs:
            if f.touches(old_lines):
                if f.qualified in new_by_name:
                    touched.setdefault(f.qualified, new_by_name[f.qualified])
                else:
                    removed.append((old_path, f))
        for q, f in touched.items():
            (changed if q in old_by_name else added).append((new_path, f))
    return added, changed, removed


def bare_counts(head: str) -> dict[str, int]:
    """How many times each bare function name is declared in non-test Go at head."""
    r = subprocess.run([*GIT, "grep", "-h", "-E", "^func ", head, "--", "*.go", ":!*_test.go"],
                       capture_output=True, text=True)
    counts: dict[str, int] = {}
    for line in r.stdout.split("\n"):
        m = FUNC.match(line)
        if m:
            counts[m.group(2)] = counts.get(m.group(2), 0) + 1
    return counts


def required_forms(funcs: list[Function], counts: dict[str, int]) -> dict[Function, str]:
    """The form the body must carry for each function: bare, or qualified when
    the repository declares the bare name more than once."""
    return {f: (f.name if counts.get(f.name, 0) <= 1 else f.qualified) for f in funcs}


def named_in(body: str, form: str) -> bool:
    return re.search(r"(?<![\w])" + re.escape(form) + r"(?![\w])", body) is not None


def check_names(base: str, head: str, body: str) -> tuple[list[str], int]:
    """(problems, total): problems in check-branch's two-level form, a headline
    then indented detail; total is the number of functions added or changed."""
    added, changed, _removed = enumerate_delta(base, head)
    funcs = [f for _, f in added + changed]
    paths = {f: p for p, f in added + changed}
    forms = required_forms(funcs, bare_counts(head))
    missing = sorted((f for f in funcs if not named_in(body, forms[f])),
                     key=lambda f: (paths[f], f.start))
    if not missing:
        return [], len(funcs)
    lines = [
        f"{len(missing)} of the {len(funcs)} functions this branch adds or changes are not "
        "named in the pull request body.",
        "  for each, the sibling that applies the same rule or handles the same value on"
        " another path; name it and the sibling in the body's site list:",
    ]
    added_set = {id(f) for _, f in added}
    for f in missing:
        kind = "added" if id(f) in added_set else "changed"
        lines.append(f"    {forms[f]:<40} {kind:<8} {paths[f]}:{f.start}")
    return lines, len(funcs)


def listing(base: str, head: str) -> str:
    added, changed, removed = enumerate_delta(base, head)
    forms = required_forms([f for _, f in added + changed], bare_counts(head))
    out = []
    for kind, items in (("added", added), ("changed", changed)):
        for p, f in items:
            out.append(f"{kind:<8} {forms[f]:<40} {p}:{f.start}")
    for p, f in removed:
        out.append(f"{'removed':<8} {f.qualified:<40} {p}:{f.start}  (reported, not required)")
    return "\n".join(out)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="origin/main")
    ap.add_argument("--head", default="HEAD")
    ap.add_argument("--body-file", help="the pull request body; CI passes PR_BODY instead")
    ap.add_argument("--list", action="store_true", help="print the enumeration and exit 0")
    args = ap.parse_args()

    for rev in (args.base, args.head):
        try:
            git("rev-parse", "--verify", rev)
        except subprocess.CalledProcessError:
            print(f"check-delta: {rev} not found; fetch it first", file=sys.stderr)
            return 1

    if args.list:
        print(listing(args.base, args.head))
        return 0

    body = os.environ.get("PR_BODY")
    if args.body_file:
        body = pathlib.Path(args.body_file).read_text()
    if body is None:
        print("check-delta: no body. Pass --body-file, set PR_BODY, or use --list to see "
              "what the body must name.", file=sys.stderr)
        return 1

    problems, total = check_names(args.base, args.head, body)
    if not problems:
        print(f"check-delta: OK ({total} functions added or changed, every one named in the body)")
        return 0
    print("check-delta: 1 problem\n")
    for line in problems:
        print(line)
    return 1


if __name__ == "__main__":
    sys.exit(main())
