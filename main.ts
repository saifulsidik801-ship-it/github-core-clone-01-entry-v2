// Launcher: spawns ./forge, registers its port with the make.fun harness,
// waits for /done, then kills the child and exits.
import { spawn } from "node:child_process";

const envUrl = process.env.MAKEFUN_ENV_URL;
const forge = spawn("./forge", [], {
  stdio: ["ignore", "pipe", "inherit"],
  env: { ...process.env, PORT: process.env.PORT ?? "" },
});

let port: number | null = null;
let registered = false;

const portPromise = new Promise<number>((resolve, reject) => {
  let buf = "";
  forge.stdout.on("data", (chunk: Buffer) => {
    buf += chunk.toString();
    const m = buf.match(/PORT\s+(\d+)/);
    if (m) resolve(Number(m[1]));
  });
  forge.on("exit", (code) => {
    if (port === null) reject(new Error(`forge exited early with code ${code}`));
  });
});

async function register(p: number): Promise<void> {
  if (!envUrl) return;
  for (let attempt = 0; attempt < 30; attempt++) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ port: p }),
      });
      if (res.ok) return;
    } catch {
      // retry
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error("failed to register port with harness");
}

async function waitDone(): Promise<void> {
  if (!envUrl) {
    // Standalone mode: run until killed.
    await new Promise(() => {});
    return;
  }
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/done`);
      if (res.ok) {
        const body = (await res.json()) as { done?: boolean };
        if (body.done) return;
      }
    } catch {
      // retry
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
}

try {
  port = await portPromise;
  await register(port);
  registered = true;
  await waitDone();
} finally {
  if (!forge.killed) forge.kill("SIGTERM");
}
process.exit(0);
