// The post map. One WebGL point per post, placed by what the post says (see postclusters/build_snapshot.py) and
// coloured by area, segment quality, kind or the existing classifier's tone; a 2D canvas on top draws labels and
// the selection. Everything the page shows comes from /snap/<id>/ (immutable files) and /api/v1 (post texts, search).
// Text from the data (post text, titles) only ever goes in through textContent.

const $ = (s) => document.querySelector(s);

function h(tag, props = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === 'class') e.className = v;
    else if (k === 'style') Object.assign(e.style, v);
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v != null && v !== false) e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) e.append(c.nodeType ? c : String(c));
  return e;
}

const fmtN = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e4 ? Math.round(n / 1e3) + 'k' : n >= 1e3 ? (n / 1e3).toFixed(1) + 'k' : String(n));
const pct = (x) => (x == null ? '–' : Math.round(x * 100) + '%');
const clamp = (x, a, b) => Math.min(b, Math.max(a, x));
const words = (s) => String(s).replaceAll('_', ' ').replace('/', ' / ');
const when = (at) => new Date(at * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';

const KINDS = ['discussion', 'personal_update', 'news_link', 'art_media', 'humor_meme', 'promo_ad', 'spam', 'automated_bot', 'adult', 'other'];
const KIND_RGB = {
  discussion: [110, 168, 255], personal_update: [79, 209, 122], news_link: [241, 185, 59], art_media: [190, 130, 240], humor_meme: [255, 140, 90],
  promo_ad: [230, 90, 150], spam: [239, 91, 91], automated_bot: [150, 160, 175], adult: [200, 60, 200], other: [90, 96, 112],
};
const BOTLIKE = new Set(['automated_bot', 'spam', 'promo_ad']);

const S = {
  map: null, id: '', segs: [], areas: [], n: 0,
  xy: null, seg: null, outr: null, sent: null, crit: null,
  bounds: [0, 0, 1, 1], view: { cx: 0, cy: 0, scale: 400 }, fit: 400,
  mode: 0, sel: null, hover: -1,
  filters: { bots: false, adult: false, sides: false, coh: 0 },
  hits: [], hitQuery: '',
  W: 0, H: 0, dpr: 1,
  color: [], areaColor: [], avg: {}, postCache: new Map(),
};

// ---------- data ----------

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`${url}: ${r.status}`);
  return r.json();
}

function hsl(hh, s, l) {
  s /= 100; l /= 100;
  const k = (n) => (n + hh / 30) % 12;
  const a = s * Math.min(l, 1 - l);
  const f = (n) => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
  return [Math.round(f(0) * 255), Math.round(f(8) * 255), Math.round(f(4) * 255)];
}

async function boot() {
  const { id } = await getJSON('/api/latest');
  const base = `/snap/${id}/`;
  const bin = (n) => fetch(base + n).then((r) => r.arrayBuffer());
  const [map, xyb, segb, ob, sb, cb] = await Promise.all([getJSON(base + 'map.json'), bin('xy.f32'), bin('seg.u16'), bin('outraged.u8'), bin('sentiment.u8'), bin('critical.u8')]);
  S.id = id; S.map = map; S.segs = map.segments; S.areas = map.areas; S.n = map.n_posts;
  S.xy = new Float32Array(xyb); S.seg = new Uint16Array(segb);
  S.outr = new Uint8Array(ob); S.sent = new Uint8Array(sb); S.crit = new Uint8Array(cb);
  if (S.xy.length !== 2 * S.n || S.seg.length !== S.n) throw new Error('the map files do not agree');

  // Colours: an area is a hue (the biggest get the first, most distinct hues); its segments are shades of it.
  const byPosts = [...S.areas].sort((a, b) => b.posts - a.posts);
  byPosts.forEach((a, i) => { S.areaColor[a.id] = hsl((i * 137.508) % 360, 72, 58); });
  for (const sg of S.segs) {
    const hash = (sg.id * 2654435761) >>> 0, a = S.areas[sg.area];
    const hue = ((byPosts.indexOf(a) * 137.508) % 360 + ((hash % 36) - 18) + 360) % 360;
    S.color[sg.id] = hsl(hue, 55 + ((hash >> 8) % 25), 44 + ((hash >> 16) % 22));
  }
  // Network averages (weighted by posts), for the tick marks in the drawers.
  let tot = 0; const sum = {};
  for (const sg of S.segs) { tot += sg.n_texts; for (const k of ['outraged', 'sentiment', 'critical', 'spam', 'promo', 'news']) sum[k] = (sum[k] || 0) + sg[k] * sg.n_texts; }
  for (const k in sum) S.avg[k] = sum[k] / tot;

  let x0 = 1e9, y0 = 1e9, x1 = -1e9, y1 = -1e9;
  for (let i = 0; i < S.n; i++) {
    const x = S.xy[2 * i], y = S.xy[2 * i + 1];
    if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y;
  }
  S.bounds = [x0, y0, x1, y1];
  $('#meta').textContent = `${fmtN(S.n)} posts (${fmtN(map.n_posts_total)} counting repeats) · ${S.segs.length} segments · ${S.areas.length} areas · ${when(map.from)} to ${when(map.to)}`;
  buildGrid();
  initGL();
  buildOutline();
  resize();
  setPalette();
  fitView();
  $('#loading').hidden = true;
  requestDraw();
  route();
}

// ---------- spatial index for hover and click ----------

const GRID = 700;
let cellStart, cellItems, gx0, gy0, gcw;
function buildGrid() {
  const [x0, y0, x1, y1] = S.bounds;
  gx0 = x0; gy0 = y0; gcw = Math.max(x1 - x0, y1 - y0) / GRID * 1.0001;
  const cs = new Int32Array(GRID * GRID + 1), cell = (i) => clamp(Math.floor((S.xy[2 * i + 1] - gy0) / gcw), 0, GRID - 1) * GRID + clamp(Math.floor((S.xy[2 * i] - gx0) / gcw), 0, GRID - 1);
  for (let i = 0; i < S.n; i++) cs[cell(i) + 1]++;
  for (let c = 0; c < GRID * GRID; c++) cs[c + 1] += cs[c];
  const fill = cs.slice(0, GRID * GRID), items = new Int32Array(S.n);
  for (let i = 0; i < S.n; i++) items[fill[cell(i)]++] = i;
  cellStart = cs; cellItems = items;
}

