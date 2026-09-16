#!/usr/bin/env python3
"""Fixture corpus for the cross-run step in .github/workflows/register.yml.

The step runs the base corpus against the checker a pull request ships. This builds four
head checkers in a scratch directory and applies the step to each, so what the step does is
a fixture rather than a claim in a comment. Offline: no event payload and no API call.

It runs in the Test workflow, off the pull request head, and has no part in the trust that
the step itself rests on. A pull request that neuters this file still faces the step, whose
corpus comes from the base.
"""
import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True

HERE = os.path.dirname(os.path.abspath(__file__))
CHECKER = os.path.join(HERE, "register-lint.py")
CORPUS = os.path.join(HERE, "register-lint-selftest.py")


def step(head_dir):
    """The step, in Python: refuse an absent checker, else run the base corpus against it."""
    if not os.path.exists(os.path.join(head_dir, "register-lint.py")):
        return 1, "the step refuses: this pull request removes or renames the checker"
    shutil.copy(CORPUS, head_dir)
    r = subprocess.run([sys.executable, os.path.join(head_dir, "register-lint-selftest.py")],
                       capture_output=True, text=True)
    tail = [l for l in r.stdout.splitlines() if l.startswith("FAIL") or "fixtures behave" in l]
    return r.returncode, " / ".join(tail) or (r.stderr.strip() or "no output").splitlines()[-1]


def head(tmp, checker_edit=None, corpus_edit=None, delete_checker=False):
    d = tempfile.mkdtemp(dir=tmp)
    if not delete_checker:
        text = open(CHECKER, encoding="utf-8").read()
        open(os.path.join(d, "register-lint.py"), "w", encoding="utf-8").write(
            checker_edit(text) if checker_edit else text)
    if corpus_edit:
        text = open(CORPUS, encoding="utf-8").read()
        open(os.path.join(d, "register-lint-selftest.py"), "w", encoding="utf-8").write(
            corpus_edit(text))
    return d


def drop_trailer_pattern(text):
    out = text.replace('    r"co-authored-by",\n', "")
    assert out != text, "the pattern this proof edits is no longer in the checker"
    return out


def drop_trailer_fixture(text):
    out = "\n".join(l for l in text.splitlines() if "a co-author trailer" not in l)
    assert out != text, "the fixture this proof edits is no longer in the corpus"
    return out


def main():
    tmp = tempfile.mkdtemp(prefix="register-crossrun-")
    cases = [
        ("the head checker is unchanged", head(tmp), 0),
        ("the head drops one attribution pattern", head(tmp, checker_edit=drop_trailer_pattern), 1),
        ("the head drops the pattern and its own fixture together",
         head(tmp, checker_edit=drop_trailer_pattern, corpus_edit=drop_trailer_fixture), 1),
        ("the head deletes the checker", head(tmp, delete_checker=True), 1),
    ]
    bad = 0
    for name, d, want in cases:
        rc, detail = step(d)
        ok = (rc != 0) == (want != 0)
        bad += 0 if ok else 1
        print(("PASS" if ok else "FAIL"), "|", name, "| exit", rc, "|", detail)

    # The third case is worth seeing twice: judged by its own corpus and its own checker,
    # which is what the check amounted to before this step, the same head reads clean.
    spec = importlib.util.spec_from_file_location(
        "head_checker", os.path.join(cases[2][1], "register-lint.py"))
    head_checker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(head_checker)
    verdict = head_checker.lint("x\n\nCo-Authored-By: a <a@b.c>")
    print("\nthe same head, judged by its own corpus and its own checker:",
          "finding" if verdict else "clean")

    shutil.rmtree(tmp)
    print(f"\n{len(cases) - bad}/{len(cases)} cases behave")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
