#!/usr/bin/env python3
"""Index bundled documentation and enforce the SEP-2640 manifest limits."""
from pathlib import Path
import sys

def prepare(root):
    for entry in sorted(root.glob("*/SKILL.md")):
        directory = entry.parent
        pages = sorted((directory / "references").rglob("*.md"))
        index = "# Project documentation\n\n" + "\n".join(
            f"- [{page.relative_to(directory)}]({page.relative_to(directory)})" for page in pages
        ) + "\n"
        (directory / "INDEX.md").write_text(index)
        files = [p for p in directory.rglob("*") if p.is_file()]
        if len(files) > 512 or sum(p.stat().st_size for p in files) > 16 * 1024 * 1024:
            raise ValueError(f"{directory.name} exceeds SEP-2640 manifest limits")
        print(f"{directory.name}: {len(files)} files, {len(pages)} Markdown pages")

if __name__ == "__main__":
    prepare(Path(sys.argv[1]))