function eachNear(wx, wy, radius, fn) {
  const r = Math.ceil(radius / gcw), cx = Math.floor((wx - gx0) / gcw), cy = Math.floor((wy - gy0) / gcw);
  for (let y = Math.max(0, cy - r); y <= Math.min(GRID - 1, cy + r); y++) {
    for (let x = Math.max(0, cx - r); x <= Math.min(GRID - 1, cx + r); x++) {
      const c = y * GRID + x;
      for (let k = cellStart[c]; k < cellStart[c + 1]; k++) fn(cellItems[k]);
    }
  }
}

let segAlpha;
function nearest(wx, wy, radius) {
  let best = -1, bd = radius * radius;
  eachNear(wx, wy, radius, (i) => {
    if (segAlpha[S.seg[i]] < 128) return;
    const dx = S.xy[2 * i] - wx, dy = S.xy[2 * i + 1] - wy, d = dx * dx + dy * dy;
    if (d < bd) { bd = d; best = i; }
  });
  return best;
}

// The k posts nearest to post i on the map, whatever their segment.
function nearPosts(i, k) {
  const wx = S.xy[2 * i], wy = S.xy[2 * i + 1];
  for (let r = 0.004; r < 0.5; r *= 2.5) {
    const c = [];
    eachNear(wx, wy, r, (j) => { if (j !== i) c.push([(S.xy[2 * j] - wx) ** 2 + (S.xy[2 * j + 1] - wy) ** 2, j]); });
    if (c.length >= k) return c.sort((a, b) => a[0] - b[0]).slice(0, k).map((e) => e[1]);
  }
  return [];
}

// ---------- WebGL ----------

const PW = 256; // palette texture width
let gl, prog, U = {}, palTex;
const VS = `#version 300 es
in vec2 a_pos; in uint a_seg; in uvec4 a_misc;
uniform vec2 u_center; uniform float u_scale; uniform vec2 u_res; uniform int u_mode; uniform float u_size; uniform float u_dpr; uniform float u_alpha;
uniform sampler2D u_pal;
out vec4 v_col;
vec3 ramp(float t) {
  vec3 a = vec3(0.20, 0.42, 0.85), b = vec3(0.93, 0.86, 0.38), c = vec3(0.92, 0.22, 0.22);
  return t < 0.5 ? mix(a, b, t * 2.0) : mix(b, c, (t - 0.5) * 2.0);
}
void main() {
  int s = int(a_seg);
  vec4 pal = texelFetch(u_pal, ivec2(s % ${PW}, s / ${PW}), 0);
  vec3 col = pal.rgb;
  if (u_mode == 3) col = ramp(float(a_misc.z) / 255.0);
  else if (u_mode == 4) col = ramp(1.0 - float(a_misc.y) / 255.0);
  else if (u_mode == 5) col = ramp(float(a_misc.x) / 255.0);
  vec2 p = (a_pos - u_center) * u_scale;
  gl_Position = vec4(p.x / (u_res.x * 0.5), -p.y / (u_res.y * 0.5), 0.0, 1.0);
  gl_PointSize = u_size * u_dpr;
  v_col = vec4(col, pal.a * u_alpha);
}`;
const FS = `#version 300 es
precision mediump float;
in vec4 v_col; out vec4 o;
void main() {
  vec2 d = gl_PointCoord - 0.5; float r = length(d);
  if (r > 0.5) discard;
  o = vec4(v_col.rgb, v_col.a * smoothstep(0.5, 0.3, r));
}`;

function initGL() {
  gl = $('#gl').getContext('webgl2', { antialias: false, alpha: false });
  if (!gl) { $('#loading').textContent = 'This page needs WebGL 2.'; $('#loading').hidden = false; throw new Error('no webgl2'); }
  const sh = (type, src) => { const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s); if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(s)); return s; };
  prog = gl.createProgram();
  gl.attachShader(prog, sh(gl.VERTEX_SHADER, VS)); gl.attachShader(prog, sh(gl.FRAGMENT_SHADER, FS));
  gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(prog));
  for (const u of ['u_center', 'u_scale', 'u_res', 'u_mode', 'u_size', 'u_dpr', 'u_alpha', 'u_pal']) U[u] = gl.getUniformLocation(prog, u);
  gl.bindVertexArray(gl.createVertexArray());
  const buf = (data, name, size, type, integer) => {
    const loc = gl.getAttribLocation(prog, name), b = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, b); gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
    gl.enableVertexAttribArray(loc);
    if (integer) gl.vertexAttribIPointer(loc, size, type, 0, 0); else gl.vertexAttribPointer(loc, size, type, false, 0, 0);
  };
  buf(S.xy, 'a_pos', 2, gl.FLOAT, false);
  buf(S.seg, 'a_seg', 1, gl.UNSIGNED_SHORT, true);
  const misc = new Uint8Array(S.n * 4);
  for (let i = 0; i < S.n; i++) { misc[4 * i] = S.outr[i]; misc[4 * i + 1] = S.sent[i]; misc[4 * i + 2] = S.crit[i]; }
  buf(misc, 'a_misc', 4, gl.UNSIGNED_BYTE, true);
  palTex = gl.createTexture(); gl.bindTexture(gl.TEXTURE_2D, palTex);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  gl.enable(gl.BLEND); gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
}

const passes = (sg) => {
  const f = S.filters;
  if (f.bots && (BOTLIKE.has(sg.kind) || sg.flags.includes('bot/spam') || sg.flags.includes('templated bot posts'))) return false;
  if (f.adult && sg.kind === 'adult') return false;
  if (f.sides && !(sg.disagreement && sg.disagreement.length)) return false;
  if (sg.coherence < f.coh) return false;
  return true;
};

function cohColor(c) { return [[225, 70, 70], [235, 140, 60], [235, 215, 80], [110, 200, 110], [60, 160, 235]][clamp(c, 1, 5) - 1]; }

