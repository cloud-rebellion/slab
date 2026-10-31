#!/usr/bin/env python3
"""Prove the Slab CI source layout before choosing paths or build lanes."""
import argparse
import json
import os
from pathlib import Path
import re
import stat
import sys

# Readable components preserve distinct identities through the mirror's normal
# module-path redaction. These already-public module names are not secret data.
PRIVATE_MODULE = '/'.join(('github.com', 'brightinteraction', 'atomicsite'))
PUBLIC_MODULES = frozenset('/'.join(parts) for parts in (
    ('github.com', 'bright-interaction', 'slab'),
    ('github.com', 'cloud-rebellion', 'slab'),
))


def regular(path):
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError:
        return False
    if not stat.S_ISREG(mode):
        raise ValueError('CI layout critical path must be a regular file: ' + path.name)
    return True


def positive_enterprise(expression, tag_names=('ee', 'pro')):
    """Parse Go build expressions; !ee stubs are permitted, positive EE is not."""
    tokens = re.findall(r'[\w.]+|\|\||&&|!|\(|\)', expression, flags=re.UNICODE)
    if ''.join(tokens) != re.sub(r'\s+', '', expression):
        raise ValueError('invalid Go build constraint')
    i = 0
    def primary():
        nonlocal i
        if i == len(tokens):
            raise ValueError('incomplete Go build constraint')
        token = tokens[i]
        i += 1
        if token == '!':
            return ('!', primary())
        if token == '(':
            node = disjunction()
            if i == len(tokens) or tokens[i] != ')':
                raise ValueError('unclosed Go build constraint')
            i += 1
            return node
        if token in ('&&', '||', ')'):
            raise ValueError('invalid Go build constraint token')
        return ('tag', token)
    def conjunction():
        nonlocal i
        node = primary()
        while i < len(tokens) and tokens[i] == '&&':
            i += 1
            node = ('&&', node, primary())
        return node
    def disjunction():
        nonlocal i
        node = conjunction()
        while i < len(tokens) and tokens[i] == '||':
            i += 1
            node = ('||', node, conjunction())
        return node
    def contains(node, positive=True):
        if node[0] == 'tag':
            return positive and node[1] in tag_names
        if node[0] == '!':
            return contains(node[1], not positive)
        return contains(node[1], positive) or contains(node[2], positive)
    tree = disjunction()
    if i != len(tokens):
        raise ValueError('trailing Go build constraint token')
    return contains(tree)


def source_scope(workspace):
    workspace = Path(workspace).absolute()
    if workspace.is_symlink() or not workspace.is_dir():
        raise ValueError('CI workspace must be a real directory')
    nested = workspace / 'atomicsite'
    if nested.is_symlink():
        raise ValueError('ambiguous or symlinked private product root')
    root_mod = regular(workspace / 'go.mod')
    nested_mod = regular(nested / 'go.mod')
    if root_mod == nested_mod:
        raise ValueError('exactly one private or standalone module root is required')
    root = nested if nested_mod else workspace
    raw = (root / 'go.mod').read_text(encoding='utf-8')
    modules = re.findall(r'^module\s+(\S+)\s*$', raw, flags=re.MULTILINE)
    if len(modules) != 1:
        raise ValueError('one unambiguous module declaration is required')
    module = modules[0]
    private = nested_mod and module == PRIVATE_MODULE
    public = root_mod and module in PUBLIC_MODULES
    if not (private or public):
        raise ValueError('module identity does not match its CI layout')
    stub = root / 'ee' / 'cloud_oss.go'
    if stub.parent.is_symlink() or not regular(stub):
        raise ValueError('the OSS enterprise-boundary stub must be present')
    stub_constraints = re.findall(r'^//go:build (.+)$', stub.read_text(encoding='utf-8'), flags=re.MULTILINE)
    if (len(stub_constraints) != 1 or positive_enterprise(stub_constraints[0])
            or not positive_enterprise('!(' + stub_constraints[0] + ')', ('ee',))):
        raise ValueError('the OSS boundary stub must negate the EE tag')
    enterprise = []
    sentinel_positive_ee = False
    for directory, dirs, files in os.walk(root, followlinks=False):
        dirs[:] = [name for name in dirs if name != '.git']
        for name in dirs:
            if (Path(directory) / name).is_symlink():
                raise ValueError('symlinked source directory cannot prove CI scope')
        for name in files:
            if not name.endswith('.go'):
                continue
            path = Path(directory) / name
            if not regular(path):
                raise ValueError('symlinked Go source cannot prove CI scope')
            # Build constraints belong before the package clause.
            header = []
            with path.open(encoding='utf-8') as handle:
                for line in handle:
                    if line.startswith('package '):
                        break
                    header.append(line)
                    if sum(map(len, header)) > 65536:
                        raise ValueError('oversized Go source header')
            constraints = [line[len('//go:build '):].strip()
                           for line in header if line.startswith('//go:build ')]
            if len(constraints) > 1:
                raise ValueError('multiple Go build constraints')
            if constraints and positive_enterprise(constraints[0]):
                enterprise.append(path.relative_to(root).as_posix())
            if path.relative_to(root).as_posix() == 'ee/cloud_ee.go':
                sentinel_positive_ee = bool(constraints and positive_enterprise(constraints[0], ('ee',)))
            if not constraints:
                legacy = [line[len('// +build '):].strip()
                          for line in header if line.startswith('// +build ')]
                expressions = [' || '.join(' && '.join(term.split(','))
                               for term in line.split()) for line in legacy]
                if any(positive_enterprise(expr) for expr in expressions):
                    enterprise.append(path.relative_to(root).as_posix())
            if public and name.endswith('_ee.go'):
                raise ValueError('enterprise-shaped Go source is forbidden in public OSS')
    if private:
        if not sentinel_positive_ee:
            raise ValueError('private EE sentinel is missing or does not positively require EE')
    elif enterprise:
        raise ValueError('public OSS source contains an enterprise build constraint')
    rel = 'atomicsite' if private else '.'
    prefix = 'atomicsite/' if private else ''
    return {
        'context': 'private' if private else 'public',
        'root': rel,
        'go_mod': prefix + 'go.mod',
        'go_sum': prefix + 'go.sum',
        'frontend': prefix + 'frontend',
        'e2e': prefix + 'tests/e2e',
        'ee_applicable': 'true' if private else 'false',
        'go_matrix': json.dumps({'build_tags': ['', 'ee'] if private else ['']}, separators=(',', ':')),
        'ee_reason': 'Verified private boundary: OSS and EE checks are required' if private else
                     'Verified standalone OSS: EE is not applicable; all OSS checks are required',
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workspace', default='.')
    parser.add_argument('--github-output')
    args = parser.parse_args()
    scope = source_scope(args.workspace)
    if args.github_output:
        with open(args.github_output, 'a', encoding='utf-8') as handle:
            for name, value in scope.items():
                handle.write(name + '=' + value + '\n')
    print(json.dumps(scope, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, UnicodeError) as error:
        print('Slab CI context refused: ' + str(error), file=sys.stderr)
        sys.exit(1)
