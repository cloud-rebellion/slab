/** One invocation's private, loopback-only TLS fixture; never a deployment credential. */
import {
  chmodSync, lstatSync, mkdtempSync, readdirSync, rmdirSync, unlinkSync,
  realpathSync, type BigIntStats
} from 'node:fs';
import { execFileSync, type ExecFileSyncOptions } from 'node:child_process';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
const filenames = ['stub-cert.pem', 'stub-key.pem'];
const stable = (s: BigIntStats) => [s.dev, s.ino, s.mode, s.uid, s.gid, s.nlink, s.size, s.mtimeNs, s.ctimeNs].join(':');
const snapshot = (path: string) => lstatSync(path, { bigint: true });

function currentUID(): bigint {
  if (typeof process.getuid !== 'function') throw new Error('e2e_tls_requires_posix_host');
  return BigInt(process.getuid());
}

function privatePlain(path: string) {
  const s = snapshot(path);
  if (!s.isFile() || s.nlink !== 1n || s.uid !== currentUID() ||
      (s.mode & 0o777n) !== 0o600n || s.size < 1n || s.size > 16384n) {
    throw new Error('e2e_tls_fixture_file_identity_invalid');
  }
  return s;
}

type Execute = (command: string, args: readonly string[], options: ExecFileSyncOptions) => unknown;
export type TLSPaths = { directory: string; certFile: string; keyFile: string };
export type TLSFixture = TLSPaths & { cleanup: () => void };
export function createTLSFixture({ temporaryRoot = tmpdir(), execute = execFileSync as Execute }:
  { temporaryRoot?: string; execute?: Execute } = {}): TLSFixture {
  if (typeof process.getuid !== 'function') throw new Error('e2e_tls_requires_posix_host');
  const directory = mkdtempSync(join(realpathSync(temporaryRoot), 'slab-e2e-tls-'));
  chmodSync(directory, 0o700);
  const originalDirectory = snapshot(directory);
  const certFile = join(directory, filenames[0]);
  const keyFile = join(directory, filenames[1]);
  let expectedFiles: Record<string, string> | null = null;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    const current = snapshot(directory);
    if (!current.isDirectory() || current.dev !== originalDirectory.dev ||
        current.ino !== originalDirectory.ino || current.uid !== originalDirectory.uid ||
        (current.mode & 0o777n) !== 0o700n) throw new Error('e2e_tls_cleanup_directory_changed');
    const entries = readdirSync(directory);
    if (entries.some((name) => !filenames.includes(name))) throw new Error('e2e_tls_cleanup_unknown_entry');
    // Check the complete set before unlinking anything. A replaced/foreign leaf is retained.
    const leaves = entries.map((name) => {
      const path = join(directory, name);
      const info = privatePlain(path);
      if (expectedFiles && stable(info) !== expectedFiles[name]) throw new Error('e2e_tls_cleanup_leaf_changed');
      return [path, stable(info)] as const;
    });
    for (const [path, identity] of leaves) {
      if (stable(privatePlain(path)) !== identity) throw new Error('e2e_tls_cleanup_leaf_changed');
      unlinkSync(path);
    }
    const finalDirectory = snapshot(directory);
    if (finalDirectory.dev !== originalDirectory.dev || finalDirectory.ino !== originalDirectory.ino ||
        !finalDirectory.isDirectory()) throw new Error('e2e_tls_cleanup_directory_changed');
    rmdirSync(directory);
    cleaned = true;
  };
  try {
    const oldMask = process.umask(0o077);
    try {
      // No shell, no prompt, bounded child, no key/certificate output. No parent trust store changes.
      execute('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-sha256',
        '-days', '1', '-batch', '-subj', '/CN=localhost',
        '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost',
        '-addext', 'basicConstraints=critical,CA:TRUE',
        '-keyout', keyFile, '-out', certFile],
      { stdio: 'ignore', timeout: 30000, killSignal: 'SIGKILL', maxBuffer: 65536 });
    } finally { process.umask(oldMask); }
    const certIdentity = privatePlain(certFile);
    const keyIdentity = privatePlain(keyFile);
    execute('openssl', ['verify', '-CAfile', certFile, '-verify_ip', '127.0.0.1', certFile],
      { stdio: 'ignore', timeout: 10000, killSignal: 'SIGKILL', maxBuffer: 65536 });
    if (stable(privatePlain(certFile)) !== stable(certIdentity) ||
        stable(privatePlain(keyFile)) !== stable(keyIdentity)) throw new Error('e2e_tls_fixture_changed');
    expectedFiles = { [filenames[0]]: stable(certIdentity), [filenames[1]]: stable(keyIdentity) };
    return { directory, certFile, keyFile, cleanup };
  } catch {
    try { cleanup(); } catch { throw new Error('e2e_tls_generation_failed_cleanup_unconfirmed'); }
    throw new Error('e2e_tls_generation_failed');
  }
}

type ExitHandler = (code: number) => void;
type LifecycleOptions = {
  environment?: NodeJS.ProcessEnv;
  pid?: number;
  temporaryRoot?: string;
  createFixture?: () => TLSFixture;
  registerExit?: (handler: ExitHandler) => void;
  unregisterExit?: (handler: ExitHandler) => void;
  ownerAlive?: (pid: number) => void;
  unconfirmedExit?: (code: number) => void;
};
type Owned = { fixture: TLSFixture; phase: 'active' | 'retained' | 'finished' };

