import { test, expect } from 'bun:test';
import { execFileSync } from 'node:child_process';
import { createPrivateKey, createPublicKey, X509Certificate } from 'node:crypto';
import { existsSync, lstatSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createTLSFixture, TLSLifecycle, currentTLSFixture } from './tls-fixture.ts';
import TLSFixtureReporter from '../tls-fixture-reporter.ts';

test('real fixture has loopback SAN, matching fresh private key and successful trust verification', () => {
  const fixture = createTLSFixture();
  try {
    const certificate = new X509Certificate(readFileSync(fixture.certFile));
    expect(certificate.checkIP('127.0.0.1')).toBe('127.0.0.1');
    expect(certificate.checkHost('localhost')).toBe('localhost');
    const actual = createPublicKey(createPrivateKey(readFileSync(fixture.keyFile))).export({ type: 'spki', format: 'der' });
    expect(actual.equals(certificate.publicKey.export({ type: 'spki', format: 'der' }))).toBe(true);
    expect(lstatSync(fixture.directory).mode & 0o777).toBe(0o700);
    expect(lstatSync(fixture.keyFile).mode & 0o777).toBe(0o600);
    execFileSync('openssl', ['verify', '-CAfile', fixture.certFile, '-verify_ip', '127.0.0.1', fixture.certFile],
      { stdio: 'ignore', timeout: 10000, killSignal: 'SIGKILL' });
  } finally { fixture.cleanup(); }
  expect(existsSync(fixture.directory)).toBe(false);
});

test('two invocations never reuse a fixture directory or private key', () => {
  const first = createTLSFixture();
  const second = createTLSFixture();
  try {
    expect(first.directory).not.toBe(second.directory);
    expect(readFileSync(first.keyFile).equals(readFileSync(second.keyFile))).toBe(false);
  } finally { first.cleanup(); second.cleanup(); }
});

