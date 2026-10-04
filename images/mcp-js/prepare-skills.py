#!/usr/bin/env python3
"""Turn each repository documentation page into an independent MCP skill."""
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import shutil
import sys
from urllib.parse import quote, unquote, urlsplit

PUBLIC_SECTIONS = ('tutorials', 'guides', 'reference', 'explanation')

LINK = re.compile(r'(?P<prefix>!?\[[^\]\n]*\]\(\s*)(?P<target><[^>\n]+>|[^\s)]+)')
REFERENCE = re.compile(r'(?m)^(?P<prefix> {0,3}\[(?!\^)[^\]\n]+\]:\s*)(?P<target><[^>\n]+>|\S+)')
FENCE = re.compile(r'^ {0,3}(`{3,}|~{3,})')


def page_name(path):
    parts = path.with_suffix('').parts
    if parts[:1] == ('site',):
        parts = parts[1:]
    slug = re.sub(r'[^a-z0-9]+', '-', '-'.join(parts).lower()).strip('-')
    return slug


def page_text(page):
    text = page.read_text(encoding='utf-8')
    # Site frontmatter is page metadata, not skill frontmatter.
    if text.startswith('---\n'):
        end = re.search(r'(?m)^---\s*$', text[4:])
        if end:
            text = text[4 + end.end():].lstrip('\n')
    return text


def rewrite_links(text, resolve):
    """Leave fenced code examples untouched."""
    output = []
    fence = None
    for line in text.splitlines(keepends=True):
        marker = FENCE.match(line)
        if marker:
            run = marker[1]
            if fence is None:
                fence = run
            elif run[0] == fence[0] and len(run) >= len(fence) and not line[marker.end():].strip():
                fence = None
            output.append(line)
        elif fence is not None:
            output.append(line)
        else:
            spans = [(m.start(), m.end()) for m in re.finditer(r'(`+).*?\1', line)]
            def replace(match):
                if any(start <= match.start('target') < end for start, end in spans):
                    return match[0]
                target = match['target']
                bracketed = target.startswith('<')
                rewritten = resolve(target[1:-1] if bracketed else target)
                return match['prefix'] + ('<' + rewritten + '>' if bracketed else rewritten)
            output.append(REFERENCE.sub(replace, LINK.sub(replace, line)))
    return ''.join(output)


def prepare(source, output):
    source = source.resolve()
    output = output.resolve()
    if output.exists() and any(output.iterdir()):
        raise ValueError('output directory must be empty')
    pages = sorted(page for section in PUBLIC_SECTIONS
                   for page in (source / 'site' / section).rglob('*.md'))
    if not pages:
        raise ValueError('no customer-facing documentation pages found')
    paths = [page.relative_to(source) for page in pages]
    slugs = {path: page_name(path) for path in paths}
    counts = Counter(slugs.values())
    names = {}
    for path, slug in slugs.items():
        if len(slug) > 64 or counts[slug] > 1:
            suffix = hashlib.sha256(path.as_posix().encode()).hexdigest()[:10]
            slug = slug[:53].rstrip('-') + '-' + suffix
        names[path] = slug
    if len(set(names.values())) != len(names):
        raise ValueError('skill names collided')
    output.mkdir(parents=True, exist_ok=True)
    total = 0
    for page, path in zip(pages, paths):
        if page.is_symlink():
            raise ValueError(f'symlink documentation page: {path}')
        name = names[path]
        directory = output / name
        directory.mkdir()
        text = page_text(page)
        heading = re.search(r'(?m)^#\s+(.+)$', text)
        title = heading[1].strip() if heading else path.stem.replace('-', ' ')
        description = f'Use for {title} ({path.as_posix()}).'

        def resolve(target):
            url = urlsplit(target)
            if url.scheme or url.netloc or not url.path:
                return target
            linked = (source / 'site' / unquote(url.path).lstrip('/')) if url.path.startswith('/') else (page.parent / unquote(url.path))
            destination = linked.resolve()
            if not destination.suffix:
                destination = destination.with_suffix('.md')
            if not destination.is_relative_to(source):
                return target
            relative = destination.relative_to(source)
            suffix = ('?' + url.query if url.query else '') + ('#' + url.fragment if url.fragment else '')
            if relative in names:
                return f'skill://{names[relative]}/SKILL.md' + suffix
            bundled = len(relative.parts) > 2 and relative.parts[0] == 'site' and relative.parts[1] in PUBLIC_SECTIONS
            if bundled and destination.is_file():
                if linked.is_symlink():
                    raise ValueError(f'symlink attachment: {relative}')
                asset = Path('assets') / relative
                copied = directory / asset
                copied.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(destination, copied)
                return quote(asset.as_posix()) + suffix
            if url.path.startswith('/'):
                return 'https://computeruse.site' + target
            # Files outside public documentation are never bundled.
            return 'https://github.com/r33drichards/computer-use/blob/main/' + quote(relative.as_posix()) + suffix

        body = rewrite_links(text, resolve)
        entry = ('---\nname: ' + name + '\ndescription: ' + json.dumps(description[:1024]) +
                 '\n---\n\n' + f'Source: `{path.as_posix()}`. Load linked `skill://` pages separately when relevant.\n\n' + body)
        (directory / 'SKILL.md').write_text(entry, encoding='utf-8')
        files = [p for p in directory.rglob('*') if p.is_file()]
        size = sum(p.stat().st_size for p in files)
        if len(files) > 512 or size > 16 * 1024 * 1024:
            raise ValueError(f'{name} exceeds SEP-2640 manifest limits')
        total += size
        print(f'{name}: {len(files)} files; source {path}')
    if total > 64 * 1024 * 1024:
        raise ValueError('skills catalog exceeds the server limit of 64 MiB')
    print(f'{len(pages)} documentation pages → {len(names)} independent skills')
    return names


if __name__ == '__main__':
    prepare(Path(sys.argv[1]), Path(sys.argv[2]))
