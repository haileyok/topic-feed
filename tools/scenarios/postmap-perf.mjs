// How fast the post map draws and pans: node tools/cdp.mjs tools/scenarios/postmap-perf.mjs
// Headless Chromium draws with a software renderer, so the numbers are slow in absolute terms; they are for comparing
// one version of the page with another. For each zoom: milliseconds for a draw (points and labels, GL finished), for the
// labels alone, and the frame rate while a drag is simulated with pointer events.
export const options = { width: 1500, height: 900 };

export default async (page) => {
  const base = process.env.BASE ?? 'http://127.0.0.1:8740';
  const t0 = Date.now();
  await page.goto(base + '/');
  await page.waitFor('window.__map && window.__map.S.n > 0 && document.querySelector("#loading").hidden');
  console.log(`ready ${Date.now() - t0} ms after navigating (files, building the points, first draw)`);
  await page.wait(1000);
  const rows = [];
  for (const zoom of (process.env.ZOOMS ?? '1,3,10,40').split(',').map(Number)) {
    rows.push(await page.eval(`(async () => {
      const m = window.__map, S = m.S, gl = m.gl;
      const cx0 = (S.bounds[0] + S.bounds[2]) / 2, cy0 = (S.bounds[1] + S.bounds[3]) / 2;
      const settle = () => new Promise((r) => setTimeout(r, 400));
      S.view = { cx: cx0, cy: cy0, scale: S.fit * ${zoom} };
      m.draw(); gl.finish(); await settle();
      // one draw at a time, the view moved a little between them, GL finished
      const draws = [], labels = [];
      for (let k = 0; k < 12; k++) {
        S.view = { cx: cx0 + k * 2 / S.view.scale, cy: cy0, scale: S.fit * ${zoom} };
        let t = performance.now(); m.draw(); gl.finish(); draws.push(performance.now() - t);
        t = performance.now(); m.drawLabels(); labels.push(performance.now() - t);
      }
      // a full-quality redraw when nothing is moving: how long until the next frame after asking for one
      const idle = [];
      for (let k = 0; k < 6; k++) {
        await new Promise((r) => setTimeout(r, 300));
        S.view = { cx: cx0 + k * 3 / S.view.scale, cy: cy0, scale: S.fit * ${zoom} };
        const t = performance.now(); m.draw(); await new Promise((res) => requestAnimationFrame(() => requestAnimationFrame(res))); idle.push(performance.now() - t);
      }
      await settle();
      // a drag: pointer events, one per animation frame
      const cv = document.querySelector('#labels'), r = cv.getBoundingClientRect();
      const ev = (type, x, y) => cv.dispatchEvent(new PointerEvent(type, { pointerId: 1, clientX: r.x + x, clientY: r.y + y, buttons: type === 'pointerup' ? 0 : 1, bubbles: true }));
      const frames = [];
      let x = S.W / 2, y = S.H / 2;
      ev('pointerdown', x, y);
      let last = await new Promise((res) => requestAnimationFrame(res));
      const t0 = last;
      for (let k = 0; k < 40; k++) {
        x += 7; y += 3; ev('pointermove', x, y);
        const now = await new Promise((res) => requestAnimationFrame(res));
        frames.push(now - last); last = now;
      }
      ev('pointerup', x, y);
      const med = (a) => [...a].sort((p, q) => p - q)[a.length >> 1];
      return { stats: JSON.stringify(m.stats), zoom: ${zoom}, idleMs: Math.round(med(idle)), drawMs: Math.round(med(draws)), labelsMs: +med(labels).toFixed(1), dragFrameMs: Math.round(med(frames)), dragFps: +(1000 / med(frames)).toFixed(1), n: S.n };
    })()`));
  }
  console.log('zoom  idle redraw(ms)  draw call(ms)  labels(ms)  drag frame(ms)  drag fps');
  for (const r of rows) console.log(String(r.zoom).padEnd(6) + String(r.idleMs).padEnd(16) + String(r.drawMs).padEnd(15) + String(r.labelsMs).padEnd(12) + String(r.dragFrameMs).padEnd(16) + r.dragFps + '   ' + r.stats);
  return page.logs.length ? { logs: page.logs } : undefined;
};