test('failed generator cleans only its own plain partial output and restores umask', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  const oldMask = process.umask();
  try {
    expect(() => createTLSFixture({ temporaryRoot: parent, execute: (_command, args) => {
      writeFileSync(args[args.indexOf('-keyout') + 1], 'partial-fixture', { mode: 0o600 });
      throw new Error('raw child diagnostic must not escape');
    } })).toThrow('e2e_tls_generation_failed');
    expect(readdirSync(parent)).toEqual([]);
    expect(process.umask()).toBe(oldMask);
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('unknown content makes cleanup refuse without deleting any fixture or foreign file', () => {
  const fixture = createTLSFixture();
  const marker = join(fixture.directory, 'foreign');
  try {
    writeFileSync(marker, 'retain');
    expect(() => fixture.cleanup()).toThrow('e2e_tls_cleanup_unknown_entry');
    expect(readFileSync(marker, 'utf8')).toBe('retain');
    expect(existsSync(fixture.keyFile)).toBe(true);
    expect(existsSync(fixture.certFile)).toBe(true);
  } finally { rmSync(fixture.directory, { recursive: true, force: true }); }
});

test('owner config reloads share one fixture and normal completion permits a fresh next run', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  const environment = {};
  const exits = new Set();
  const lifecycle = new TLSLifecycle({ temporaryRoot: parent, environment,
    registerExit: (handler) => exits.add(handler), unregisterExit: (handler) => exits.delete(handler) });
  try {
    const first = lifecycle.get();
    expect(lifecycle.get().directory).toBe(first.directory);
    expect(readdirSync(parent)).toHaveLength(1);
    expect(exits.size).toBe(1);
    lifecycle.finish();
    expect(existsSync(first.directory)).toBe(false);
    expect(environment).toEqual({});
    expect(exits.size).toBe(0);
    const next = lifecycle.get();
    expect(next.directory).not.toBe(first.directory);
    lifecycle.finish();
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('a real child config reload borrows without generating or deleting the owner fixture', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  const environment = {};
  const lifecycle = new TLSLifecycle({ temporaryRoot: parent, environment,
    registerExit: () => {}, unregisterExit: () => {} });
  try {
    const fixture = lifecycle.get();
    const before = readdirSync(fixture.directory);
    const source = new URL('./tls-fixture.ts', import.meta.url).href;
    const code = `import { TLSLifecycle } from ${JSON.stringify(source)};
      const lifecycle = new TLSLifecycle({ temporaryRoot: ${JSON.stringify(parent)} });
      const paths = lifecycle.get(); lifecycle.finish();
      console.log(JSON.stringify({borrowedSame: paths.directory === process.env.SLAB_E2E_TLS_DIR}));`;
    const output = execFileSync(process.execPath, ['--eval', code], {
      env: { ...process.env, ...environment }, timeout: 5000, killSignal: 'SIGKILL', maxBuffer: 4096
    });
    expect(JSON.parse(output.toString()).borrowedSame).toBe(true);
    expect(readdirSync(parent)).toHaveLength(1);
    expect(readdirSync(fixture.directory)).toEqual(before);
    lifecycle.finish();
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('partial inherited markers and external paths fail before creating or borrowing a fixture', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  try {
    for (const environment of [
      { SLAB_E2E_TLS_DIR: parent },
      { SLAB_E2E_TLS_OWNER_PID: '1' },
      { SLAB_E2E_TLS_DIR: '/foreign/slab-e2e-tls-abcdef', SLAB_E2E_TLS_OWNER_PID: '1' },
      { SLAB_E2E_TLS_DIR: join(parent, 'slab-e2e-tls-abcdef'), SLAB_E2E_TLS_OWNER_PID: String(process.pid) }
    ]) {
      const lifecycle = new TLSLifecycle({ temporaryRoot: parent, environment });
      expect(() => lifecycle.get()).toThrow('e2e_tls_inherited_fixture_invalid');
    }
    expect(readdirSync(parent)).toEqual([]);
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('unknown natural exit retains only the bounded owner fixture and never retries cleanup', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  let onExit;
  let uncertain;
  const lifecycle = new TLSLifecycle({ temporaryRoot: parent, environment: {},
    registerExit: (handler) => { onExit = handler; }, unregisterExit: () => {},
    unconfirmedExit: (code) => { uncertain = code; } });
  try {
    const fixture = lifecycle.get();
    onExit(0);
    expect(uncertain).toBe(0);
    expect(existsSync(fixture.keyFile)).toBe(true);
    expect(() => lifecycle.finish()).toThrow('e2e_tls_previous_cleanup_unconfirmed');
    expect(() => lifecycle.get()).toThrow('e2e_tls_previous_cleanup_unconfirmed');
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('an unknown cleanup entry is retained and future owner operations fail closed', () => {
  const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
  const lifecycle = new TLSLifecycle({ temporaryRoot: parent, environment: {},
    registerExit: () => {}, unregisterExit: () => {} });
  try {
    const fixture = lifecycle.get();
    writeFileSync(join(fixture.directory, 'foreign'), 'retain');
    expect(() => lifecycle.finish()).toThrow('e2e_tls_cleanup_unconfirmed');
    expect(readFileSync(join(fixture.directory, 'foreign'), 'utf8')).toBe('retain');
    expect(existsSync(fixture.keyFile)).toBe(true);
    expect(() => lifecycle.finish()).toThrow('e2e_tls_previous_cleanup_unconfirmed');
  } finally { rmSync(parent, { recursive: true, force: true }); }
});

test('supported reporter completion cleans the real owner fixture without rewriting test status', async () => {
  const fixture = currentTLSFixture();
  const reporter = new TLSFixtureReporter();
  expect(await reporter.onEnd({ status: 'failed', startTime: new Date(), duration: 0 })).toBeUndefined();
  expect(existsSync(fixture.directory)).toBe(false);
  expect(process.env.SLAB_E2E_TLS_DIR).toBeUndefined();
  expect(await reporter.onEnd({ status: 'failed', startTime: new Date(), duration: 0 })).toBeUndefined();
});


test('uncertain reporter paths retain files while the fallback preserves actual Bun caller status', () => {
  const source = new URL('./tls-fixture.ts', import.meta.url).href;
  const reporterSource = new URL('../tls-fixture-reporter.ts', import.meta.url).href;
  for (const fixtureCase of [
    { status: 'interrupted', globalError: false, callerExit: 0 },
    { status: 'timedout', globalError: false, callerExit: 0 },
    { status: 'passed', globalError: true, callerExit: 7 }
  ]) {
    const parent = mkdtempSync(join(tmpdir(), 'slab-e2e-tls-test-'));
    try {
      const code = `import { currentTLSFixture } from ${JSON.stringify(source)};
        import Reporter from ${JSON.stringify(reporterSource)};
        import { existsSync } from 'node:fs';
        const fixture = currentTLSFixture(); const reporter = new Reporter();
        if (${fixtureCase.globalError}) reporter.onError({ message: 'synthetic global error' });
        const result = await reporter.onEnd({ status: ${JSON.stringify(fixtureCase.status)}, startTime: new Date(), duration: 0 });
        console.log(JSON.stringify({ retained: existsSync(fixture.keyFile) && existsSync(fixture.certFile), result }));
        process.exit(${fixtureCase.callerExit});`;
      const environment = { ...process.env, TMPDIR: parent };
      delete environment.SLAB_E2E_TLS_DIR; delete environment.SLAB_E2E_TLS_OWNER_PID;
      let output;
      let actualExit = 0;
      try { output = execFileSync(process.execPath, ['--eval', code], { env: environment, timeout: 5000, killSignal: 'SIGKILL', maxBuffer: 8192 }); }
      catch (error) { actualExit = error.status; output = error.stdout; }
      expect(actualExit).toBe(fixtureCase.callerExit);
      const result = JSON.parse(output.toString());
      expect(result.retained).toBe(true);
      expect(result.result.status).toBe(fixtureCase.status === 'passed' ? 'failed' : fixtureCase.status);
      expect(readdirSync(parent)).toHaveLength(1);
    } finally { rmSync(parent, { recursive: true, force: true }); }
  }
});