// Each segment's colour (for the modes that colour by segment) and how visible it is: a filtered-out segment all
// but vanishes, and everything outside the selection is dimmed.
function setPalette() {
  const n = S.segs.length, rows = Math.ceil(n / PW), data = new Uint8Array(PW * rows * 4);
  segAlpha = new Uint8Array(n);
  let shown = 0, shownPosts = 0;
  const sel = S.sel;
  const inSel = (sg) => !sel || sel.type === 'search' || (sel.type === 'area' ? sg.area === sel.id : sel.type === 'seg' ? sg.id === sel.id : sg.id === S.seg[sel.id]);
  for (const sg of S.segs) {
    const col = S.mode === 1 ? cohColor(sg.coherence) : S.mode === 2 ? (KIND_RGB[sg.kind] || KIND_RGB.other) : S.color[sg.id];
    const ok = passes(sg), a = !ok ? 10 : inSel(sg) ? 255 : 28;
    if (ok) { shown++; shownPosts += sg.n_posts; }
    data.set([col[0], col[1], col[2], a], 4 * sg.id); segAlpha[sg.id] = a;
  }
  gl.bindTexture(gl.TEXTURE_2D, palTex);
  gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA8, PW, rows, 0, gl.RGBA, gl.UNSIGNED_BYTE, data);
  const active = S.filters.bots || S.filters.adult || S.filters.sides || S.filters.coh > 0;
  $('#filter-btn').classList.toggle('active', active);
  $('#f-count').textContent = active ? `${shown} of ${n} segments shown (${fmtN(shownPosts)} posts)` : '';
  requestDraw();
}

// ---------- view ----------

const toScreen = (x, y) => [S.W / 2 + (x - S.view.cx) * S.view.scale, S.H / 2 + (y - S.view.cy) * S.view.scale];
const toWorld = (sx, sy) => [S.view.cx + (sx - S.W / 2) / S.view.scale, S.view.cy + (sy - S.H / 2) / S.view.scale];

function resize() {
  const st = $('#stage'); S.dpr = window.devicePixelRatio || 1;
  S.W = st.clientWidth; S.H = st.clientHeight;
  for (const c of [$('#gl'), $('#labels')]) { c.width = Math.round(S.W * S.dpr); c.height = Math.round(S.H * S.dpr); }
  if (S.n) { const [x0, y0, x1, y1] = S.bounds; S.fit = Math.min(S.W / (x1 - x0), S.H / (y1 - y0)) * 0.92; }
  requestDraw();
}
function fitView() { const [x0, y0, x1, y1] = S.bounds; S.view = { cx: (x0 + x1) / 2, cy: (y0 + y1) / 2, scale: S.fit }; }

let anim = null;
function flyTo(cx, cy, scale) {
  const from = { ...S.view }, t0 = performance.now();
  const step = (now) => {
    const t = clamp((now - t0) / 450, 0, 1), e = 1 - Math.pow(1 - t, 3);
    S.view = { cx: from.cx + (cx - from.cx) * e, cy: from.cy + (cy - from.cy) * e, scale: Math.exp(Math.log(from.scale) + (Math.log(scale) - Math.log(from.scale)) * e) };
    requestDraw();
    if (t < 1) anim = requestAnimationFrame(step);
  };
  cancelAnimationFrame(anim); anim = requestAnimationFrame(step);
}
function flyToNode(nd, mult = 2.4) { flyTo(nd.x, nd.y, Math.min(S.W, S.H) / (2 * Math.max(nd.r * mult, 0.01))); }

// How many posts there are against the 855,000 the point size and opacity were tuned on: more posts, finer and fainter points.
const density = () => clamp(Math.sqrt(900000 / S.n), 0.55, 1);

let drawQueued = false;
function requestDraw() { if (!drawQueued) { drawQueued = true; requestAnimationFrame(draw); } }

function draw() {
  drawQueued = false;
  if (!gl || !S.n) return;
  const zoom = S.view.scale / S.fit;
  gl.viewport(0, 0, $('#gl').width, $('#gl').height);
  gl.clearColor(0.055, 0.063, 0.086, 1); gl.clear(gl.COLOR_BUFFER_BIT);
  gl.useProgram(prog);
  gl.uniform2f(U.u_center, S.view.cx, S.view.cy); gl.uniform1f(U.u_scale, S.view.scale);
  gl.uniform2f(U.u_res, S.W, S.H); gl.uniform1i(U.u_mode, S.mode); gl.uniform1f(U.u_dpr, S.dpr);
  gl.uniform1f(U.u_size, clamp(1.7 * density() + Math.log2(zoom) * 0.9, 1.2, 10));
  gl.uniform1f(U.u_alpha, clamp(0.6 * density() + Math.log2(zoom) * 0.1, 0.4, 0.92));
  gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, palTex); gl.uniform1i(U.u_pal, 0);
  gl.drawArrays(gl.POINTS, 0, S.n);
  drawLabels();
}

// ---------- labels and overlay ----------

