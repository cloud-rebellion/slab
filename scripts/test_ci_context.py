#!/usr/bin/env python3
"""Synthetic CI context cases; no application servers or external providers."""
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('ci_context', Path(__file__).with_name('ci-context.py'))
context = importlib.util.module_from_spec(spec)
spec.loader.exec_module(context)


class ContextTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.workspace = Path(self.tmp.name)

    def source(self, private=False, module=None):
        root = self.workspace / 'atomicsite' if private else self.workspace
        root.mkdir(exist_ok=True)
        (root / 'ee').mkdir()
        module = module or (context.PRIVATE_MODULE if private else sorted(context.PUBLIC_MODULES)[0])
        (root / 'go.mod').write_text('module ' + module + '\n\ngo 1.26.3\n')
        (root / 'ee/cloud_oss.go').write_text('//go:build !ee\n\npackage ee\n')
        if private:
            (root / 'ee/cloud_ee.go').write_text('//go:build ee\n\npackage ee\n')
            for number in range(6):
                (root / ('fixture_ee_' + str(number) + '.go')).write_text('//go:build ee\n\npackage fixture\n')
        return root

    def test_both_public_module_aliases_require_all_oss_checks(self):
        root = self.source()
        for module in context.PUBLIC_MODULES:
            (root / 'go.mod').write_text('module ' + module + '\n')
            scope = context.source_scope(self.workspace)
            self.assertEqual(scope['root'], '.')
            self.assertEqual(scope['go_matrix'], '{"build_tags":[""]}')
            self.assertEqual(scope['ee_applicable'], 'false')
            self.assertIn('not applicable', scope['ee_reason'])

    def test_complete_private_source_keeps_both_lanes(self):
        self.source(private=True)
        scope = context.source_scope(self.workspace)
        self.assertEqual(scope['root'], 'atomicsite')
        self.assertEqual(scope['go_matrix'], '{"build_tags":["","ee"]}')
        self.assertEqual(scope['ee_applicable'], 'true')
        self.assertEqual(scope['go_mod'], 'atomicsite/go.mod')

    def test_missing_or_negated_private_ee_sentinel_is_refused(self):
        root = self.source(private=True)
        sentinel = root / 'ee/cloud_ee.go'
        sentinel.unlink()
        with self.assertRaisesRegex(ValueError, 'EE sentinel'):
            context.source_scope(self.workspace)
        for constraint in ('!ee', 'pro'):
            sentinel.write_text('//go:build ' + constraint + '\n\npackage ee\n')
            with self.assertRaisesRegex(ValueError, 'EE sentinel'):
                context.source_scope(self.workspace)

    def test_new_valid_private_ee_source_keeps_both_lanes_without_workflow_edit(self):
        root = self.source(private=True)
        (root / 'new_feature_ee.go').write_text('//go:build ee\n\npackage fixture\n')
        scope = context.source_scope(self.workspace)
        self.assertEqual(scope['go_matrix'], '{"build_tags":["","ee"]}')
        self.assertEqual(scope['ee_applicable'], 'true')

    def test_missing_or_positive_oss_stub_is_refused(self):
        root = self.source(private=True)
        stub = root / 'ee/cloud_oss.go'
        stub.unlink()
        with self.assertRaisesRegex(ValueError, 'OSS enterprise-boundary stub'):
            context.source_scope(self.workspace)
        stub.write_text('//go:build ee\n\npackage ee\n')
        with self.assertRaisesRegex(ValueError, 'OSS boundary stub must negate'):
            context.source_scope(self.workspace)

    def test_public_enterprise_leak_is_refused(self):
        root = self.source()
        (root / 'unexpected.go').write_text('//go:build linux && (ee || pro)\n\npackage fixture\n')
        with self.assertRaisesRegex(ValueError, 'enterprise build constraint'):
            context.source_scope(self.workspace)

    def test_unknown_module_and_ambiguous_roots_are_refused(self):
        root = self.source(module='example.com/unrelated')
        with self.assertRaisesRegex(ValueError, 'module identity'):
            context.source_scope(self.workspace)
        (root / 'atomicsite').mkdir()
        (root / 'atomicsite/go.mod').write_text('module ' + context.PRIVATE_MODULE + '\n')
        with self.assertRaisesRegex(ValueError, 'exactly one'):
            context.source_scope(self.workspace)

    def test_module_and_source_symlinks_are_refused(self):
        root = self.source()
        original = (root / 'go.mod').read_text()
        (root / 'go.mod').unlink()
        (root / 'other.mod').write_text(original)
        (root / 'go.mod').symlink_to('other.mod')
        with self.assertRaisesRegex(ValueError, 'regular file'):
            context.source_scope(self.workspace)

    def test_public_legacy_positive_tag_and_ee_filename_are_refused(self):
        root = self.source()
        path = root / 'unexpected.go'
        path.write_text('// +build ee,linux\n\npackage fixture\n')
        with self.assertRaisesRegex(ValueError, 'enterprise build constraint'):
            context.source_scope(self.workspace)
        path.unlink()
        (root / 'partial_ee.go').write_text('package fixture\n')
        with self.assertRaisesRegex(ValueError, 'enterprise-shaped'):
            context.source_scope(self.workspace)

    def test_symlinked_product_directory_is_refused(self):
        target = self.workspace / 'outside'
        target.mkdir()
        (self.workspace / 'atomicsite').symlink_to(target, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symlinked private'):
            context.source_scope(self.workspace)

    def test_constraint_polarity_and_syntax(self):
        for expr in ('ee', 'linux && (ee || pro)', '!(!ee)', 'ee || !pro'):
            with self.subTest(expr=expr):
                self.assertTrue(context.positive_enterprise(expr))
        for expr in ('!ee', '!ee && linux', '!(ee || pro)', 'linux || windows'):
            with self.subTest(expr=expr):
                self.assertFalse(context.positive_enterprise(expr))
        for expr in ('ee &&', '(ee', 'ee | pro', ''):
            with self.subTest(expr=expr):
                with self.assertRaises(ValueError):
                    context.positive_enterprise(expr)

    def test_readable_module_components_stay_distinct_under_normal_literal_redaction(self):
        # The normal exporter rewrites full module strings in source. Its exact
        # replacement cannot alter the readable component tuples used here.
        source = Path(__file__).with_name('ci-context.py').read_text()
        for old in (context.PRIVATE_MODULE, '/'.join(('github.com', 'bright-interaction', 'slab'))):
            self.assertNotIn(old, source)
        rewritten = source.replace(context.PRIVATE_MODULE, '/'.join(('github.com', 'cloud-rebellion', 'slab')))
        self.assertEqual(source, rewritten)
        self.assertNotIn(context.PRIVATE_MODULE, context.PUBLIC_MODULES)


if __name__ == '__main__':
    unittest.main()
