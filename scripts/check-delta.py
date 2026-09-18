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
whole word anywhere in the body, in prose or in backticks. Where the repository
declares that bare name more than once (`Call` on a `Client` in both `rpc` and
`electrumx`, `String` on three types), the qualified form is required:
`Type.Method` for a method, `<dir>.Func` for a function, with the directory
holding the file rather than the Go package name, so a `main` package reads as
its directory. Where even `Type.Method` is declared more than once, as
`Client.Call` is, `<dir>.Type.Method` is required.

Parsing. A small scanner over the source skips comments, interpreted strings,
raw strings and rune literals, so a brace or a `func` inside any of them is
not a brace or a declaration. A declaration is `func` in column 0; its body
begins at the first `{` outside parentheses and ends at the matching `}`, so a
signature spanning several lines and a body holding any text are read as Go
reads them. A declaration whose line ends outside parentheses before any `{`
has no body. The doc comment is the run of `//` lines directly above.

Reading the diff. The file list comes from `--name-status -z`, so a path with a
space or a rename is exact, and only hunk headers (`@@`) are read from a
zero-context diff, so no content line can pose as a file header and a reader's
diff configuration cannot change what is read.

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
DIFF = ["--no-ext-diff", "--no-textconv", "--ignore-submodules=none", "-M"]

# A declaration line as gofmt prints it: `func Name(` or `func (r *Type) Name(`,
# with an optional type parameter list on the receiver. Group 1 is the receiver
# type, group 2 the name.
FUNC = re.compile(
    r"^func\s+(?:\(\s*(?:\w+\s+)?\*?\s*(\w+)(?:\[[^\]]*\])?\s*\)\s*)?(\w+)"
)
GENERATED = re.compile(r"^// Code generated .* DO NOT EDIT\.$", re.MULTILINE)
HUNK = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")


class Function:
    __slots__ = ("directory", "receiver", "name", "start", "end")

    def __init__(self, directory, receiver, name, start, end):
        self.directory, self.receiver, self.name = directory, receiver, name
        self.start, self.end = start, end  # 1-based, inclusive, doc comment included

    @property
    def qualified(self) -> str:
        return f"{self.receiver}.{self.name}" if self.receiver else f"{self.directory}.{self.name}"

    @property
    def full(self) -> str:
        return f"{self.directory}.{self.qualified}" if self.receiver else self.qualified

    def touches(self, lines: set[int]) -> bool:
        return any(self.start <= n <= self.end for n in lines)


def git(*args: str) -> str:
    return subprocess.run([*GIT, *args], capture_output=True, text=True, check=True).stdout