let labelRects = [];
function drawLabels() {
  const cv = $('#labels'), ctx = cv.getContext('2d');
  ctx.setTransform(S.dpr, 0, 0, S.dpr, 0, 0); ctx.clearRect(0, 0, S.W, S.H);
  labelRects = [];
  const zoom = S.view.scale / S.fit, sel = S.sel;

  // Search hits and the selected post.
  ctx.lineWidth = 1.5; ctx.strokeStyle = 'rgba(255,230,120,0.95)';
  for (const i of S.hits) { const [x, y] = toScreen(S.xy[2 * i], S.xy[2 * i + 1]); ctx.beginPath(); ctx.arc(x, y, 5, 0, 7); ctx.stroke(); }
  if (sel && sel.type === 'post') {
    const [x, y] = toScreen(S.xy[2 * sel.id], S.xy[2 * sel.id + 1]);
    ctx.strokeStyle = '#fff'; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(x, y, 8, 0, 7); ctx.stroke();
  }
  if (sel && (sel.type === 'area' || sel.type === 'seg')) {
    const nd = sel.type === 'area' ? S.areas[sel.id] : S.segs[sel.id], [sx, sy] = toScreen(nd.x, nd.y);
    ctx.strokeStyle = 'rgba(255,255,255,0.35)'; ctx.setLineDash([4, 4]); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.arc(sx, sy, Math.max(nd.r * S.view.scale * 1.3, 10), 0, 7); ctx.stroke(); ctx.setLineDash([]);
  }

  // Which level is named follows the zoom: areas first, then segments. Labels never overlap; the biggest win.
  const items = [];
  if (zoom < 3.2) {
    for (const a of S.areas) {
      if (a.r * S.view.scale < 22) continue;
      if (sel && sel.type === 'area' && sel.id !== a.id) continue;
      items.push({ kind: 'area', nd: a, text: a.title, count: a.posts, weight: a.posts, color: S.areaColor[a.id] });
    }
  }
  const picked = sel && (sel.type === 'area' || sel.type === 'seg');
  if (zoom >= 2 || picked) {
    const MIN = zoom >= 2 ? 5 : 0;
    for (const sg of S.segs) {
      if (segAlpha[sg.id] < 128 || sg.r * S.view.scale < MIN) continue;
      if (sel && sel.type === 'area' && sg.area !== sel.id) continue;
      if (sel && sel.type === 'seg' && zoom < 2 && sg.id !== sel.id) continue;
      items.push({ kind: 'seg', nd: sg, text: sg.title, count: sg.n_posts, weight: sg.n_texts, color: S.color[sg.id] });
    }
  }
  const vis = items.map((it) => ({ ...it, p: toScreen(it.nd.x, it.nd.y) })).filter((it) => it.p[0] > -60 && it.p[1] > -30 && it.p[0] < S.W + 60 && it.p[1] < S.H + 30);
  vis.sort((a, b) => (a.kind === b.kind ? b.weight - a.weight : a.kind === 'area' ? -1 : 1));
  ctx.textAlign = 'center'; ctx.textBaseline = 'middle'; ctx.lineJoin = 'round';
  let drawn = 0;
  for (const it of vis) {
    if (drawn > 90) break;
    const rpx = it.nd.r * S.view.scale, fs = it.kind === 'area' ? clamp(rpx * 0.1, 14, 19) : 13;
    const lines = splitTitle(it.text, it.kind === 'area' ? 24 : 34);
    ctx.font = `600 ${fs}px system-ui, sans-serif`;
    const lh = fs * 1.25, w = Math.max(...lines.map((l) => ctx.measureText(l).width)) + 20, hgt = lh * lines.length + 8;
    const rect = { x: it.p[0] - w / 2, y: it.p[1] - hgt / 2, w, h: hgt, kind: it.kind, id: it.nd.id };
    if (labelRects.some((r) => rect.x - 3 < r.x + r.w && rect.x + rect.w + 3 > r.x && rect.y - 2 < r.y + r.h && rect.y + rect.h + 2 > r.y)) continue;
    labelRects.push(rect); drawn++;
    // A dark pill, so the text reads against any colour behind it, with a bar in the area's or segment's own colour.
    ctx.fillStyle = 'rgba(10,12,18,0.86)'; ctx.strokeStyle = 'rgba(255,255,255,0.18)'; ctx.lineWidth = 1;
    ctx.beginPath(); ctx.roundRect(rect.x, rect.y, rect.w, rect.h, 6); ctx.fill(); ctx.stroke();
    ctx.fillStyle = `rgb(${it.color})`; ctx.beginPath(); ctx.roundRect(rect.x + 4, rect.y + 5, 3, rect.h - 10, 2); ctx.fill();
    ctx.fillStyle = '#fff';
    lines.forEach((l, k) => ctx.fillText(l, rect.x + 5 + (rect.w - 5) / 2, rect.y + 4 + lh * (k + 0.5)));
  }
}

// A title on one line, or on two split at the space nearest the middle; each line is cut to fit.
function splitTitle(t, max) {
  const cut = (x) => (x.length > max + 6 ? x.slice(0, max + 5) + '…' : x);
  if (t.length <= max) return [t];
  let best = -1;
  for (let i = 0; i < t.length; i++) if (t[i] === ' ' && (best < 0 || Math.abs(i - t.length / 2) < Math.abs(best - t.length / 2))) best = i;
  return best < 0 ? [cut(t)] : [cut(t.slice(0, best)), cut(t.slice(best + 1))];
}

// ---------- pointer ----------

function initPointer() {
  const cv = $('#labels'), pts = new Map();
  let down = null, pinch = null;
  cv.addEventListener('pointerdown', (e) => {
    cv.setPointerCapture(e.pointerId); pts.set(e.pointerId, [e.offsetX, e.offsetY]);
    down = { x: e.offsetX, y: e.offsetY, moved: false, view: { ...S.view } };
    if (pts.size === 2) { const [a, b] = [...pts.values()]; pinch = { d: Math.hypot(a[0] - b[0], a[1] - b[1]), scale: S.view.scale }; }
    cancelAnimationFrame(anim);
  });
  cv.addEventListener('pointermove', (e) => {
    if (pts.has(e.pointerId)) pts.set(e.pointerId, [e.offsetX, e.offsetY]);
    if (pinch && pts.size === 2) {
      const [a, b] = [...pts.values()], d = Math.hypot(a[0] - b[0], a[1] - b[1]);
      S.view.scale = clamp(pinch.scale * d / pinch.d, S.fit * 0.7, S.fit * 2000); down.moved = true; requestDraw(); return;
    }
    if (down && pts.has(e.pointerId)) {
      const dx = e.offsetX - down.x, dy = e.offsetY - down.y;
      if (Math.abs(dx) + Math.abs(dy) > 4) { down.moved = true; cv.classList.add('drag'); hideTip(); }
      if (down.moved) { S.view.cx = down.view.cx - dx / S.view.scale; S.view.cy = down.view.cy - dy / S.view.scale; requestDraw(); }
      return;
    }
    hoverAt(e.offsetX, e.offsetY);
  });
  const up = (e) => {
    pts.delete(e.pointerId); cv.classList.remove('drag');
    if (pts.size < 2) pinch = null;
    if (down && !down.moved && pts.size === 0) click(e.offsetX, e.offsetY);
    if (pts.size === 0) down = null;
  };
  cv.addEventListener('pointerup', up); cv.addEventListener('pointercancel', up);
  cv.addEventListener('pointerleave', hideTip);
  cv.addEventListener('wheel', (e) => {
    e.preventDefault();
    const [wx, wy] = toWorld(e.offsetX, e.offsetY);
    S.view.scale = clamp(S.view.scale * Math.exp(-e.deltaY * 0.0016), S.fit * 0.7, S.fit * 2000);
    S.view.cx = wx - (e.offsetX - S.W / 2) / S.view.scale; S.view.cy = wy - (e.offsetY - S.H / 2) / S.view.scale;
    cancelAnimationFrame(anim); hideTip(); requestDraw();
  }, { passive: false });
}

