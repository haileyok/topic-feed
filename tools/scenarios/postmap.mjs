// Headless checks of the post map: node tools/cdp.mjs tools/scenarios/postmap.mjs
// Needs the service running (BASE, default http://127.0.0.1:8740) with a snapshot loaded. Exits non-zero on a failed check.
// Screenshots go to $SHOTS (default /tmp/postmap-*.png).
export const options = { width: 1500, height: 900 };

export default async (page) => {
  const base = process.env.BASE ?? 'http://127.0.0.1:8740', shots = process.env.SHOTS ?? '/tmp/postmap';
  const results = [];
  const check = (name, ok, detail = '') => { results.push({ name, ok: !!ok, detail }); };

  await page.goto(base + '/');
  await page.waitFor('window.__map && window.__map.S.n > 0 && document.querySelector("#loading").hidden');
  await page.wait(800);

  // Lit pixels: read the canvas back in the same task as the draw.
  const lit = () => page.eval(`(() => {
    const m = window.__map, gl = m.gl, w = gl.canvas.width, h = gl.canvas.height;
    m.draw();
    const px = new Uint8Array(w * h * 4);
    gl.readPixels(0, 0, w, h, gl.RGBA, gl.UNSIGNED_BYTE, px);
    let n = 0;
    for (let i = 0; i < px.length; i += 4) if (px[i] + px[i + 1] + px[i + 2] > 120) n++;
    return n / (w * h);
  })()`);

  const info = await page.eval(`(() => {
    const m = window.__map, S = m.S, st = document.querySelector('#stage').getBoundingClientRect();
    const cov = document.elementFromPoint(st.x + st.width / 2, st.y + st.height / 2);
    return { n: S.n, segs: S.segs.length, areas: S.areas.length, stage: [st.width, st.height], coveredBy: cov && cov.id,
      outline: document.querySelectorAll('#outline > div').length, drawerHidden: document.querySelector('#drawer').hidden,
      tipHidden: document.querySelector('#tip').hidden, meta: document.querySelector('#meta').textContent, labels: m.labelRects.length };
  })()`);
  check('posts loaded', info.n > 1000, info.n);
  check('segments and areas loaded', info.segs > 10 && info.areas > 3, `${info.segs} / ${info.areas}`);
  check('nothing covers the map', info.coveredBy === 'labels', info.coveredBy);
  check('stage has size', info.stage[0] > 600 && info.stage[1] > 400, info.stage.join('x'));
  check('outline has every area', info.outline === info.areas, info.outline);
  check('drawer and tooltip start hidden', info.drawerHidden && info.tipHidden);
  check('area labels drawn at the start', info.labels > 0, info.labels);
  const frac = await lit();
  check('points are drawn', frac > 0.01, frac.toFixed(4));
  await page.screenshot(shots + '-overview.png');

  // Colour modes all draw something and change the picture.
  for (let mode = 1; mode <= 5; mode++) {
    await page.eval(`window.__map.setMode(${mode})`);
    const f = await lit();
    check('colour mode ' + mode + ' draws', f > 0.01, f.toFixed(4));
  }
  await page.eval('window.__map.setMode(0)');

  // Selecting an area opens the drawer and dims the rest.
  await page.eval('window.__map.selectArea(window.__map.S.areas[0].id)');
  await page.wait(700);
  const area = await page.eval(`({ hidden: document.querySelector('#drawer').hidden, h2: document.querySelector('#drawer h2')?.textContent,
    hash: location.hash, sel: document.querySelectorAll('#outline .node-row.sel').length })`);
  check('area drawer opens', !area.hidden && area.h2, area.h2);
  check('area is in the hash', area.hash === '#area/' + (await page.eval('window.__map.S.areas[0].id')), area.hash);
  check('outline marks the area', area.sel === 1, area.sel);
  await page.screenshot(shots + '-area.png');

  // A segment: drawer with example posts, tone bars, neighbours.
  const segId = await page.eval('window.__map.S.segs.filter((s) => s.examples.length > 3 && s.neighbours.length)[0].id');
  await page.eval(`window.__map.selectSeg(${segId})`);
  await page.waitFor('document.querySelectorAll("#drawer .post").length >= 3');
  await page.wait(700);
  const seg = await page.eval(`({ h2: document.querySelector('#drawer h2').textContent, posts: document.querySelectorAll('#drawer .post').length,
    bars: document.querySelectorAll('#drawer .bar').length, nb: document.querySelectorAll('#drawer .list .item').length,
    zoom: window.__map.S.view.scale / window.__map.S.fit, crumbs: document.querySelector('#drawer .crumbs').textContent })`);
  check('segment drawer has its title', seg.h2.length > 3, seg.h2);
  check('segment drawer shows example posts', seg.posts >= 3, seg.posts);
  check('segment drawer shows tone bars', seg.bars >= 6, seg.bars);
  check('segment drawer lists neighbouring segments', seg.nb >= 1, seg.nb);
  check('segment drawer has breadcrumbs through its area', seg.crumbs.includes('›'), seg.crumbs);
  check('view flew in on the segment', seg.zoom > 3, seg.zoom.toFixed(1));
  await page.screenshot(shots + '-segment.png');

  // Search inside the segment.
  const word = await page.eval(`window.__map.S.postCache.size && [...window.__map.S.postCache.values()].find((p) => p.s === ${segId}).t.split(/\\s+/).find((w) => w.length >= 4) || 'the'`);
  await page.eval(`(() => { const i = document.querySelector('#drawer .mini input'); i.value = ${JSON.stringify(word)}; i.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' })); })()`);
  await page.waitFor('document.querySelectorAll("#drawer .mini ~ div .post").length >= 1');
  const inside = await page.eval(`[...document.querySelectorAll('#drawer .mini ~ div .post')].length`);
  check('search inside a segment finds posts', inside >= 1, `${inside} for ${word}`);

  // Hover over a point shows a tooltip; clicking opens the post. A post of the segment that no label covers, found by
  // centring the view on candidates in turn (set directly, and drawn at once: the flight is not what is tested here).
  const spot0 = await page.eval(`(() => {
    const m = window.__map, S = m.S, r = document.querySelector('#labels').getBoundingClientRect();
    const mine = [];
    for (let i = 0; i < S.n && mine.length < 4000; i++) if (S.seg[i] === ${segId}) mine.push(i);
    const why = { tried: 0, covered: 0, notNearest: 0, posts: mine.length, sel: JSON.stringify(S.sel) };
    for (const i of mine.filter((_, k) => k % 7 === 0).slice(0, 80)) {
      why.tried++;
      S.view = { cx: S.xy[2 * i], cy: S.xy[2 * i + 1], scale: S.fit * 60 };
      m.draw();
      const x = S.W / 2, y = S.H / 2;
      if (m.labelRects.some((q) => x >= q.x - 12 && x <= q.x + q.w + 12 && y >= q.y - 12 && y <= q.y + q.h + 12)) { why.covered++; continue; }
      if (m.nearest(...m.toWorld(x, y), 6 / S.view.scale) === i) return { i, x: r.x + x, y: r.y + y };
      why.notNearest++;
    }
    return { why };
  })()`);
  const spot = spot0.why ? null : spot0;
  check('found a post to hover', spot, JSON.stringify(spot0));
  if (spot) {
    await page.move(spot.x, spot.y);
    await page.waitFor('!document.querySelector("#tip").hidden && document.querySelector("#tip .txt")');
    check('hover shows the post text', await page.eval('document.querySelector("#tip .txt").textContent.length > 0'));
    await page.click(spot.x, spot.y);
    await page.waitFor('document.querySelector("#drawer .postbig")');
    const post = await page.eval(`({ text: document.querySelector('#drawer .postbig').textContent, hash: location.hash,
      link: document.querySelector('#drawer a[href^="https://bsky.app/profile/"]')?.getAttribute('rel') })`);
    check('clicking a post opens it', post.text.length > 0 && post.hash === '#post/' + spot.i, post.hash);
    check('post links out safely', post.link === 'noopener noreferrer', post.link);
    await page.screenshot(shots + '-post.png');
  }

  // Full-text search: a ringed result set and a drawer.
  await page.eval(`window.__map.runSearch('the')`);
  await page.waitFor('window.__map.S.hits.length > 0 && document.querySelector("#drawer h2")?.textContent.includes("the")');
  check('text search finds posts and rings them', await page.eval('window.__map.S.hits.length > 5'));
  await page.screenshot(shots + '-search.png');

  // Typing in the box offers topics and posts; the keyboard picks one.
  await page.eval('window.__map.S.sel = null; document.querySelector("#search").focus()');
  await page.eval(`(() => { const i = document.querySelector('#search'); i.value = ${JSON.stringify((await page.eval('window.__map.S.segs[0].title')).split(' ')[0])}; i.dispatchEvent(new Event('input')); })()`);
  await page.waitFor('!document.querySelector("#results").hidden && document.querySelectorAll("#results .row").length > 0');
  check('typing offers results', true);
  await page.key('Escape');

  // Filters: hiding bots and spam leaves fewer lit pixels and says how many segments remain.
  await page.eval('history.replaceState(null, "", location.pathname)');
  const before = await lit();
  await page.eval(`(() => { const c = document.querySelector('#f-bots'); c.checked = true; c.dispatchEvent(new Event('change')); })()`);
  const after = await lit();
  const countText = await page.eval('document.querySelector("#f-count").textContent');
  check('hiding bots removes points', after < before, `${before.toFixed(4)} -> ${after.toFixed(4)}`);
  check('filter says how many segments remain', /segments shown/.test(countText), countText);
  await page.eval(`(() => { const c = document.querySelector('#f-bots'); c.checked = false; c.dispatchEvent(new Event('change')); })()`);
  await page.eval(`(() => { const c = document.querySelector('#f-coh'); c.value = '5'; c.dispatchEvent(new Event('change')); })()`);
  const high = await lit();
  check('minimum coherence removes points', high < before, high.toFixed(4));
  await page.eval(`(() => { const c = document.querySelector('#f-coh'); c.value = '0'; c.dispatchEvent(new Event('change')); })()`);

  // Zoomed in far enough, segments are named.
  const segLabels = await page.eval(`(() => { const m = window.__map, S = m.S; S.view = { cx: (S.bounds[0] + S.bounds[2]) / 2, cy: (S.bounds[1] + S.bounds[3]) / 2, scale: S.fit * 6 }; m.draw(); return m.labelRects.filter((r) => r.kind === 'seg').length; })()`);
  check('segments are labelled when zoomed in', segLabels > 0, segLabels);
  await page.screenshot(shots + '-zoomed.png');

  // Escape clears, and a direct link restores a selection.
  await page.key('Escape');
  await page.goto(base + '/#seg/' + segId);
  await page.waitFor('window.__map && !document.querySelector("#drawer").hidden && document.querySelector("#drawer h2")');
  check('a link to a segment opens it', await page.eval(`window.__map.S.sel && window.__map.S.sel.id === ${segId}`));

  // The page's own errors.
  const errors = page.logs.filter((l) => /EXCEPTION|console\.error|log\.error/.test(l));
  check('no console errors', errors.length === 0, errors.join(' | '));

  const failed = results.filter((r) => !r.ok);
  for (const r of results) console.log(`${r.ok ? 'ok  ' : 'FAIL'} ${r.name}${r.detail !== '' ? ' (' + r.detail + ')' : ''}`);
  console.log(failed.length ? `${failed.length} of ${results.length} checks failed` : `all ${results.length} checks passed`);
  if (failed.length) process.exitCode = 1;
};
