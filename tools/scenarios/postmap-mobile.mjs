// The post map on a phone-sized window: node tools/cdp.mjs tools/scenarios/postmap-mobile.mjs
// Everything must stay inside the window and the bar must not overlap itself. Screenshots go to $SHOTS (default /tmp/postmap-mobile).
export const options = { width: 420, height: 820, dpr: 2 };

export default async (page) => {
  const base = process.env.BASE ?? 'http://127.0.0.1:8740', shots = process.env.SHOTS ?? '/tmp/postmap-mobile';
  const results = [];
  const check = (name, ok, detail = '') => results.push({ name, ok: !!ok, detail });
  await page.goto(base + '/');
  await page.waitFor('window.__map && window.__map.S.n > 0 && document.querySelector("#loading").hidden');
  await page.waitFor('window.__map.stats.complete', 120000);
  const lay = () => page.eval(`(() => {
    const r = (sel) => { const e = document.querySelector(sel); if (!e || e.hidden) return null; const b = e.getBoundingClientRect(); return { x: b.x, y: b.y, r: b.right, b: b.bottom }; };
    const parts = ['#brand', '#search-wrap', '#modes', '#filter-wrap'].map(r);
    const touch = (a, b) => a.x < b.r - 1 && a.r > b.x + 1 && a.y < b.b - 1 && a.b > b.y + 1;
    let overlaps = 0;
    for (let i = 0; i < parts.length; i++) for (let j = i + 1; j < parts.length; j++) if (touch(parts[i], parts[j])) overlaps++;
    const inside = (b) => !b || (b.x >= -1 && b.r <= innerWidth + 1 && b.y >= -1 && b.b <= innerHeight + 1);
    return { overlaps, parts, scrollW: document.documentElement.scrollWidth, vw: innerWidth, inside: parts.every(inside), outline: r('#outline'), drawer: r('#drawer'), zoom: r('#zoom'), headerBottom: Math.max(...parts.map((p) => p.b)) };
  })()`);
  let l = await lay();
  check('the bar has no overlapping parts', l.overlaps === 0, JSON.stringify(l.parts));
  check('the bar is inside the window', l.inside, JSON.stringify(l.parts));
  check('the page does not scroll sideways', l.scrollW <= l.vw, `${l.scrollW} in ${l.vw}`);
  check('the list of areas starts hidden', l.outline === null || (l.outline.r <= 0) || await page.eval('getComputedStyle(document.querySelector("#outline")).visibility === "hidden"'));
  await page.screenshot(shots + '-overview.png');
  await page.eval('window.__map.selectSeg(window.__map.S.segs.filter((s) => s.examples.length > 3)[0].id, false)');
  await page.waitFor('document.querySelectorAll("#drawer .post").length >= 2');
  await page.wait(500);  // the drawer slides in
  l = await lay();
  check('the drawer is inside the window, below the bar', l.drawer && l.drawer.x >= 0 && l.drawer.r <= l.vw + 1 && l.drawer.b <= 820 && l.drawer.y >= l.headerBottom - 1, JSON.stringify(l.drawer));
  check('the zoom buttons stay in the window', l.zoom && l.zoom.r <= l.vw && l.zoom.b <= 820, JSON.stringify(l.zoom));
  await page.screenshot(shots + '-drawer.png');
  await page.eval('document.querySelector("#outline-toggle").click()');
  await page.wait(500);
  l = await lay();
  check('the list of areas opens inside the window', l.outline && l.outline.x >= 0 && l.outline.r <= l.vw && l.outline.y >= l.headerBottom - 1, JSON.stringify(l.outline));
  await page.screenshot(shots + '-outline.png');
  const errors = page.logs.filter((x) => /EXCEPTION|console\.error|log\.error/.test(x));
  check('no console errors', errors.length === 0, errors.join(' | '));
  for (const r of results) console.log(`${r.ok ? 'ok  ' : 'FAIL'} ${r.name}${r.detail !== '' ? ' (' + r.detail + ')' : ''}`);
  const failed = results.filter((r) => !r.ok);
  console.log(failed.length ? `${failed.length} of ${results.length} checks failed` : `all ${results.length} checks passed`);
  if (failed.length) process.exitCode = 1;
};
