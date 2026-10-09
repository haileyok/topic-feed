// Minimal Chrome DevTools driver for checking the web UIs headlessly.
//
//   node tools/cdp.mjs scenario.mjs
//
// A scenario module default-exports async (page) => {...} where page has:
//   goto(url), eval(expr), wait(ms), click(x, y), move(x, y), wheel(x, y, dy), key(k),
//   screenshot(path), logs (console messages and exceptions so far)
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, readdirSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

function findChromium() {
  if (process.env.CHROMIUM) return process.env.CHROMIUM;
  for (const d of readdirSync('/nix/store')) {
    const bin = `/nix/store/${d}/bin/chromium`;
    if (/-chromium-\d/.test(d) && existsSync(bin)) return bin;
  }
  throw new Error('set CHROMIUM');
}

export async function withPage(fn, { width = 1600, height = 900, dpr = 1 } = {}) {
  const profile = mkdtempSync(join(tmpdir(), 'cdp-'));
  const port = 9300 + Math.floor(Math.random() * 500);
  const proc = spawn(findChromium(), [
    '--headless=new', '--no-sandbox', '--disable-gpu-sandbox', `--user-data-dir=${profile}`,
    `--remote-debugging-port=${port}`, '--use-gl=angle', '--use-angle=swiftshader',
    '--enable-unsafe-swiftshader', '--ignore-gpu-blocklist', `--window-size=${width},${height}`,
    '--hide-scrollbars', 'about:blank',
  ], { stdio: 'ignore' });
  try {
    let targets;
    for (let i = 0; i < 80; i++) {
      try { targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json(); if (targets.some((t) => t.type === 'page')) break; } catch {}
      await new Promise((r) => setTimeout(r, 150));
    }
    const wsUrl = targets.find((t) => t.type === 'page').webSocketDebuggerUrl;
    const ws = new WebSocket(wsUrl);
    await new Promise((r) => (ws.onopen = r));
    let id = 0;
    const pending = new Map();
    const logs = [];
    ws.onmessage = (m) => {
      const msg = JSON.parse(m.data);
      if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); return; }
      if (msg.method === 'Runtime.consoleAPICalled') logs.push(`console.${msg.params.type}: ${msg.params.args.map((a) => a.value ?? a.description ?? '').join(' ')}`);
      if (msg.method === 'Runtime.exceptionThrown') logs.push('EXCEPTION: ' + (msg.params.exceptionDetails.exception?.description ?? msg.params.exceptionDetails.text));
      if (msg.method === 'Log.entryAdded' && ['error', 'warning'].includes(msg.params.entry.level)) logs.push(`log.${msg.params.entry.level}: ${msg.params.entry.text} ${msg.params.entry.url ?? ''}`);
    };
    const send = (method, params = {}) => new Promise((res, rej) => {
      const i = ++id;
      pending.set(i, (m) => (m.error ? rej(new Error(`${method}: ${m.error.message}`)) : res(m.result)));
      ws.send(JSON.stringify({ id: i, method, params }));
    });
    await send('Runtime.enable'); await send('Page.enable'); await send('Log.enable');
    await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: dpr, mobile: false });
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    const page = {
      logs,
      wait: sleep,
      async goto(url) { await send('Page.navigate', { url }); await sleep(500); },
      async eval(expr) {
        const r = await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
        if (r.exceptionDetails) throw new Error('eval: ' + (r.exceptionDetails.exception?.description ?? r.exceptionDetails.text));
        return r.result.value;
      },
      async waitFor(expr, ms = 20000) {
        const t0 = Date.now();
        while (Date.now() - t0 < ms) { try { if (await page.eval(expr)) return true; } catch {} await sleep(200); }
        throw new Error('timeout waiting for ' + expr);
      },
      async move(x, y) { await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y }); },
      async click(x, y) {
        await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
      },
      async drag(x0, y0, x1, y1) {
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: x0, y: y0, button: 'left', clickCount: 1 });
        for (let k = 1; k <= 8; k++) await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: x0 + ((x1 - x0) * k) / 8, y: y0 + ((y1 - y0) * k) / 8, button: 'left', buttons: 1 });
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: x1, y: y1, button: 'left' });
      },
      async wheel(x, y, dy) { await send('Input.dispatchMouseEvent', { type: 'mouseWheel', x, y, deltaX: 0, deltaY: dy }); },
      async key(key) { await send('Input.dispatchKeyEvent', { type: 'keyDown', key }); await send('Input.dispatchKeyEvent', { type: 'keyUp', key }); },
      async screenshot(path, { format = 'png', quality } = {}) {
        const r = await send('Page.captureScreenshot', { format, ...(quality ? { quality } : {}) });
        writeFileSync(path, Buffer.from(r.data, 'base64'));
      },
    };
    return await fn(page);
  } finally {
    proc.kill('SIGKILL');
    try { rmSync(profile, { recursive: true, force: true }); } catch {}
  }
}

// Run a scenario when invoked directly (not when another script imports withPage).
if (process.argv[2] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const mod = await import(new URL(process.argv[2], `file://${process.cwd()}/`));
  const out = await withPage(mod.default, mod.options ?? {});
  if (out !== undefined) console.log(typeof out === 'string' ? out : JSON.stringify(out, null, 2));
  process.exit(process.exitCode ?? 0);
}
