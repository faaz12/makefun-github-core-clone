// Entry shim: exec the statically linked Go forge binary vendored in this
// repo (built from ./go with CGO_ENABLED=0, linux/amd64, -trimpath -s -w).
// The harness runs `node /bot/main.ts`; the Go binary does the real work:
// a raw-TCP HTTP/1.1 forge server (REST v3 + GraphQL + MCP + git smart
// HTTP), zero-alloc hot paths, single-inflate pack ingest. It owns the whole
// entry lifecycle itself: bind an ephemeral port, POST $MAKEFUN_ENV_URL/register,
// poll GET $MAKEFUN_ENV_URL/done, exit 0 when the env says done.
// This shim only spawns it, forwards signals, and propagates its exit code.
// The original TypeScript implementation is kept in *.ts alongside (app.ts,
// git.ts, githttp.ts, graphql.ts, mcp.ts, rest.ts, store.ts) as the port
// source; the Go code in ./go is a behavior-identical port of it.
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const binary = join(dirname(fileURLToPath(import.meta.url)), "forge");
const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
for (const sig of ["SIGINT", "SIGTERM"] as const) {
  process.on(sig, () => child.kill(sig));
}
child.on("exit", (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
});
child.on("error", (err) => {
  process.stderr.write(`[entry] failed to start forge binary: ${err.message}\n`);
  process.exit(1);
});
