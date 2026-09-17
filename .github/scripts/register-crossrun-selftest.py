#!/usr/bin/env python3
"""Fixture corpus for register-crossrun.sh, the cross-run step of the Register workflow.

Each case builds a head checkout whose checker has been edited the way a pull request could
edit it, runs register-crossrun.sh against it, and requires the script to reach the right
verdict for the right reason. The script under test is the one the workflow runs, not a
transcription of it, so an edit to the step is an edit to what these cases exercise.

Offline: no event payload and no API call. It runs in the Test workflow, off the pull request
head, and has no part in the trust the step rests on. The corpus that step runs comes from
the base either way, so a pull request that rewrites this file still faces the step.
"""
import os
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "register-crossrun.sh")
CHECKER = open(os.path.join(HERE, "register-lint.py"), encoding="utf-8").read()


def build(root, edit=None, delete=False, corpus_symlink=None):
    """A head checkout, with the checker edited as a pull request might edit it."""
    d = os.path.join(root, "head", ".github", "scripts")
    os.makedirs(d)
    if not delete:
        with open(os.path.join(d, "register-lint.py"), "w", encoding="utf-8") as fh:
            fh.write(edit(CHECKER) if edit else CHECKER)
    if corpus_symlink:
        os.symlink(corpus_symlink, os.path.join(d, "register-lint-selftest.py"))
    return os.path.join(root, "head")


def run(head, root):
    r = subprocess.run(["bash", SCRIPT, head, os.path.join(root, "work"), HERE],
                       capture_output=True, text=True)
    return r.returncode, r.stdout + r.stderr


def drop(needle):
    def edit(text):
        out = text.replace(needle, "", 1)
        if out == text:
            raise AssertionError(f"this corpus edits {needle!r}, which the checker no longer has")
        return out
    return edit


def sub(old, new):
    def edit(text):
        out = text.replace(old, new, 1)
        if out == text:
            raise AssertionError(f"this corpus edits {old!r}, which the checker no longer has")
        return out
    return edit


CASES = [
    # (name, build kwargs, expected failure, a string the output must contain)
    ("the head checker is unchanged", {}, False, "fixtures behave"),
    ("the head drops the co-author pattern",
     {"edit": drop('    r"co-authored-by",\n')}, True, "a co-author trailer"),
    ("the head drops a vendor name, which no fixture pinned before this change",
     {"edit": drop('r"\\bclaude\\b", ')}, True, "a vendor name, claude"),
    ("the head weakens main() and leaves lint() intact",
     {"edit": sub("    if findings:\n", "    if False:\n")}, True, "main() returned 0"),
    ("the head checker exits at import",
     {"edit": lambda t: "import sys\nsys.exit(0)\n" + t}, True, "does not import"),
    ("the head checker is emptied", {"edit": lambda t: ""}, True, "no callable"),
    ("the head deletes the checker", {"delete": True}, True, "removes or renames"),
    ("a hostile corpus path in the head does not take the write",
     {"edit": drop('    r"co-authored-by",\n'), "corpus_symlink": "/dev/null"},
     True, "a co-author trailer"),
]


def main():
    bad = 0
    for name, kwargs, want_fail, needle in CASES:
        root = tempfile.mkdtemp(prefix="register-crossrun-")
        try:
            try:
                head = build(root, **kwargs)
            except AssertionError as exc:
                print("FAIL |", name, "| the case could not be built |", exc)
                bad += 1
                continue
            rc, out = run(head, root)
            failed = rc != 0
            ok = failed == want_fail and needle in out
            bad += 0 if ok else 1
            why = ""
            if not ok:
                why = (f"| exit {rc}, expected {'nonzero' if want_fail else 'zero'}"
                       f"{'' if needle in out else f'; output does not contain {needle!r}'}")
            print(("PASS" if ok else "FAIL"), "|", name, "|",
                  "refused" if failed else "accepted", why)
        finally:
            shutil.rmtree(root, ignore_errors=True)

    print(f"\n{len(CASES) - bad}/{len(CASES)} cases behave")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