def is_non_test_go(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go")


def directory_of(path: str) -> str:
    return pathlib.PurePosixPath(path).parent.name or "."


def tokens(source: str) -> list[tuple[str, int]]:
    """The tokens the declaration walk needs: `func` in column 0, the four
    brackets, and line ends, each with its line number. Comments, interpreted
    strings, raw strings and rune literals are consumed whole."""
    out: list[tuple[str, int]] = []
    i, n, line, column0 = 0, len(source), 1, True
    while i < n:
        c = source[i]
        if c == "\n":
            out.append(("nl", line))
            line, i, column0 = line + 1, i + 1, True
            continue
        if source.startswith("//", i):
            j = source.find("\n", i)
            i = n if j < 0 else j
        elif source.startswith("/*", i):
            j = source.find("*/", i + 2)
            j = n if j < 0 else j + 2
            line += source.count("\n", i, j)
            i = j
        elif c == "`":
            j = source.find("`", i + 1)
            j = n if j < 0 else j + 1
            line += source.count("\n", i, j)
            i = j
        elif c in "\"'":
            j = i + 1
            while j < n and source[j] not in (c, "\n"):
                j += 2 if source[j] == "\\" else 1
            i = j + 1
        elif column0 and source.startswith("func", i) and i + 4 < n and source[i + 4] in " \t(":
            out.append(("func", line))
            i += 4
        else:
            if c in "{}()":
                out.append((c, line))
            i += 1
        column0 = False
    return out


def functions(source: str, directory: str) -> list[Function]:
    """Top-level functions and methods with their line ranges.

    The range runs from the first line of the doc comment (contiguous `//`
    lines directly above the declaration) to the `}` that closes the body, or
    to the line the declaration ends on when it has no body.
    """
    if GENERATED.search(source[:2000]):
        return []
    lines = source.split("\n")
    toks = tokens(source)
    out: list[Function] = []
    k = 0
    while k < len(toks):
        if toks[k][0] != "func":
            k += 1
            continue
        decl_line = toks[k][1]
        m = FUNC.match(lines[decl_line - 1])
        end = decl_line
        paren = 0
        j = k + 1
        body = False
        while j < len(toks):
            kind, ln = toks[j]
            if kind == "(":
                paren += 1
            elif kind == ")":
                paren -= 1
            elif kind == "{" and paren == 0:
                body = True
                break
            elif kind in ("nl", "func") and paren == 0:
                end = ln if kind == "nl" else ln - 1
                break
            j += 1
        if body:
            depth = 0
            while j < len(toks):
                kind, ln = toks[j]
                if kind == "{":
                    depth += 1
                elif kind == "}":
                    depth -= 1
                    if depth == 0:
                        end = ln
                        # A `{` before the line ends is a body after a result
                        # type written as a struct or interface literal.
                        if j + 1 < len(toks) and toks[j + 1][0] == "{":
                            j += 1
                            continue
                        break
                j += 1
            else:
                end = len(lines)
        start = decl_line
        while start > 1 and lines[start - 2].startswith("//"):
            start -= 1
        if m:
            out.append(Function(directory, m.group(1), m.group(2), start, end))
        k = j + 1
    return out


def changed_files(base: str, head: str) -> list[tuple[str | None, str | None]]:
    """[(old_path, new_path)] for the non-test Go files the branch changes; a
    path is None on the side where the file does not exist."""
    fields = git("diff", "--name-status", "-z", *DIFF, f"{base}...{head}", "--").split("\0")
    out: list[tuple[str | None, str | None]] = []
    i = 0
    while i < len(fields) and fields[i]:
        status = fields[i][0]
        if status in "RC":
            old, new = fields[i + 1], fields[i + 2]
            i += 3
        else:
            old = new = fields[i + 1]
            i += 2
            if status == "A":
                old = None
            elif status == "D":
                new = None
        if is_non_test_go(new if new is not None else old):
            out.append((old, new))
    return out


def hunks(base: str, head: str, *paths: str) -> tuple[set[int], set[int]]:
    """(old changed lines, new changed lines) for one file, from the hunk
    headers of a zero-context diff; every hunk is exactly the changed lines."""
    text = git("diff", "-U0", "--no-color", "--inter-hunk-context=0", *DIFF,
               f"{base}...{head}", "--", *paths)
    old_lines: set[int] = set()
    new_lines: set[int] = set()
    for line in text.split("\n"):
        m = HUNK.match(line)
        if not m:
            continue
        a, b = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
        c, d = int(m.group(3)), int(m.group(4) if m.group(4) is not None else 1)
        old_lines.update(range(a, a + b))
        new_lines.update(range(c, c + d))
    return old_lines, new_lines


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
    for old_path, new_path in changed_files(base, head):
        old_lines, new_lines = hunks(base, head, *(p for p in (old_path, new_path) if p))
        new_funcs = functions(show(head, new_path), directory_of(new_path)) if new_path else []
        old_funcs = functions(show(merge_base, old_path), directory_of(old_path)) if old_path else []
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


def declarations(head: str) -> dict[str, int]:
    """How many times each name form is declared in non-test Go at head: the
    bare name, the qualified form and the full form all counted."""
    # `-z` puts a NUL after the path, so a path holding a colon is still exact:
    # each output line is `<head>:<path>NUL<line text>`.
    r = subprocess.run([*GIT, "grep", "-z", "-E", "^func ", head, "--", "*.go", ":!*_test.go"],
                       capture_output=True, text=True)
    counts: dict[str, int] = {}
    for line in r.stdout.split("\n"):
        where, sep, text = line.partition("\0")
        m = FUNC.match(text) if sep else None
        if not m:
            continue
        path = where.partition(":")[2]
        f = Function(directory_of(path), m.group(1), m.group(2), 0, 0)
        for form in (f.name, f.qualified, f.full):
            counts[form] = counts.get(form, 0) + 1
    return counts


def required_forms(funcs: list[Function], counts: dict[str, int]) -> dict[Function, str]:
    """The form the body must carry for each function: the shortest form the
    repository declares only once."""
    out = {}
    for f in funcs:
        for form in (f.name, f.qualified, f.full):
            if counts.get(form, 0) <= 1:
                break
        out[f] = form
    return out


def named_in(body: str, form: str) -> bool:
    return re.search(r"(?<![\w])" + re.escape(form) + r"(?![\w])", body) is not None


def check_names(base: str, head: str, body: str) -> tuple[list[str], int]:
    """(problems, total): problems in check-branch's two-level form, a headline
    then indented detail; total is the number of functions added or changed."""
    added, changed, _removed = enumerate_delta(base, head)
    funcs = [f for _, f in added + changed]
    paths = {f: p for p, f in added + changed}
    forms = required_forms(funcs, declarations(head))
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
    forms = required_forms([f for _, f in added + changed], declarations(head))
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
