// Keep generated source maps available to Stage while removing them from the
// static directory embedded in the Atomic Site Go binary.
import { mkdir, readdir, rename, rm } from 'node:fs/promises';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

export async function stageSourceMaps(publicRoot, privateRoot) {
  const publicDir = resolve(publicRoot);
  const privateDir = resolve(privateRoot);
  if (privateDir === publicDir || privateDir.startsWith(`${publicDir}${sep}`) ||
      publicDir.startsWith(`${privateDir}${sep}`)) {
    throw new Error('private source-map directory must be separate from the public build');
  }
  await rm(privateDir, { recursive: true, force: true });
  const moved = [];
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (entry.isFile() && entry.name.endsWith('.map')) {
        const name = relative(publicDir, file);
        const target = join(privateDir, name);
        await mkdir(dirname(target), { recursive: true });
        await rename(file, target);
        moved.push(name);
      }
    }
  }
  await walk(publicDir);
  return moved.sort();
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    const moved = await stageSourceMaps('build', 'source-maps');
    if (moved.length === 0) throw new Error('build produced no source maps');
    console.log(`staged ${moved.length} private source maps`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : 'source-map staging failed');
    process.exitCode = 1;
  }
}
