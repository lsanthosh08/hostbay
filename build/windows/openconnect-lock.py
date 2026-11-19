#!/usr/bin/env python3
"""Rewrites the MSYS2 lines of build/windows/openconnect-packages.txt.

bundle-openconnect.sh builds OpenConnect on the Windows release runner from
exactly the MSYS2 packages that file lists, each checked against the SHA-256
recorded there. This script makes that list: it reads MSYS2's current
package databases, takes the UCRT64 packages the build needs (ROOTS) with
every package they depend on, plus the MSYS make the build runs, and writes
one "<url> <sha256>" line for each. Run it on any machine with python3 and
a tar that reads .tar.zst, from the repository root, to move the build to
newer libraries:

    python3 build/windows/openconnect-lock.py

It reads package metadata only; nothing it downloads is run. Commit the
result, and update the Windows part of NOTICE if the bundled libraries
changed (bundle-openconnect.sh fails the build until it covers them).
"""
import os
import re
import subprocess
import sys
import tempfile
import urllib.request

REPO = "https://repo.msys2.org"
LOCK = os.path.join(os.path.dirname(os.path.abspath(__file__)), "openconnect-packages.txt")

# The compiler and pkg-config, and the libraries OpenConnect is configured to
# link (bundle-openconnect.sh): GnuTLS, libxml2, zlib and libiconv.
ROOTS = ["gcc", "pkgconf", "gnutls", "libxml2", "zlib", "libiconv"]
UCRT = "mingw-w64-ucrt-x86_64-"
# The runner's MSYS2 base install already has make's own dependencies
# (libintl, sh); pinning those too would replace the running shell's.
MSYS = ["make"]


def database(url):
    """Returns {name: desc fields} for the repository database at url."""
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "db.tar.zst")
        urllib.request.urlretrieve(url, path)
        subprocess.run(["tar", "-xf", path, "-C", d], check=True)
        pkgs, provides = {}, {}
        for entry in os.listdir(d):
            desc = os.path.join(d, entry, "desc")
            if not os.path.isfile(desc):
                continue
            fields, key = {}, None
            with open(desc, encoding="utf-8") as f:
                for line in f.read().splitlines():
                    if line.startswith("%") and line.endswith("%"):
                        key = line.strip("%")
                        fields[key] = []
                    elif line and key:
                        fields[key].append(line)
            name = fields["NAME"][0]
            pkgs[name] = fields
            for p in fields.get("PROVIDES", []):
                provides[bare(p)] = name
        return pkgs, provides


def bare(dep):
    """Strips a version constraint: "gmp>=6" is "gmp"."""
    return re.split(r"[<>=]", dep, maxsplit=1)[0]


def closure(pkgs, provides, roots):
    seen, todo = [], list(roots)
    while todo:
        dep = bare(todo.pop())
        name = dep if dep in pkgs else provides.get(dep)
        if name is None:
            sys.exit(f"no package provides {dep}")
        if name in seen:
            continue
        seen.append(name)
        todo.extend(pkgs[name].get("DEPENDS", []))
    return sorted(seen)


def line(base, fields):
    return f"{base}/{fields['FILENAME'][0]} {fields['SHA256SUM'][0]}"


def main():
    ucrt, ucrt_prov = database(f"{REPO}/mingw/ucrt64/ucrt64.db.tar.zst")
    msys, _ = database(f"{REPO}/msys/x86_64/msys.db.tar.zst")
    lines = [line(f"{REPO}/mingw/ucrt64", ucrt[n]) for n in closure(ucrt, ucrt_prov, [UCRT + r for r in ROOTS])]
    lines += [line(f"{REPO}/msys/x86_64", msys[n]) for n in MSYS]

    with open(LOCK, encoding="utf-8") as f:
        kept = [l for l in f.read().splitlines() if not l.startswith(REPO + "/")]
    with open(LOCK, "w", encoding="utf-8") as f:
        f.write("\n".join(kept + lines) + "\n")
    print(f"wrote {len(lines)} MSYS2 packages to {LOCK}")


if __name__ == "__main__":
    main()