/** Finite TLS ownership only. Playwright retains all process/browser/signal ownership. */
export class TLSLifecycle {
  private environment: NodeJS.ProcessEnv;
  private pid: number;
  private temporaryRoot: string;
  private createFixture: () => TLSFixture;
  private registerExit: (handler: ExitHandler) => void;
  private unregisterExit: (handler: ExitHandler) => void;
  private ownerAlive: (pid: number) => void;
  private unconfirmedExit: (code: number) => void;
  private owned: Owned | undefined;
  private exitHandler: ExitHandler;

  constructor(options: LifecycleOptions = {}) {
    this.environment = options.environment ?? process.env;
    this.pid = options.pid ?? process.pid;
    this.temporaryRoot = realpathSync(options.temporaryRoot ?? tmpdir());
    this.createFixture = options.createFixture ?? (() => createTLSFixture({ temporaryRoot: this.temporaryRoot }));
    this.registerExit = options.registerExit ?? ((handler) => { process.once('exit', handler); });
    this.unregisterExit = options.unregisterExit ?? ((handler) => { process.off('exit', handler); });
    this.ownerAlive = options.ownerAlive ?? ((pid) => { process.kill(pid, 0); });
    this.unconfirmedExit = options.unconfirmedExit ?? ((_code) => {
      // Observe only. Forcing an exit here would skip Playwright's later native exit listeners.
      console.error('e2e_tls_lifecycle_unconfirmed: small temporary fixture retained; caller status preserved');
    });
    this.exitHandler = (code) => {
      if (this.owned?.phase === 'finished' || !this.owned) return;
      // A failed load, reporter override, signal or abrupt framework exit is not teardown proof.
      // Retain bounded files. Never start cleanup or signal a process from this fallback.
      this.owned.phase = 'retained';
      this.unconfirmedExit(code);
    };
  }

  get(): TLSPaths {
    if (this.owned?.phase === 'retained') throw new Error('e2e_tls_previous_cleanup_unconfirmed');
    if (this.owned?.phase === 'active') {
      if (this.environment.SLAB_E2E_TLS_DIR !== this.owned.fixture.directory ||
          this.environment.SLAB_E2E_TLS_OWNER_PID !== String(this.pid)) throw new Error('e2e_tls_owner_environment_changed');
      return this.owned.fixture;
    }
    const directory = this.environment.SLAB_E2E_TLS_DIR;
    const owner = this.environment.SLAB_E2E_TLS_OWNER_PID;
    if (directory !== undefined || owner !== undefined) {
      if (!directory || !owner || !/^[1-9][0-9]{0,9}$/.test(owner) || Number(owner) === this.pid ||
          directory !== resolve(directory) || dirname(directory) !== this.temporaryRoot ||
          !/^slab-e2e-tls-[A-Za-z0-9]{6}$/.test(basename(directory))) {
        throw new Error('e2e_tls_inherited_fixture_invalid');
      }
      try {
        this.ownerAlive(Number(owner));
        const info = snapshot(directory);
        if (!info.isDirectory() || info.uid !== currentUID() ||
            (info.mode & 0o777n) !== 0o700n) throw new Error('invalid');
        const certFile = join(directory, filenames[0]);
        const keyFile = join(directory, filenames[1]);
        privatePlain(certFile); privatePlain(keyFile);
        // Worker/config reloads borrow the exact inherited fixture. No exit hook or cleanup ownership.
        return { directory, certFile, keyFile };
      } catch { throw new Error('e2e_tls_inherited_fixture_invalid'); }
    }
    const fixture = this.createFixture();
    this.owned = { fixture, phase: 'active' };
    this.environment.SLAB_E2E_TLS_DIR = fixture.directory;
    this.environment.SLAB_E2E_TLS_OWNER_PID = String(this.pid);
    this.registerExit(this.exitHandler);
    return fixture;
  }

  retain(): void {
    if (this.owned?.phase === 'active') this.owned.phase = 'retained';
  }

  finish(): void {
    if (!this.owned || this.owned.phase === 'finished') return;
    if (this.owned.phase === 'retained') throw new Error('e2e_tls_previous_cleanup_unconfirmed');
    this.owned.phase = 'retained';
    if (this.environment.SLAB_E2E_TLS_DIR !== this.owned.fixture.directory ||
        this.environment.SLAB_E2E_TLS_OWNER_PID !== String(this.pid)) throw new Error('e2e_tls_owner_environment_changed');
    try { this.owned.fixture.cleanup(); } catch { throw new Error('e2e_tls_cleanup_unconfirmed'); }
    this.owned.phase = 'finished';
    delete this.environment.SLAB_E2E_TLS_DIR;
    delete this.environment.SLAB_E2E_TLS_OWNER_PID;
    this.unregisterExit(this.exitHandler);
  }
}

const slot = Symbol.for('slab.e2e.owned-tls-lifecycle');
const globals = globalThis as unknown as Record<symbol, TLSLifecycle | undefined>;
function lifecycle(): TLSLifecycle {
  return globals[slot] ??= new TLSLifecycle();
}
export function currentTLSFixture(): TLSPaths { return lifecycle().get(); }
export function finishTLSFixture(): void { lifecycle().finish(); }

export function retainTLSFixture(): void { lifecycle().retain(); }
