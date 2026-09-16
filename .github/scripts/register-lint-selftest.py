#!/usr/bin/env python3
"""Fixture corpus for register-lint.py. Offline: no event payload and no API call.

Every negative case below is a text that was published from this repository before the check
existed. Every positive case is one a check has to let through, and each cost something
before it did: a review bot's prose, and a commit message naming the manifest file it changes.
"""
import importlib.util
import os
import sys

# Loading the checker by path would otherwise leave a __pycache__ directory beside it, in a
# directory this repository tracks.
sys.dont_write_bytecode = True

_spec = importlib.util.spec_from_file_location(
    "lint", os.path.join(os.path.dirname(os.path.abspath(__file__)), "register-lint.py"))
mod = importlib.util.module_from_spec(_spec); _spec.loader.exec_module(mod)

TEXTS = [
    # (name, text, must_fail)
    ("a body in register", "Closes the double payment. Costs 44 us per address on loopback.", False),
    ("a generated-by footer", "Closes item 5.\n\n\U0001F916 Generated with Claude Code\n", True),
    ("a co-author trailer", "utxo: persist errors\n\nCo-Authored-By: someone <a@b.c>", True),
    ("a review round named in a body", "Round 1 review (clean-room): nothing blocks.", True),
    ("review triage vocabulary", "Four should-fix items, all addressed.", True),
    ("a release train named in a body", "Rides the soak clock as a freeze candidate.", True),
    ("an em dash in prose", "The reader sees the diff — the whole of it.", True),
    ("the X, not Y tic", "A stale cache is an outage, not an absence of deposits.", True),
    ("a filler adjective", "A comprehensive rewrite of the connection layer.", True),
    ("a figurative verb applied to code", "A refused address does not starve the reconcile.", True),
    ("a commit message naming the file it changes",
     "scripts: declare the guards\n\nEach change is declared in scripts/mutants.json, and "
     "check-mutants.py asks whether one test catches it.", False),
    ("a title in register", "withdraw: one intent builds one transaction", False),
]

PATHS = [
    ("an ordinary test path", "deposit/final_set_staleness_and_errors_test.go", False),
    ("a path named for a review round", "deposit/final_set_round2_test.go", True),
    ("a path named for a review round, underscore", "electrumx/round_2_test.go", True),
    ("a path with a number that is not a round", "crypto/mldsa44_test.go", False),
    ("a source path", "withdraw/engine.go", False),
]

bad = 0
for name, text, must_fail in TEXTS:
    failed = bool(mod.lint(text))
    ok = failed == must_fail
    bad += 0 if ok else 1
    print(("PASS" if ok else "FAIL"), "|", name, "|", "finding" if failed else "clean",
          "" if ok else f"| {mod.lint(text)}")
for name, path, must_fail in PATHS:
    failed = bool(mod.lint_path(path))
    ok = failed == must_fail
    bad += 0 if ok else 1
    print(("PASS" if ok else "FAIL"), "|", name, "|", "finding" if failed else "clean",
          "" if ok else f"| {mod.lint_path(path)}")

total = len(TEXTS) + len(PATHS)
print(f"\n{total - bad}/{total} fixtures behave")
sys.exit(1 if bad else 0)
