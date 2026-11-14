// Entry lifecycle: the harness env owns the trial, this process only exposes the app to it.
// The flow is: bind an ephemeral port, tell the env where to reach the app, then wait until the
// env reports the trial is over. Nothing here is scored — the suite grades the server that
// app.ts builds. This file only wires the app to the env and exits when told.
//
// How it works, step by step:
//   1. MAKEFUN_ENV_URL points at the env (in a trial container, http://store:8080). Without it
//      there is no env to talk to, so the process exits 1 right away.
//   2. The app listens on port 0 so the kernel picks a free ephemeral port. Port 8080 is never
//      bound — that port belongs to the env.
//   3. POST $MAKEFUN_ENV_URL/register with {"port": N}. The env remembers the caller's address
//      plus N and dials the app there from then on. The first register wins; a 409 means some
//      other entry already registered, and this process simply keeps waiting.
//   4. Poll GET $MAKEFUN_ENV_URL/done until it answers {"done": true}. The env answers
//      immediately ({"done": false} while the suite is still running), so this is a poll loop,
//      not a held request. A failed or slow poll is retried with a short exponential backoff,
//      and the process never exits before the env says done.
//   5. Close the server and exit 0. The harness then stops the env, which writes the export
//      the scorer reads.
//
// This module imports its own server and nothing else: no third-party package, no Node built-in,
// no model call.

import { createApp } from './app.ts';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** Longest a single register or done request may run before it is abandoned and retried. */
const FETCH_TIMEOUT_MS = 30_000;
/** First backoff between retries, and the ceiling it grows to. */
const RETRY_BASE_MS = 250;
const RETRY_CAP_MS = 2_000;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Backoff after `attempt` failed requests: exponential from RETRY_BASE_MS, capped at RETRY_CAP_MS. */
function backoffMs(attempt: number): number {
  return Math.min(RETRY_CAP_MS, RETRY_BASE_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer and 409 means a first
 * register already won, which is not this process's problem to fix. 400 and 405 are contract
 * errors and end the program; a network error or any other status is retried, since the env
 * may still be starting up.
 */
async function registerPort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) {
        return;
      }
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) {
        throw err;
      }
    }
    await sleep(backoffMs(attempt));
  }
}

/** Poll GET /done until it answers {"done": true}; never return before that. */
async function waitUntilDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, {
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) {
          return;
        }
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(backoffMs(attempt));
  }
}

/** Bind the port, register it, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });

  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[entry] listening on port ${address.port}\n`);

  await registerPort(address.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitUntilDone();
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
