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
        self.write('site/tutorials/start.md', '# Start\n')

    def write(self, path, text):
        file = self.source / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(text)
        return file

    def prepare(self):
        with contextlib.redirect_stdout(io.StringIO()):
            return bundler.prepare(self.source, self.output)

    def test_each_nested_page_is_a_distinct_skill_with_its_own_body(self):
        self.write('site/guides/nested/testing.md', '# Testing\nOnly this page.\n')
        self.write('site/reference/testing.md', '# Reference testing\nDifferent page.\n')
        names = self.prepare()
        self.assertEqual(len(names), 3)
        first = (self.output / 'guides-nested-testing/SKILL.md').read_text()
        self.assertIn('Only this page.', first)
        self.assertNotIn('Different page.', first)
        self.assertFalse((self.output / 'project-documentation').exists())

    def test_links_select_other_skills_and_only_linked_assets_are_bundled(self):
        self.write('site/guides/a.md', '# A\n[B](b.md#details)\n![shot](shots/a.png)\n[spec][s]\n[s]: spec.yaml\n')
        self.write('site/guides/b.md', '# B\n## Details\n')
        self.write('site/guides/shots/a.png', 'linked image')
        self.write('site/guides/shots/unused.png', 'unrelated image')
        self.write('site/guides/spec.yaml', 'type: object')
        self.prepare()
        directory = self.output / 'guides-a'
        entry = (directory / 'SKILL.md').read_text()
        self.assertIn('skill://guides-b/SKILL.md#details', entry)
        self.assertIn('assets/site/guides/shots/a.png', entry)
        self.assertIn('[s]: assets/site/guides/spec.yaml', entry)
        self.assertEqual({p.relative_to(directory).as_posix() for p in directory.rglob('*') if p.is_file()},
                         {'SKILL.md', 'assets/site/guides/shots/a.png', 'assets/site/guides/spec.yaml'})

    def test_code_examples_and_external_links_remain_literal(self):
        original = '# A\n~~~markdown\n[B](b.md)\n~~~\n`[B](b.md)`\n[web](https://example.com/x)\n'
        self.write('site/guides/a.md', original)
        self.write('site/guides/b.md', '# B\n')
        self.prepare()
        self.assertIn(original, (self.output / 'guides-a/SKILL.md').read_text())

    def test_name_collisions_and_long_paths_have_stable_valid_names(self):
        paths = ['site/guides/a_b.md', 'site/guides/a-b.md', 'site/guides/' + 'x' * 80 + '.md']
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
        self.write('site/guides/a.md', '# A\n[code](../backend/main.go)\n')
        self.write('site/backend/main.go', 'package main')
        self.prepare()
        entry = (self.output / 'guides-a/SKILL.md').read_text()
        self.assertIn('https://github.com/r33drichards/computer-use/blob/main/site/backend/main.go', entry)
        self.assertFalse((self.output / 'guides-a/assets').exists())

    def test_internal_docs_readmes_and_blog_are_excluded(self):
        for path in ('docs/private.md', 'README.md', 'site/README.md', 'site/blog/news.md'):
            self.write(path, '# Internal or non-documentation content\nSECRET MARKER\n')
        names = self.prepare()
        self.assertEqual(set(names.values()), {'tutorials-start'})
        self.assertNotIn('SECRET MARKER', (self.output / 'tutorials-start/SKILL.md').read_text())

    def test_site_clean_url_links_select_page_skills(self):
        self.write('site/guides/a.md', '# A\n[B](/reference/b#details)\n[C](../reference/b)\n')
        self.write('site/reference/b.md', '# B\n')
        self.prepare()
        text = (self.output / 'guides-a/SKILL.md').read_text()
        self.assertIn('skill://reference-b/SKILL.md#details', text)
        self.assertIn('[C](skill://reference-b/SKILL.md)', text)

    def test_symlink_attachments_are_rejected(self):
        self.write('site/guides/a.md', '# A\n[data](alias.yaml)\n')
        file = self.write('site/guides/data.yaml', 'secret: no')
        (self.source / 'site/guides/alias.yaml').symlink_to(file)
        with self.assertRaisesRegex(ValueError, 'symlink attachment'):
            self.prepare()


if __name__ == '__main__':
    unittest.main()
