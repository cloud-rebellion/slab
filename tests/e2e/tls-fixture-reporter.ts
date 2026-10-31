import type { FullResult, Reporter } from '@playwright/test/reporter';
import { finishTLSFixture, retainTLSFixture } from './scripts/tls-fixture';

export default class TLSFixtureReporter implements Reporter {
  private globalError = false;
  printsToStdio(): boolean { return false; }
  onError(): void { this.globalError = true; }
  async onEnd(result: FullResult): Promise<void | { status: FullResult['status'] }> {
    // Normal 1.59.1 task completion awaits plugin teardown before this hook.
    // Interrupted/deadline/global-error paths cannot establish that same proof.
    if (this.globalError || result.status === 'interrupted' || result.status === 'timedout') {
      retainTLSFixture();
      return { status: result.status === 'passed' ? 'failed' : result.status };
    }
    try { finishTLSFixture(); } catch {
      // Reporter exceptions are swallowed by Playwright; explicitly preserve failure here.
      console.error('e2e_tls_cleanup_unconfirmed: small temporary fixture retained');
      return { status: result.status === 'passed' ? 'failed' : result.status };
    }
  }
}
