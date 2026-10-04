// Bind the served frontend files and private maps produced by one Docker
// frontend stage. The final runtime image retains only these hashes.
import { createHash } from 'node:crypto';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import { join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

async function hashes(root) {
  const entries = {};
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (entry.isFile()) {
        const name = relative(root, file).split(sep).join('/');
        entries[name] = createHash('sha256').update(await readFile(file)).digest('hex');
      } else {
        throw new Error('build contains an unsupported filesystem entry');
      }
    }
  }
  await walk(root);
  return Object.fromEntries(Object.entries(entries).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
}

// The browser DSN is injected into the compiled frontend by this same build.
// Bind its project and public ingest key to the image without retaining the
// DSN itself in the runtime manifest. Stage compares this digest with an
// authenticated project read before uploading private source maps.
export function browserDsnBinding(dsn) {
  if (!dsn?.trim()) return '';
  let url;
  try {
    url = new URL(dsn.trim());
  } catch {
    throw new Error('invalid browser DSN build input');
  }
  if (!['http:', 'https:'].includes(url.protocol) || !url.host || url.password ||
      url.search || url.hash || !/^[A-Za-z0-9_-]{1,64}$/.test(url.username) ||
      !/^\/[0-9]{1,20}\/?$/.test(url.pathname)) {
    throw new Error('invalid browser DSN build input');
  }
  const projectID = url.pathname.replace(/\/$/, '').slice(1);
  return createHash('sha256').update(`flare-browser-dsn-v1\n${url.username}\n${projectID}`).digest('hex');
}

// A configured browser must identify the exact checked-out source revision.
// Stage supplies this value through FLARE_RELEASE before Compose builds the
// image. Local builds with no DSN may keep the development default.
export function validatedBrowserRelease(browserDSN, release) {
  const value = release?.trim() ?? '';
  if (browserDSN?.trim() && !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(value)) {
    throw new Error('configured browser DSN requires a full source commit in PUBLIC_FLARE_RELEASE');
  }
  return value;
}

export async function writeBuildManifest(
  publicRoot, privateRoot, output,
  browserDSN = process.env.PUBLIC_FLARE_DSN ?? '',
  release = process.env.PUBLIC_FLARE_RELEASE ?? ''
) {
  const dsnBinding = browserDsnBinding(browserDSN);
  const exactRelease = validatedBrowserRelease(browserDSN, release);
  const publicDir = resolve(publicRoot);
  const privateDir = resolve(privateRoot);
  const outputFile = resolve(output);
  if (publicDir === privateDir || outputFile === publicDir || outputFile === privateDir ||
      outputFile.startsWith(`${publicDir}${sep}`) ||
      outputFile.startsWith(`${privateDir}${sep}`)) {
    throw new Error('manifest and source maps must stay outside the public build');
  }
  const publicFiles = await hashes(publicDir);
  const sourceMaps = await hashes(privateDir);
  if (!Object.keys(publicFiles).length || !Object.keys(sourceMaps).length) {
    throw new Error('public files and private source maps are required');
  }
  if (Object.keys(publicFiles).some((name) => name.endsWith('.map'))) {
    throw new Error('public build still contains a source map');
  }
  for (const name of Object.keys(sourceMaps)) {
    if (!name.endsWith('.js.map') || !Object.hasOwn(publicFiles, name.slice(0, -4))) {
      throw new Error('each JavaScript source map must match a public script');
    }
  }
  const manifest = {
    version: 2,
    program_id: 'atomicsite',
    release: exactRelease,
    public_files: publicFiles,
    source_maps: sourceMaps,
    browser_dsn_binding_sha256: dsnBinding
  };
  await writeFile(outputFile, `${JSON.stringify(manifest)}\n`, { flag: 'w', mode: 0o600 });
  return manifest;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.length === 3 && process.argv[2] === '--validate-config') {
      browserDsnBinding(process.env.PUBLIC_FLARE_DSN ?? '');
      validatedBrowserRelease(process.env.PUBLIC_FLARE_DSN ?? '', process.env.PUBLIC_FLARE_RELEASE ?? '');
    } else if (process.argv.length === 2) {
      const manifest = await writeBuildManifest('build', 'source-maps', 'build-manifest.json');
      console.log(`bound ${Object.keys(manifest.public_files).length} public files to ${Object.keys(manifest.source_maps).length} private maps`);
    } else {
      throw new Error('usage: bun write-build-manifest.mjs [--validate-config]');
    }
  } catch (error) {
    console.error(error instanceof Error ? error.message : 'build manifest failed');
    process.exitCode = 1;
  }
}
