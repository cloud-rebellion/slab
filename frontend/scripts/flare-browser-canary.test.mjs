import { afterEach, expect, test } from 'bun:test';
import { flareBrowserAssetName, installFlareBrowserCapture } from '../src/lib/flare-client.ts';

const originalGlobals = new Map();

function setGlobal(name, value) {
  if (!originalGlobals.has(name)) originalGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
  Object.defineProperty(globalThis, name, { configurable: true, value });
}

afterEach(() => {
  for (const [name, descriptor] of originalGlobals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else delete globalThis[name];
  }
  originalGlobals.clear();
});

test('browser canary reports a fixed handled error once per page, even after reinstall', async () => {
  const page = new EventTarget();
  const beacons = [];
  setGlobal('window', page);
  setGlobal('location', { origin: 'https://slab.example', pathname: '/login', href: 'https://slab.example/login' });
  setGlobal('navigator', { sendBeacon: (_endpoint, body) => { beacons.push(body); return true; } });

  const options = { dsn: 'https://publicKey@flare.example/123', release: 'a'.repeat(40) };
  const stop = installFlareBrowserCapture(options);
  const event = new Event('flare:browser-canary');
  Object.defineProperty(event, 'detail', { value: { message: 'untrusted private detail' } });
  page.dispatchEvent(event);
  page.dispatchEvent(new Event('flare:browser-canary'));
  stop();
  const stopAgain = installFlareBrowserCapture(options);
  page.dispatchEvent(new Event('flare:browser-canary'));
  stopAgain();

  for (let i = 0; i < 20 && beacons.length === 0; i++) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  expect(beacons).toHaveLength(1);
  const payload = JSON.parse(await beacons[0].text());
  expect(payload.message).toBe('Atomicsite Flare browser source-map canary v1');
  expect(payload.exception.values[0].type).toBe('FlareBrowserCanary');
  expect(payload.request.url).toBe('https://slab.example/login');
  expect(payload.exception.values[0].stacktrace.frames.length).toBeGreaterThan(0);
  expect(JSON.stringify(payload)).not.toContain('untrusted private detail');
});

test('asset naming accepts a same-origin compiled script only with loaded-script evidence', async () => {
  const origin = 'https://slab.example';
  const script = `${origin}/_app/immutable/chunks/flare-client.ABCDEFGH.js`;
  setGlobal('document', { scripts: [{ src: script }] });
  expect(await flareBrowserAssetName(script, origin)).toMatch(/^flare-asset-sha256:[0-9a-f]{64}$/);
  expect(await flareBrowserAssetName(`${origin}/_app/immutable/chunks/forged.ABCDEFGH.js`, origin)).toBeUndefined();
  expect(await flareBrowserAssetName('https://other.example/_app/immutable/chunks/flare-client.ABCDEFGH.js', origin)).toBeUndefined();
});
