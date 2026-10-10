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
      // zooming with a wheel: a notch every 70 ms for 1.5 s, then the same out; the gaps between animation frames throughout
      await settle();
      const zf = [];
      {
        let prev = await new Promise((res) => requestAnimationFrame(res)), stop = false, k = 0;
        const timer = setInterval(() => { if (k >= 44) { stop = true; return; } const dy = k < 22 ? -100 : 100; k++; cv.dispatchEvent(new WheelEvent('wheel', { deltaY: dy, clientX: r.x + S.W / 2, clientY: r.y + S.H / 2, bubbles: true, cancelable: true })); }, 70);
        while (!stop) { const now = await new Promise((res) => requestAnimationFrame(res)); zf.push(now - prev); prev = now; }
        clearInterval(timer);
      }
      const med = (a) => [...a].sort((p, q) => p - q)[a.length >> 1];
      const p95 = (a) => [...a].sort((p, q) => p - q)[Math.floor(a.length * 0.95)];
      return { stats: JSON.stringify(m.stats), zoom: ${zoom}, idleMs: Math.round(med(idle)), drawMs: Math.round(med(draws)), labelsMs: +med(labels).toFixed(1), dragFrameMs: Math.round(med(frames)), dragFps: +(1000 / med(frames)).toFixed(1), zoomMed: Math.round(med(zf)), zoomP95: Math.round(p95(zf)), zoomMax: Math.round(Math.max(...zf)), n: S.n };
    })()`));
  }
  console.log('zoom  idle redraw(ms)  draw call(ms)  labels(ms)  drag frame(ms)  drag fps   wheel-zoom frame ms: median  p95  max');
  for (const r of rows) console.log(String(r.zoom).padEnd(6) + String(r.idleMs).padEnd(16) + String(r.drawMs).padEnd(15) + String(r.labelsMs).padEnd(12) + String(r.dragFrameMs).padEnd(16) + String(r.dragFps).padEnd(11) + `${r.zoomMed}  ${r.zoomP95}  ${r.zoomMax}`.padEnd(22) + r.stats);
  return page.logs.length ? { logs: page.logs } : undefined;
};
