import contextlib
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('prepare_skills', Path(__file__).with_name('prepare-skills.py'))
bundler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bundler)


class PageSkillsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        self.output = self.root / 'skills'
        self.write('README.md', '# Project\n')

    def write(self, path, text):
        file = self.source / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(text)
        return file

    def prepare(self):
        with contextlib.redirect_stdout(io.StringIO()):
            return bundler.prepare(self.source, self.output)

    def test_each_nested_page_is_a_distinct_skill_with_its_own_body(self):
        self.write('docs/contracts/testing.md', '# Testing\nOnly this page.\n')
        self.write('site/reference/testing.md', '# Reference testing\nDifferent page.\n')
        names = self.prepare()
        self.assertEqual(len(names), 3)
        first = (self.output / 'docs-contracts-testing/SKILL.md').read_text()
        self.assertIn('Only this page.', first)
        self.assertNotIn('Different page.', first)
        self.assertFalse((self.output / 'project-documentation').exists())

    def test_links_select_other_skills_and_only_linked_assets_are_bundled(self):
        self.write('docs/a.md', '# A\n[B](b.md#details)\n![shot](shots/a.png)\n[spec][s]\n[s]: spec.yaml\n')
        self.write('docs/b.md', '# B\n## Details\n')
        self.write('docs/shots/a.png', 'linked image')
        self.write('docs/shots/unused.png', 'unrelated image')
        self.write('docs/spec.yaml', 'type: object')
        self.prepare()
        directory = self.output / 'docs-a'
        entry = (directory / 'SKILL.md').read_text()
        self.assertIn('skill://docs-b/SKILL.md#details', entry)
        self.assertIn('assets/docs/shots/a.png', entry)
        self.assertIn('[s]: assets/docs/spec.yaml', entry)
        self.assertEqual({p.relative_to(directory).as_posix() for p in directory.rglob('*') if p.is_file()},
                         {'SKILL.md', 'assets/docs/shots/a.png', 'assets/docs/spec.yaml'})

    def test_code_examples_and_external_links_remain_literal(self):
        original = '# A\n~~~markdown\n[B](b.md)\n~~~\n`[B](b.md)`\n[web](https://example.com/x)\n'
        self.write('docs/a.md', original)
        self.write('docs/b.md', '# B\n')
        self.prepare()
        self.assertIn(original, (self.output / 'docs-a/SKILL.md').read_text())

    def test_name_collisions_and_long_paths_have_stable_valid_names(self):
        paths = ['docs/a_b.md', 'docs/a-b.md', 'docs/' + 'x' * 80 + '.md']
        for path in paths:
            self.write(path, '# Title\n')
        names = self.prepare()
        self.assertEqual(len(set(names.values())), 4)
        for name in names.values():
            self.assertLessEqual(len(name), 64)
            self.assertRegex(name, r'^[a-z0-9]+(?:-[a-z0-9]+)*$')
        self.output = self.root / 'again'
        self.assertEqual(names, self.prepare())

    def test_source_frontmatter_is_replaced_by_skill_metadata(self):
        self.write('site/reference/test.md', '---\nlayout: page\n---\n# Reference\nActual text.\n')
        self.prepare()
        entry = (self.output / 'reference-test/SKILL.md').read_text()
        self.assertIn('name: reference-test\n', entry)
        self.assertNotIn('layout:', entry)
        self.assertIn('# Reference\nActual text.', entry)

    def test_repository_code_is_linked_instead_of_duplicated(self):
        self.write('docs/a.md', '# A\n[code](../backend/main.go)\n')
        self.write('backend/main.go', 'package main')
        self.prepare()
        entry = (self.output / 'docs-a/SKILL.md').read_text()
        self.assertIn('https://github.com/r33drichards/computer-use/blob/main/backend/main.go', entry)
        self.assertFalse((self.output / 'docs-a/assets').exists())

    def test_symlink_attachments_are_rejected(self):
        self.write('docs/a.md', '# A\n[data](alias.yaml)\n')
        file = self.write('docs/data.yaml', 'secret: no')
        (self.source / 'docs/alias.yaml').symlink_to(file)
        with self.assertRaisesRegex(ValueError, 'symlink attachment'):
            self.prepare()


if __name__ == '__main__':
    unittest.main()