let hoverTimer = 0;
function hoverAt(sx, sy) {
  const [wx, wy] = toWorld(sx, sy), i = nearest(wx, wy, 6 / S.view.scale);
  S.hover = i;
  const tip = $('#tip');
  if (i < 0) { tip.hidden = true; return; }
  const rec = S.postCache.get(i), sg = S.segs[S.seg[i]];
  tip.replaceChildren(h('b', {}, sg.title), h('div', { class: 'sub' }, `${S.areas[sg.area].title} · ${words(sg.kind)}`), rec ? h('div', { class: 'txt' }, rec.t.length > 220 ? rec.t.slice(0, 220) + '…' : rec.t) : null);
  tip.hidden = false;
  tip.style.left = Math.min(sx + 14, S.W - 300) + 'px'; tip.style.top = Math.min(sy + 14, S.H - 110) + 'px';
  clearTimeout(hoverTimer);
  if (!rec) hoverTimer = setTimeout(async () => { try { await fetchPosts([i]); } catch { return; } if (S.hover === i) hoverAt(sx, sy); }, 150);
}
function hideTip() { $('#tip').hidden = true; S.hover = -1; }

function click(sx, sy) {
  const r = labelRects.find((r) => sx >= r.x && sx <= r.x + r.w && sy >= r.y && sy <= r.y + r.h);
  if (r) { r.kind === 'area' ? selectArea(r.id) : selectSeg(r.id); return; }
  const [wx, wy] = toWorld(sx, sy), i = nearest(wx, wy, 8 / S.view.scale);
  if (i >= 0) selectPost(i);
  else if (S.sel) clearSelection();
}

// ---------- posts ----------

async function fetchPosts(ids) {
  const need = ids.filter((i) => !S.postCache.has(i));
  for (let k = 0; k < need.length; k += 50) {
    const r = await getJSON('/api/v1/posts?ids=' + need.slice(k, k + 50).join(','));
    for (const p of r.posts) S.postCache.set(p.i, p);
  }
  return ids.map((i) => S.postCache.get(i)).filter(Boolean);
}

// ---------- selection ----------

function setHash(v) { history.replaceState(null, '', v ? '#' + v : location.pathname); }
function refresh() { setPalette(); showDrawer(); highlightOutline(); }

function selectArea(id, fly = true) {
  S.sel = { type: 'area', id }; S.hits = []; refresh(); setHash('area/' + id);
  if (fly) flyToNode(S.areas[id], 1.6);
}
function selectSeg(id, fly = true) {
  S.sel = { type: 'seg', id }; S.hits = []; refresh(); setHash('seg/' + id);
  if (fly) flyToNode(S.segs[id], 3);
}
async function selectPost(i, fly = false) {
  S.sel = { type: 'post', id: i }; refresh(); setHash('post/' + i);
  if (fly) flyTo(S.xy[2 * i], S.xy[2 * i + 1], Math.max(S.view.scale, S.fit * 40));
  try { await fetchPosts([i, ...nearPosts(i, 6)]); } catch { /* the drawer says so */ }
  if (S.sel && S.sel.type === 'post' && S.sel.id === i) showDrawer();
}
function clearSelection() {
  S.sel = null; S.hits = []; refresh(); setHash('');
}

