#!/usr/bin/env python3
"""Documentation integrity checks for the Soqucoin SDK.

This exists because the repository has shipped two classes of documentation defect
that were mechanically detectable and caught by no test:

  1. Docs referencing an API that does not exist. The README described
     `tx.Build(...)` as the build-and-sign call for three months. It never
     existed. SECURITY.md documented nine identifiers, none of them real. An
     exchange evaluating the SDK found both before we did.

  2. Broken internal anchor links. A sweep that reworded headings silently
     invalidated every link pointing at them, including in the guide an
     integrator reads first.

Run from anywhere:  python3 scripts/check-docs.py
Exits non-zero on any problem, so it can gate CI.
"""
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def md_files():
    return sorted(list(ROOT.glob("*.md")) + list((ROOT / "docs").glob("*.md")))


def go_packages():
    skip = {"docs", "examples", "scripts"}
    return sorted(
        p.name for p in ROOT.iterdir()
        if p.is_dir() and not p.name.startswith((".", "_"))
        and p.name not in skip and any(p.glob("*.go"))
    )


def exported_identifiers(pkg):
    out = subprocess.run(["go", "doc", "-all", "./" + pkg], cwd=ROOT,
                         capture_output=True, text=True).stdout
    ids = set(re.findall(r"^func ([A-Z]\w*)", out, re.M))
    ids |= set(re.findall(r"^func \([^)]+\) ([A-Z]\w*)", out, re.M))
    ids |= set(re.findall(r"^type ([A-Z]\w*)", out, re.M))
    ids |= set(re.findall(r"^\s+([A-Z]\w*)\s", out, re.M))
    ids |= set(re.findall(r"^(?:var|const)\s+([A-Z]\w*)", out, re.M))
    return ids


def check_api_references():
    """Every pkg.Identifier inside a Go code block must exist in that package."""
    pkgs = go_packages()
    if not pkgs:
        return []
    real = {p: exported_identifiers(p) for p in pkgs}
    pattern = re.compile(r"\b(" + "|".join(pkgs) + r")\.([A-Z]\w*)")
    problems = []
    for f in md_files():
        text = f.read_text()
        for block in re.finditer(r"```go\n(.*?)```", text, re.S):
            body = block.group(1)
            base = text[: block.start()].count("\n") + 1
            # A local variable can shadow a package name, for example
            # `client := electrumx.NewClient(...)`. Those are not package refs,
            # and treating them as such would make this check cry wolf.
            shadowed = set(re.findall(r"\b(\w+)\s*(?::=|,\s*\w+\s*:=)", body))
            shadowed |= set(re.findall(r"\b(\w+)\s+\*?\w+\.\w+\s*[,)]", body))
            for m in pattern.finditer(body):
                pkg, ident = m.group(1), m.group(2)
                if pkg in shadowed or ident in real[pkg]:
                    continue
                line = base + body[: m.start()].count("\n")
                problems.append((f"{f.relative_to(ROOT)}:{line}",
                                 f"{pkg}.{ident}", "no such exported identifier"))
    return problems


def slug(heading):
    """Approximate GitHub's heading-to-anchor transformation."""
    s = re.sub(r"[^\w\s-]", "", heading.strip().lower())
    return re.sub(r"\s+", "-", s)


def check_anchors():
    """Every internal #anchor must match a heading in its target file."""
    anchors = {
        f.name: {slug(m.group(1))
                 for m in re.finditer(r"^#{1,6}\s+(.*)$", f.read_text(), re.M)}
        for f in md_files()
    }
    problems = []
    for f in md_files():
        for m in re.finditer(r"\[[^\]]+\]\(([^)]*?)#([^)]+)\)", f.read_text()):
            target, anchor = m.group(1), m.group(2)
            name = Path(target).name if target else f.name
            if name not in anchors:
                problems.append((str(f.relative_to(ROOT)),
                                 f"{target}#{anchor}", "target file not found"))
            elif anchor not in anchors[name]:
                problems.append((str(f.relative_to(ROOT)),
                                 f"{target}#{anchor}", "no matching heading"))
    return problems


def main():
    problems = check_api_references() + check_anchors()
    if not problems:
        print("check-docs: OK (API references and anchor links all resolve)")
        return 0
    print(f"check-docs: {len(problems)} problem(s)\n")
    for where, what, why in problems:
        print(f"  {where:<42} {what:<46} {why}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
