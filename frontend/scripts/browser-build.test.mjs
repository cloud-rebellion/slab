import { afterEach, expect, test } from 'bun:test';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { stageSourceMaps } from './stage-source-maps.mjs';
import { browserDsnBinding, validatedBrowserRelease, writeBuildManifest } from './write-build-manifest.mjs';

const roots = [];
const release = 'a'.repeat(40);
const dsn = 'https://publicIngestKey@flare.cloudrebellion.tech/123456789012';

afterEach(async () => {
  for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true });
});

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'atomicsite-browser-build-'));
  roots.push(root);
  const publicDir = join(root, 'build');
  const mapsDir = join(root, 'source-maps');
  const asset = '_app/immutable/chunks/app.12345678.js';
  const script = `const dsn = "${dsn}"; const release = "${release}"; throw new Error("test")`;
  const map = '{"version":3,"sources":["src/app.ts"],"names":[],"mappings":"AAAA"}';
  await mkdir(join(publicDir, '_app/immutable/chunks'), { recursive: true });
  await writeFile(join(publicDir, asset), script);
  await writeFile(join(publicDir, `${asset}.map`), map);
  return { root, publicDir, mapsDir, asset, script, map };
}

test('public assets and private maps have one deterministic release and DSN manifest', async () => {
  const { root, publicDir, mapsDir, asset, script, map } = await fixture();
  expect(await stageSourceMaps(publicDir, mapsDir)).toEqual([`${asset}.map`]);
  expect(await readFile(join(mapsDir, `${asset}.map`), 'utf8')).toBe(map);
  await expect(readFile(join(publicDir, `${asset}.map`))).rejects.toThrow();

  const output = join(root, 'build-manifest.json');
  const manifest = await writeBuildManifest(publicDir, mapsDir, output, dsn, release);
  expect(manifest.program_id).toBe('atomicsite');
  expect(manifest.release).toBe(release);
  expect(manifest.public_files[asset]).toBe(createHash('sha256').update(script).digest('hex'));
  expect(manifest.source_maps[`${asset}.map`]).toBe(createHash('sha256').update(map).digest('hex'));
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding(dsn));
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding('https://publicIngestKey@other.example/123456789012'));
  expect(manifest.browser_dsn_binding_sha256).not.toBe(browserDsnBinding('https://otherKey@flare.cloudrebellion.tech/123456789012'));
  expect(JSON.stringify(manifest)).not.toContain('publicIngestKey');
  expect(await readFile(output, 'utf8')).toBe(`${JSON.stringify(manifest)}\n`);
  expect(await writeBuildManifest(publicDir, mapsDir, output, dsn, release)).toEqual(manifest);
  expect(await readFile(join(publicDir, asset), 'utf8')).toBe(script);
});

test('configured browser ingest requires an exact source revision', async () => {
  expect(validatedBrowserRelease('', 'development')).toBe('development');
  expect(() => validatedBrowserRelease(dsn, 'development')).toThrow('full source commit');
  expect(() => validatedBrowserRelease(dsn, 'abc1234')).toThrow('full source commit');
  expect(validatedBrowserRelease(dsn, release)).toBe(release);
  expect(() => browserDsnBinding('https://key:password@flare.cloudrebellion.tech/123456789012')).toThrow('invalid browser DSN');

  const { root, publicDir, mapsDir } = await fixture();
  await stageSourceMaps(publicDir, mapsDir);
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json'), dsn, 'development'))
    .rejects.toThrow('full source commit');
});

test('manifest rejects a bundle that omits configured browser capture', async () => {
  const { root, publicDir, mapsDir, asset } = await fixture();
  await stageSourceMaps(publicDir, mapsDir);
  await writeFile(join(publicDir, asset), `const release = "${release}"`);
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json'), dsn, release))
    .rejects.toThrow('public scripts do not contain');
  await writeFile(join(publicDir, asset), `const dsn = "${dsn}"`);
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json'), dsn, release))
    .rejects.toThrow('public scripts do not contain');
});

test('manifest refuses public maps or maps without a matching public script', async () => {
  const { root, publicDir, mapsDir, asset } = await fixture();
  await mkdir(join(mapsDir, '_app/immutable/chunks'), { recursive: true });
  await writeFile(join(mapsDir, `${asset}.map`), '{}');
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json'), dsn, release))
    .rejects.toThrow('public build still contains a source map');
  await rm(join(publicDir, `${asset}.map`));
  await rm(join(publicDir, asset));
  await writeFile(join(publicDir, 'different.js'), 'a');
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json'), dsn, release))
    .rejects.toThrow('each JavaScript source map must match a public script');
});