function route() {
  const m = location.hash.match(/^#(area|seg|post|q)\/(.+)$/);
  if (!m) return;
  if (m[1] === 'q') { runSearch(decodeURIComponent(m[2])); return; }
  const id = +m[2];
  if (m[1] === 'area' && S.areas[id]) selectArea(id);
  else if (m[1] === 'seg' && S.segs[id]) selectSeg(id);
  else if (m[1] === 'post' && id >= 0 && id < S.n) selectPost(id, true);
}

// ---------- outline ----------

const open = new Set();
function buildOutline() {
  const root = $('#outline'); root.replaceChildren();
  for (const a of [...S.areas].sort((x, y) => y.posts - x.posts)) root.append(areaRow(a));
}
function areaRow(a) {
  const wrap = h('div', { 'data-area': a.id }), col = S.areaColor[a.id];
  const row = h('div', { class: 'node-row', title: a.title },
    h('span', { class: 'tw' }, open.has(a.id) ? '▾' : '▸'),
    h('span', { class: 'sw', style: { background: `rgb(${col})` } }),
    h('span', { class: 'nm' }, a.title), h('span', { class: 'ct' }, fmtN(a.posts)));
  row.addEventListener('click', (e) => {
    if (e.target.classList.contains('tw')) { toggle(a.id); return; }
    selectArea(a.id);
  });
  wrap.append(row);
  if (open.has(a.id)) for (const s of S.segs.filter((x) => x.area === a.id).sort((x, y) => y.n_texts - x.n_texts)) wrap.append(segRow(s));
  return wrap;
}
function segRow(s) {
  const c = S.color[s.id], coh = cohColor(s.coherence);
  const row = h('div', { class: 'node-row' + (passes(s) ? '' : ' off'), 'data-seg': s.id, style: { paddingLeft: '22px' }, title: `${s.title}\n${s.description}` },
    h('span', { class: 'tw' }), h('span', { class: 'sw', style: { background: `rgb(${c})` } }), h('span', { class: 'nm' }, s.title),
    h('span', { class: 'coh', title: 'coherence ' + s.coherence, style: { background: `rgb(${coh})` } }), h('span', { class: 'ct' }, fmtN(s.n_posts)));
  row.addEventListener('click', () => selectSeg(s.id));
  return row;
}
function toggle(id) {
  if (open.has(id)) open.delete(id); else open.add(id);
  document.querySelector(`#outline [data-area="${id}"]`).replaceWith(areaRow(S.areas[id])); highlightOutline();
}
function highlightOutline() {
  document.querySelectorAll('#outline .node-row.sel').forEach((e) => e.classList.remove('sel'));
  const sel = S.sel;
  if (!sel) return;
  const seg = sel.type === 'seg' ? S.segs[sel.id] : sel.type === 'post' ? S.segs[S.seg[sel.id]] : null;
  if (seg && !open.has(seg.area)) { open.add(seg.area); buildOutline(); }
  const el = sel.type === 'area' ? document.querySelector(`#outline [data-area="${sel.id}"] > .node-row`) : seg ? document.querySelector(`#outline [data-seg="${seg.id}"]`) : null;
  if (el) { el.classList.add('sel'); el.scrollIntoView({ block: 'nearest' }); }
}
function refreshOutlineFilters() {
  document.querySelectorAll('#outline [data-seg]').forEach((e) => e.classList.toggle('off', !passes(S.segs[+e.dataset.seg])));
}

// ---------- drawer ----------

function bar(label, value, o = {}) {
  const track = h('div', { class: 'track' }, h('div', { class: 'fill', style: { width: clamp(value, 0, 1) * 100 + '%', background: o.color || '' } }));
  if (o.mark != null) track.append(h('div', { class: 'mark', style: { left: clamp(o.mark, 0, 1) * 100 + '%' }, title: o.markTitle || 'average over all posts' }));
  return h('div', { class: 'bar' + (o.wide ? ' wide' : ''), title: o.title || '' }, h('div', { class: 'lbl' }, label), track, h('div', { class: 'val' }, pct(value)));
}
const section = (title, ...kids) => [h('h3', {}, title), ...kids];
const areaLink = (id) => h('a', { class: 'link', onclick: () => selectArea(id) }, S.areas[id].title);
const segLink = (id, text) => h('a', { class: 'link', onclick: () => selectSeg(id) }, text ?? S.segs[id].title);
const bskyUrl = (uri) => { const m = /^at:\/\/(did:[a-z0-9:]+)\/app\.bsky\.feed\.post\/([a-z0-9]+)$/.exec(uri); return m ? `https://bsky.app/profile/${m[1]}/post/${m[2]}` : null; };

function crumbs(...parts) {
  const c = h('div', { class: 'crumbs' }, h('a', { onclick: clearSelection }, 'All'));
  for (const p of parts) c.append(' › ', p);
  return c;
}

function showDrawer() {
  const d = $('#drawer'), sel = S.sel;
  if (!sel) { d.hidden = true; }
  else {
    const body = sel.type === 'area' ? areaView(S.areas[sel.id]) : sel.type === 'seg' ? segView(S.segs[sel.id]) : sel.type === 'post' ? postView(sel.id) : searchView();
    d.replaceChildren(h('button', { class: 'close', title: 'Close', onclick: clearSelection }, '×'), ...body);
    d.hidden = false;
  }
  d.scrollTop = 0;
  requestAnimationFrame(resize);
}

function postCard(p, opts = {}) {
  const sg = S.segs[p.s], link = bskyUrl(p.u);
  return h('div', { class: 'post' },
    h('div', { class: 'txt' }, p.t.length > 400 ? p.t.slice(0, 400) + '…' : p.t),
    h('div', { class: 'meta' },
      opts.noSeg ? null : segLink(sg.id), h('a', { class: 'link', onclick: () => selectPost(p.i, true) }, 'on the map'),
      link ? h('a', { class: 'link', href: link, target: '_blank', rel: 'noopener noreferrer' }, 'open ↗') : null,
      opts.tag ? h('span', { class: 'tag' }, opts.tag) : null, p.n > 1 ? h('span', { class: 'tag' }, `${p.n} copies by ${p.a} accounts`) : null));
}

async function loadInto(box, ids, tags = {}) {
  box.replaceChildren(h('div', { class: 'note' }, 'Loading…'));
  try {
    const ps = await fetchPosts(ids);
    box.replaceChildren(...(ps.length ? ps.map((p) => postCard(p, { noSeg: true, tag: tags[p.i] })) : [h('div', { class: 'note' }, 'Nothing to show.')]));
  } catch (e) { box.replaceChildren(h('div', { class: 'note' }, 'Could not load posts: ' + e.message)); }
}

function toneBars(o) {
  const a = S.avg;
  return [
    bar('Outraged', o.outraged, { mark: a.outraged, color: '#ef5b5b' }),
    bar('Critical', o.critical, { mark: a.critical, color: '#ef5b5b', title: 'Negative about, critical of, or mocking its subject' }),
    bar('Sentiment', o.sentiment, { mark: a.sentiment, color: '#6ea8ff', title: '0 very negative, 1 very positive' }),
    bar('Spam', o.spam, { mark: a.spam, color: '#f1b93b' }), bar('Promo', o.promo, { mark: a.promo, color: '#f1b93b' }), bar('News', o.news, { mark: a.news }),
  ];
}

function segView(sg) {
  const out = [crumbs(areaLink(sg.area), sg.title), h('h2', {}, sg.title), h('div', { class: 'sub' }, `${sg.n_posts.toLocaleString()} posts from ${sg.n_authors.toLocaleString()} accounts`)];
  out.push(h('p', {}, sg.description));
  out.push(h('div', { class: 'chips' }, h('span', { class: 'chip' }, words(sg.kind)),
    h('span', { class: 'chip', title: 'How well the labelling model found one subject in the segment' }, 'coherence ' + sg.coherence + '/5'),
    ...sg.flags.map((f) => h('span', { class: 'chip spammy' }, f))));
  if (sg.common_thread) out.push(h('div', { class: 'note' }, 'Common thread: ' + sg.common_thread));
  if (sg.disagreement && sg.disagreement.length) out.push(...section('Opposing sides the labelling model saw', h('div', { class: 'chips' }, sg.disagreement.map((d) => h('span', { class: 'chip' }, d)))));
  out.push(...section('How it writes (existing classifier)', h('div', { class: 'note' }, 'The white tick is the average over all posts on the map. These scores were not used to build the segments.'), ...toneBars(sg)));
  out.push(...section('Who posts it',
    bar('Largest account\'s share', sg.top_author_share, { title: 'Share of the segment written by its most prolific account' }),
    bar('Repeated text', sg.repeat_share, { title: 'Share of posts whose exact text appears more than once' }),
    bar('Templated', sg.template_share, { title: 'Share of posts that follow one template with a few words changed' })));
  if (sg.classifier_top3) out.push(...section('The existing classifier calls them', h('div', { class: 'note' }, words(sg.classifier_top3))));

  const ex = h('div', {}), tags = {};
  for (const [i, g] of sg.examples) tags[i] = g === 'near' ? 'typical' : 'random';
  out.push(...section('Example posts', h('div', { class: 'note' }, 'Typical: closest to the middle of the segment. Random: drawn at random, to show the spread.'), ex));
  loadInto(ex, sg.examples.map((e) => e[0]), tags);

  const found = h('div', {});
  const input = h('input', { type: 'search', placeholder: 'Search the posts in this segment', 'aria-label': 'Search this segment' });
  const go = async () => {
    const q = input.value.trim();
    if (q.length < 3) { found.replaceChildren(h('div', { class: 'note' }, 'Type at least 3 characters.')); return; }
    try {
      const r = await getJSON(`/api/v1/search?q=${encodeURIComponent(q)}&segment=${sg.id}&limit=30`);
      for (const p of r.posts) S.postCache.set(p.i, p);
      S.hits = r.posts.map((p) => p.i); requestDraw();
      found.replaceChildren(...(r.posts.length ? r.posts.map((p) => postCard(p, { noSeg: true })) : [h('div', { class: 'note' }, 'No posts match.')]));
    } catch (e) { found.replaceChildren(h('div', { class: 'note' }, 'Search failed: ' + e.message)); }
  };
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
  out.push(...section('Search inside', h('div', { class: 'mini' }, input, h('button', { onclick: go }, 'Search')), found));

  if (sg.neighbours.length) out.push(...section('Near it in meaning', h('div', { class: 'note' }, 'Segments whose posts have the most neighbours in this one, with the strength of the link.'),
    h('div', { class: 'list' }, sg.neighbours.map(([id, w]) => h('div', { class: 'item' }, segLink(id), h('span', { class: 'num' }, `${S.areas[S.segs[id].area].title} · ${w.toFixed(2)}`))))));
  return out;
}

function areaView(a) {
  const segs = S.segs.filter((s) => s.area === a.id).sort((x, y) => y.n_texts - x.n_texts);
  const out = [crumbs(a.title), h('h2', {}, a.title), h('div', { class: 'sub' }, `${a.posts.toLocaleString()} posts in ${a.segments} segments · coherence ${a.coherence}/5`), h('p', {}, a.description)];
  const kinds = new Map();
  for (const s of segs) kinds.set(s.kind, (kinds.get(s.kind) || 0) + s.n_posts);
  out.push(...section('What kind of posts', h('div', { class: 'chips' }, [...kinds].sort((x, y) => y[1] - x[1]).map(([k, n]) => h('span', { class: 'chip' }, `${words(k)} ${pct(n / a.posts)}`)))));
  out.push(...section('Segments', h('div', { class: 'list' }, segs.slice(0, 80).map((s) => h('div', { class: 'item' }, segLink(s.id), h('span', { class: 'num' }, fmtN(s.n_posts))))),
    segs.length > 80 ? h('div', { class: 'note' }, `and ${segs.length - 80} smaller ones`) : null));
  return out;
}

function postView(i) {
  const rec = S.postCache.get(i), sg = S.segs[S.seg[i]], out = [crumbs(areaLink(sg.area), segLink(sg.id), 'post')];
  if (!rec) return [...out, h('div', { class: 'note' }, 'Loading…')];
  const link = bskyUrl(rec.u);
  out.push(h('div', { class: 'postbig' }, rec.t));
  out.push(h('div', { class: 'sub' }, `${when(rec.at)}${rec.n > 1 ? ` · the same text ${rec.n} times from ${rec.a} accounts` : ''}`));
  if (link) out.push(h('div', { style: { margin: '8px 0' } }, h('a', { class: 'link', href: link, target: '_blank', rel: 'noopener noreferrer' }, 'Open on Bluesky ↗')));
  out.push(...section('Where it sits', h('div', { class: 'list' },
    h('div', { class: 'item' }, h('span', { class: 'who' }, 'Segment'), segLink(sg.id)),
    h('div', { class: 'item' }, h('span', { class: 'who' }, 'Area'), areaLink(sg.area)),
    h('div', { class: 'item' }, h('span', { class: 'who' }, 'Existing classifier\'s subtopic'), h('span', { class: 'num' }, words(rec.p))))));
  out.push(...section('Its tone (existing classifier)', bar('Outraged', S.outr[i] / 255, { mark: S.avg.outraged, color: '#ef5b5b' }),
    bar('Critical', S.crit[i] / 255, { mark: S.avg.critical, color: '#ef5b5b' }), bar('Sentiment', S.sent[i] / 255, { mark: S.avg.sentiment, color: '#6ea8ff' })));
  const near = nearPosts(i, 6).map((j) => S.postCache.get(j)).filter(Boolean);
  if (near.length) out.push(...section('Next to it on the map', ...near.map((p) => postCard(p))));
  return out;
}

function searchView() {
  const out = [crumbs(`search “${S.hitQuery}”`), h('h2', {}, `“${S.hitQuery}”`), h('div', { class: 'sub' }, `${S.hits.length}${S.hits.length >= 100 ? '+' : ''} posts, ringed on the map`)];
  const bySeg = new Map();
  for (const i of S.hits) bySeg.set(S.seg[i], (bySeg.get(S.seg[i]) || 0) + 1);
  if (bySeg.size > 1) out.push(...section('In segments', h('div', { class: 'list' }, [...bySeg].sort((a, b) => b[1] - a[1]).slice(0, 12).map(([id, n]) => h('div', { class: 'item' }, segLink(id), h('span', { class: 'num' }, n))))));
  out.push(...section('Posts', ...S.hits.slice(0, 40).map((i) => S.postCache.get(i)).filter(Boolean).map((p) => postCard(p))));
  return out;
}

// ---------- search, filters, modes ----------

let searchSeq = 0;
async function runSearch(q) {
  q = q.trim();
  if (q.length < 3) return;
  try {
    const r = await getJSON(`/api/v1/search?q=${encodeURIComponent(q)}&limit=100`);
    for (const p of r.posts) S.postCache.set(p.i, p);
    S.hits = r.posts.map((p) => p.i); S.hitQuery = q; S.sel = { type: 'search' };
    setPalette(); showDrawer(); highlightOutline(); setHash('q/' + encodeURIComponent(q));
    if (S.hits.length) {
      let x0 = 1e9, y0 = 1e9, x1 = -1e9, y1 = -1e9;
      for (const i of S.hits) { const x = S.xy[2 * i], y = S.xy[2 * i + 1]; x0 = Math.min(x0, x); x1 = Math.max(x1, x); y0 = Math.min(y0, y); y1 = Math.max(y1, y); }
      const span = Math.max(x1 - x0, y1 - y0, 0.02);
      flyTo((x0 + x1) / 2, (y0 + y1) / 2, Math.min(S.W, S.H) / (span * 1.4));
    }
  } catch (e) { console.error(e); }
}

function initSearch() {
  const input = $('#search'), box = $('#results'); let items = [], sel = 0;
  const render = () => {
    box.replaceChildren(...items.map((it, k) => h('div', { class: 'row' + (k === sel ? ' sel' : ''), onclick: () => pick(it) }, h('span', { class: 'kind' }, it.kind), h('span', {}, it.text))));
    box.hidden = !items.length;
  };
  const pick = (it) => { box.hidden = true; input.blur(); it.go(); };
  input.addEventListener('input', async () => {
    const q = input.value.trim().toLowerCase(), seq = ++searchSeq;
    if (q.length < 2) { items = []; render(); return; }
    const a = S.areas.filter((x) => x.title.toLowerCase().includes(q)).sort((x, y) => y.posts - x.posts).slice(0, 3).map((x) => ({ kind: 'area', text: `${x.title} · ${fmtN(x.posts)}`, go: () => selectArea(x.id) }));
    const s = S.segs.filter((x) => x.title.toLowerCase().includes(q)).sort((x, y) => y.n_texts - x.n_texts).slice(0, 6).map((x) => ({ kind: 'segment', text: `${x.title} · ${fmtN(x.n_posts)}`, go: () => selectSeg(x.id) }));
    items = [...a, ...s]; sel = 0; render();
    if (q.length < 3) return;
    items.push({ kind: 'posts', text: `Find posts containing “${input.value.trim()}”`, go: () => runSearch(input.value) });
    render();
    try {
      const r = await getJSON('/api/v1/search?q=' + encodeURIComponent(q) + '&limit=5');
      if (seq !== searchSeq) return;
      for (const p of r.posts) S.postCache.set(p.i, p);
      items = items.concat(r.posts.map((p) => ({ kind: 'post', text: p.t.length > 90 ? p.t.slice(0, 90) + '…' : p.t, go: () => selectPost(p.i, true) })));
      render();
    } catch { /* keep the topic matches */ }
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { sel = Math.min(sel + 1, items.length - 1); render(); e.preventDefault(); }
    else if (e.key === 'ArrowUp') { sel = Math.max(sel - 1, 0); render(); e.preventDefault(); }
    else if (e.key === 'Enter') { if (e.shiftKey || !items[sel]) { box.hidden = true; runSearch(input.value); } else pick(items[sel]); }
    else if (e.key === 'Escape') { box.hidden = true; input.blur(); }
  });
  document.addEventListener('click', (e) => {
    if (!e.target.closest('#search-wrap')) box.hidden = true;
    if (!e.target.closest('#filter-wrap')) { $('#filters').hidden = true; $('#filter-btn').setAttribute('aria-expanded', 'false'); }
  });
  document.addEventListener('keydown', (e) => {
    const typing = ['INPUT', 'SELECT', 'TEXTAREA'].includes(document.activeElement.tagName);
    if (e.key === '/' && !typing) { e.preventDefault(); input.focus(); }
    else if (e.key === 'Escape' && !typing) clearSelection();
  });
}

function initFilters() {
  $('#filter-btn').addEventListener('click', () => { const f = $('#filters'); f.hidden = !f.hidden; $('#filter-btn').setAttribute('aria-expanded', String(!f.hidden)); });
  const apply = () => {
    S.filters = { bots: $('#f-bots').checked, adult: $('#f-adult').checked, sides: $('#f-sides').checked, coh: +$('#f-coh').value };
    if (S.n) { setPalette(); refreshOutlineFilters(); }
  };
  for (const id of ['#f-bots', '#f-adult', '#f-sides', '#f-coh']) $(id).addEventListener('change', apply);
}

const RAMP = 'linear-gradient(90deg,#336bd9,#eddb61,#eb3838)';
const LEGENDS = [
  () => 'Each colour is an area, its shades the segments in it. Posts that say alike sit close together; how close one area is to another means little beyond that.',
  () => h('span', {}, 'coherence 1', h('span', { class: 'ramp', style: { background: 'linear-gradient(90deg,#e14646,#eb8c3c,#ebd750,#6ec86e,#3ca0eb)' } }), '5 · how well the labelling model found one subject in the segment'),
  () => h('span', {}, KINDS.map((k) => h('b', { style: { color: `rgb(${KIND_RGB[k]})`, marginRight: '8px' } }, '● ' + words(k)))),
  () => h('span', {}, 'less', h('span', { class: 'ramp', style: { background: RAMP } }), 'more critical · the existing classifier\'s score for each post'),
  () => h('span', {}, 'positive', h('span', { class: 'ramp', style: { background: RAMP } }), 'negative · the existing classifier\'s sentiment for each post'),
  () => h('span', {}, 'calm', h('span', { class: 'ramp', style: { background: RAMP } }), 'outraged · the existing classifier\'s tone for each post'),
];
function setMode(m) {
  S.mode = m;
  document.querySelectorAll('#modes button').forEach((b) => b.classList.toggle('on', +b.dataset.mode === m));
  $('#legend').replaceChildren(LEGENDS[m]());
  if (S.n) setPalette(); else requestDraw();
}

document.querySelectorAll('#modes button').forEach((b) => b.addEventListener('click', () => setMode(+b.dataset.mode)));
window.addEventListener('resize', resize);
window.addEventListener('hashchange', route);
// A handle for the headless checks in tools/scenarios/postmap.mjs.
window.__map = { S, draw, selectArea, selectSeg, selectPost, runSearch, setMode, nearest, toScreen, toWorld, flyTo, get labelRects() { return labelRects; }, get gl() { return gl; } };
initPointer(); initSearch(); initFilters(); setMode(0);
boot().catch((e) => { $('#loading').textContent = 'Could not load the map: ' + e.message; console.error(e); });
