#!/usr/bin/env python3
"""Documentation integrity checks for the Soqucoin SDK.

Documentation drifts from code silently: no compiler reads a Markdown fence, so a
snippet can name a function that no longer exists and every other CI job stays
green. This closes three of those gaps mechanically.

  1. API references. Every `pkg.Identifier` in a Go code block is resolved
     against `go doc -all` for that package, so a renamed or removed export is
     caught in the docs that call it.

  2. Snippet compilation. Every self-contained `package main` block is built
     against this checkout. Naming a real API is not the same as working: missing
     imports and undeclared variables surface here.

  3. Anchor links. Every internal `#anchor` is resolved against the target file's
     headings, so rewording a heading cannot quietly break the links to it.

Run from anywhere:  python3 scripts/check-docs.py
Exits non-zero on any problem, so it can gate CI.
"""
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def _gomod(key, default):
    for line in (ROOT / "go.mod").read_text().splitlines():
        if line.startswith(key + " "):
            return line.split(None, 1)[1].strip()
    return default


MODULE = _gomod("module", "github.com/soqucoin-labs/soqucoin-sdk")


def go_version():
    return _gomod("go", "1.21")


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


def check_snippets_compile():
    """Every self-contained `package main` Go snippet must actually build.

    Fragments cannot be compiled and are left to check_api_references. A snippet
    opts in simply by being a whole program.
    """
    import shutil
    import tempfile

    problems = []
    workdir = Path(tempfile.mkdtemp(prefix="soq-checkdocs-"))
    try:
        for f in md_files():
            text = f.read_text()
            for i, block in enumerate(re.finditer(r"```go\n(.*?)```", text, re.S)):
                body = block.group(1)
                if not body.lstrip().startswith("package main"):
                    continue
                line = text[: block.start()].count("\n") + 1
                d = workdir / f"{f.stem}_{i}"
                d.mkdir()
                (d / "main.go").write_text(body)
                # Build against THIS checkout, not a published tag, so the check
                # fails on the commit that breaks a snippet rather than one release later.
                (d / "go.mod").write_text(
                    f"module docsnippet\n\ngo {go_version()}\n\n"
                    f"require {MODULE} v0.0.0\n\nreplace {MODULE} => {ROOT}\n")
                subprocess.run(["go", "mod", "tidy"], cwd=d,
                               capture_output=True, text=True)
                r = subprocess.run(["go", "build", "-o", "/dev/null", "./main.go"],
                                   cwd=d, capture_output=True, text=True)
                if r.returncode != 0:
                    # go reports "./main.go:LINE:COL: msg"; remap to the doc line.
                    for err in r.stderr.strip().splitlines():
                        m = re.match(r"\./main\.go:(\d+):\d+:\s*(.*)", err.strip())
                        if m:
                            problems.append((f"{f.relative_to(ROOT)}:{line + int(m.group(1))}",
                                             "snippet", m.group(2)))
                        elif err.strip() and not err.startswith("#"):
                            problems.append((f"{f.relative_to(ROOT)}:{line}",
                                             "snippet", err.strip()))
    finally:
        shutil.rmtree(workdir, ignore_errors=True)
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
    problems = check_api_references() + check_snippets_compile() + check_anchors()
    if not problems:
        print("check-docs: OK (snippets compile, API references and anchors resolve)")
        return 0
    print(f"check-docs: {len(problems)} problem(s)\n")
    for where, what, why in problems:
        print(f"  {where:<42} {what:<46} {why}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
