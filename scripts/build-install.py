#!/usr/bin/env python3
"""Render release pins in the installer; runs in release CI, never on clients."""
import hashlib
from pathlib import Path
import re
import sys


def render(template: str, version: str, directory: Path) -> str:
    if not re.fullmatch(r"v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})", version):
        raise ValueError("invalid release version")
    replacements = {"@RELEASE_TAG@": version}
    for arch in ("amd64", "arm64", "arm", "386"):
        binary = directory / f"vpsagent-linux-{arch}"
        if not binary.is_file() or binary.stat().st_size == 0:
            raise ValueError(f"missing release binary: {binary.name}")
        replacements[f"@SHA_{arch.upper()}@"] = hashlib.sha256(binary.read_bytes()).hexdigest()
    for placeholder, value in replacements.items():
        if template.count(placeholder) != 1:
            raise ValueError(f"expected one {placeholder} placeholder")
        template = template.replace(placeholder, value)
    return template


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: build-install.py VERSION DIST_DIRECTORY")
    directory = Path(sys.argv[2])
    template = (Path(__file__).resolve().parent / "install.sh").read_text()
    (directory / "install.sh").write_text(render(template, sys.argv[1], directory))
