// Browser-level tests of the page at /me: signing in and out, and what it shows of your interests.
// (tuning.test.mjs tests the controls for tuning your feed.)
//
//   TOPICFEED_JSDOM=/path/to/node_modules/jsdom node --test internal/feedgen/webtest/
//
// The Go test TestMePageScript runs these when TOPICFEED_JSDOM is set.

import { test } from "node:test";
import assert from "node:assert/strict";
import { open, interestsBody, interest, signedIn, tuningBody } from "./harness.mjs";

const noticeText = (p) => (p.$("notice").hidden ? null : p.$("notice").textContent);

test("signed out: the sign-in form is shown, with no complaint", async () => {
  const p = await open();
  assert.deepEqual(p.visible(), ["signed-out"]);
  assert.equal(noticeText(p), null);
  assert.equal(p.$("login-button").textContent, "Sign in with Bluesky");
  assert.equal(p.$("login-button").disabled, false);
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
});

test("signing in: the handle is sent as JSON-asking form data, then the browser goes where it is told", async () => {
  const p = await open({ login: { status: 200, body: { redirect: "https://pds.example.test/oauth/authorize?request_uri=urn%3Aabc" } } });
  await p.submit("  Alice.bsky.social ");
  await p.until(() => p.assigned.length > 0, "the redirect");
  assert.deepEqual(p.assigned, ["https://pds.example.test/oauth/authorize?request_uri=urn%3Aabc"]);
  const call = p.calls.find((c) => c.url === "/oauth/login");
  assert.equal(call.method, "POST");
  assert.equal(call.headers.Accept, "application/json");
  assert.equal(call.headers["Content-Type"], "application/x-www-form-urlencoded");
  assert.equal(call.body, "handle=Alice.bsky.social");
  assert.equal(p.$("login-button").disabled, true, "no second click while the browser is leaving");
  assert.equal(p.$("login-button").textContent, "Opening Bluesky…");
  // Coming back with the back button restores the page as it was left: usable again.
  p.win.dispatchEvent(Object.assign(new p.win.Event("pageshow"), { persisted: true }));
  assert.equal(p.$("login-button").disabled, false);
  assert.equal(p.$("login-button").textContent, "Sign in with Bluesky");
});

test("every way a login can go wrong is said in words, and the button works again", async () => {
  const cases = [
    ["invalid", { status: 400, body: { error: "invalid" } }, /doesn't look like a Bluesky handle/],
    ["busy", { status: 503, body: { error: "busy" } }, /Lots of people/],
    ["limited", { status: 429, body: { error: "limited" } }, /tried a lot/],
    ["failed", { status: 502, body: { error: "failed" } }, /didn't work/],
    ["an error code we don't know", { status: 500, body: { error: "something_new" } }, /didn't work/],
    ["not JSON at all", { status: 500 }, /didn't work/],
    ["success without a redirect", { status: 200, body: {} }, /didn't work/],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/],
  ];
  for (const [name, login, want] of cases) {
    const p = await open({ login });
    await p.submit("alice.bsky.social");
    await p.until(() => noticeText(p) !== null, name);
    assert.match(noticeText(p), want, name);
    assert.equal(p.assigned.length, 0, `${name}: must not navigate`);
    assert.equal(p.$("login-button").disabled, false, `${name}: button`);
    assert.equal(p.$("login-button").textContent, "Sign in with Bluesky", `${name}: label`);
  }
});

test("trying again clears the complaint about the last try", async () => {
  let tries = 0;
  const p = await open({
    login: () => (++tries === 1 ? { status: 400, body: { error: "invalid" } } : { status: 200, body: { redirect: "https://pds.example.test/a" } }),
  });
  await p.submit("not a handle");
  await p.until(() => noticeText(p) !== null, "the first complaint");
  await p.submit("alice.bsky.social");
  await p.until(() => p.assigned.length > 0, "the redirect");
  assert.equal(noticeText(p), null, "the old complaint is still showing");
});

test("nothing typed: said at once, without asking the server", async () => {
  const p = await open({ login: { status: 200, body: { redirect: "https://x.test/" } } });
  for (const typed of ["", "   ", "\n\t"]) {
    await p.submit(typed);
    assert.match(noticeText(p), /doesn't look like a Bluesky handle/);
  }
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
  assert.equal(p.assigned.length, 0);
});

test("a redirect that isn't a web address is never followed", async () => {
  for (const redirect of ["javascript:alert(1)", "data:text/html,<script>alert(1)</script>", "not a url", "//evil.test/", "file:///etc/passwd"]) {
    const p = await open({ login: { status: 200, body: { redirect } } });
    await p.submit("alice.bsky.social");
    await p.until(() => noticeText(p) !== null, redirect);
    assert.equal(p.assigned.length, 0, redirect);
  }
});

test("a problem reported by the sign-in callback is shown once and cleared from the address", async () => {
  for (const [code, want] of [
    ["denied", /cancelled/],
    ["invalid", /doesn't look like/],
    ["busy", /Lots of people/],
    ["failed", /didn't work/],
    ["some-new-code", /didn't work/],
    // Names every object has must not find their way into the message table.
    ["constructor", /^Sign-in didn't work/],
    ["toString", /^Sign-in didn't work/],
    ["__proto__", /^Sign-in didn't work/],
    ["hasOwnProperty", /^Sign-in didn't work/],
  ]) {
    const p = await open({ search: `?signin=${code}` });
    assert.match(noticeText(p), want, code);
    assert.equal(p.win.location.search, "", `${code}: address cleaned`);
    assert.equal(p.win.location.pathname, "/me");
  }
  // Nothing the address says can add anything to the page but words.
  const hostile = "<img src=x onerror=alert(1)>";
  const p = await open({ search: `?signin=${encodeURIComponent(hostile)}` });
  assert.match(noticeText(p), /didn't work/);
  assert.equal(p.win.document.querySelectorAll("img[src='x']").length, 0);
  assert.equal(p.win.document.body.innerHTML.includes(hostile), false);
});

test("signed in: who you are, and a way out", async () => {
  const p = await open({ api: { status: 200, body: { did: "did:plc:alice", handle: "alice.bsky.social" } } });
  assert.deepEqual(p.visible(), ["signed-in"]);
  assert.equal(p.$("who").textContent, "@alice.bsky.social");
  // An account with no handle that checks out shows its DID.
  const q = await open({ api: { status: 200, body: { did: "did:plc:alice", handle: "" } } });
  assert.equal(q.$("who").textContent, "did:plc:alice");
});

test("a handle that is markup is shown as words", async () => {
  const hostile = "<img src=x onerror=alert(1)>";
  for (const body of [{ did: "did:plc:alice", handle: hostile }, { did: hostile, handle: "" }]) {
    const p = await open({ api: { status: 200, body } });
    assert.equal(p.$("who").children.length, 0);
    assert.equal(p.win.document.querySelectorAll("img[src='x']").length, 0);
    assert.ok(p.$("who").textContent.includes("<img"), "shown as text");
  }
});

// Signing out is the header's (webtest/header.test.mjs).

test("when the server can't be asked, the page says so and still offers sign-in", async () => {
  for (const api of [{ status: 500, body: {} }, { status: 502 }, new TypeError("Failed to fetch")]) {
    const p = await open({ api });
    assert.deepEqual(p.visible(), ["signed-out"]);
    assert.match(noticeText(p), /Couldn't reach the server/);
    assert.equal(p.$("login-button").disabled, false);
  }
});

test("when sign-in is switched off, the page says so and the form can't be used", async () => {
  const p = await open({ api: { status: 404 } });
  assert.deepEqual(p.visible(), ["signed-out"]);
  assert.match(noticeText(p), /isn't turned on/);
  assert.equal(p.$("login-button").disabled, true);
  assert.equal(p.$("handle").disabled, true);
});

test("the page makes no requests it shouldn't", async () => {
  const p = await open({ login: { status: 200, body: { redirect: "https://pds.example.test/a" } } });
  await p.submit("alice.bsky.social");
  await p.until(() => p.assigned.length > 0, "the redirect");
  for (const c of p.calls) assert.ok(c.url.startsWith("/") || c.url.startsWith("https://public.api.bsky.app/xrpc/"), `${c.url} is not on this site or Bluesky's public API`);
});

// ---------- the interests ----------

const shown = (p) => [["interests-loading"], ["interests-error"], ["interests-body"]].filter(([id]) => !p.$(id).hidden).map(([id]) => id.replace("interests-", ""));
const interestsLoaded = async (p) => p.until(() => !p.$("interests-body").hidden, "the interests to be shown");
const interestsFailed = async (p) => p.until(() => !p.$("interests-error").hidden, "the interests to fail");

test("interests: each with its share, a bar, and the liked posts behind it", async () => {
  const body = interestsBody({
    interests: [
      interest({
        posts: 12,
        samples: [
          { url: "https://bsky.app/profile/did:plc:x/post/abc", text: "a post that was liked", likedAt: "2026-09-30T10:00:00Z", p: 0.9 },
          { url: "", text: "a post with no link", likedAt: "2026-09-29T10:00:00Z", p: 0.8 },
        ],
      }),
      interest({ path: "animals_nature/cats", name: "Cats", broad: "Animals", share: 0.25, tunedShare: 0.25, posts: 0 }),
    ],
    otherPosts: 3,
    unclearPosts: 2,
  });
  const p = await open({ api: signedIn, interests: { status: 200, body } });
  await interestsLoaded(p);
  assert.deepEqual(shown(p), ["body"]);
  assert.equal(p.calls.find((c) => c.url === "/api/me/interests").headers.Accept, "application/json");

  const items = p.win.document.querySelectorAll("#interest-list > li");
  assert.equal(items.length, 2);
  const first = items[0];
  assert.equal(first.querySelector(".interest-name").textContent, "AI");
  assert.equal(first.querySelector(".interest-broad").textContent, "Technology");
  assert.equal(first.querySelector(".interest-share").textContent, "50% of your likes");
  assert.equal(first.querySelector(".bar-fill").style.width, "50%");
  assert.equal(first.querySelector(".interest-pct").textContent, "50%");
  // each broad topic has a colour of its own, set through the style object
  assert.match(first.style.getPropertyValue("--hue"), /^\d+$/);
  assert.notEqual(first.style.getPropertyValue("--hue"), items[1].style.getPropertyValue("--hue"), "Technology and Animals differ");
  assert.equal(items[1].querySelector(".interest-pct").textContent, "25%");
  assert.equal(first.querySelector("summary").textContent, "12 liked posts");
  const samples = first.querySelectorAll(".sample");
  assert.equal(samples.length, 2);
  const link = samples[0].querySelector("a");
  assert.equal(link.href, "https://bsky.app/profile/did:plc:x/post/abc");
  assert.equal(link.textContent, "a post that was liked");
  assert.equal(link.target, "_blank");
  assert.match(link.rel, /noopener/);
  assert.equal(samples[1].querySelector("a"), null);
  assert.match(samples[1].textContent, /a post with no link/);
  assert.match(first.querySelector(".sample-more").textContent, /and 10 more/);

  assert.equal(items[1].querySelector(".interest-share").textContent, "25% of your likes");
  assert.equal(items[1].querySelector("details"), null);
  assert.match(items[1].querySelector(".interest-none").textContent, /No liked posts/);

  assert.match(p.$("interests-summary").textContent, /You liked or reposted 40 posts in the time we have data for \(up to 30 days\)/);
  assert.match(p.$("interests-summary").textContent, /30 of them have topics/);
  assert.match(p.$("interests-summary").textContent, /half as much every 7 days/);
  assert.match(p.$("interests-other").textContent, /3 more liked posts are about topics outside your top 20/);
  assert.match(p.$("interests-other").textContent, /2 liked posts couldn't be placed/);
  assert.equal(p.$("interests-mix").hidden, true);

  // the same numbers as tiles
  assert.equal(p.$("me-stats").hidden, false);
  assert.equal(p.$("stat-liked").textContent, "40");
  assert.equal(p.$("stat-topics").textContent, "30");
  assert.equal(p.$("stat-interests").textContent, "2");
  assert.equal(p.$("stat-memory").textContent, "7");
});

test("page: on a narrow screen the interests, the settings and the preview are views you switch between", async () => {
  const p = await open({ api: signedIn, interests: { status: 200, body: interestsBody({ interests: [interest()] }) } });
  await interestsLoaded(p);
  const grid = p.$("me-grid");
  const tab = (view) => p.win.document.querySelector(`.me-tab[data-view="${view}"]`);
  const pressed = () => ["interests", "settings", "preview"].filter((v) => tab(v).getAttribute("aria-pressed") === "true");
  assert.equal(grid.dataset.view, "interests", "the interests are what shows first");
  assert.deepEqual(pressed(), ["interests"]);
  tab("settings").click();
  assert.equal(grid.dataset.view, "settings");
  assert.deepEqual(pressed(), ["settings"]);
  tab("preview").click();
  assert.equal(grid.dataset.view, "preview");
  assert.deepEqual(pressed(), ["preview"]);
  tab("interests").click();
  p.$("jump").click(); // "See the preview" in the save bar
  assert.equal(grid.dataset.view, "preview");
  assert.deepEqual(pressed(), ["preview"]);
});

test("interests: what other people posted is shown as words, and only real post addresses are links", async () => {
  const markup = "<img src=x onerror=alert(1)>";
  const body = interestsBody({
    interests: [
      interest({
        name: markup, broad: "<script>alert(1)</script>", posts: 7,
        samples: [
          { url: "javascript:alert(1)", text: "js " + markup },
          { url: "https://evil.test/profile/x/post/1", text: "another site" },
          { url: "https://bsky.app.evil.test/profile/x", text: "lookalike host" },
          { url: "https://bsky.app@evil.test/profile/x", text: "credentials trick" },
          { url: "http://bsky.app/profile/x/post/1", text: "not https" },
          { url: "//bsky.app/profile/x/post/1", text: "no scheme" },
          { url: "data:text/html,<script>alert(1)</script>", text: "data" },
          { url: "https://bsky.app/profile/ok/post/1", text: "the real one " + markup },
          { url: "https://evilbsky.app/profile/x/post/1", text: "a host that ends the same way" },
          { url: "https://user:pass@bsky.app/profile/x/post/1", text: "credentials on the real host" },
          { text: markup },
          { url: 12, text: 34 },
        ],
      }),
    ],
  });
  const p = await open({ api: signedIn, interests: { status: 200, body } });
  await interestsLoaded(p);
  const doc = p.win.document;
  assert.equal(doc.querySelectorAll("img[src='x']").length, 0, "markup became an element");
  assert.equal(doc.querySelectorAll("#interest-list script").length, 0);
  assert.equal(doc.querySelectorAll("[onerror]").length, 0, "markup became an attribute");
  assert.ok(doc.querySelector(".interest-name").textContent.includes("<img"), "shown as text");
  assert.ok(doc.querySelector(".interest input").getAttribute("aria-label").includes("<img"), "a topic's name in a label is still only text");
  const links = [...doc.querySelectorAll("#interest-list a")];
  assert.deepEqual(links.map((a) => a.href), ["https://bsky.app/profile/ok/post/1"], "only the genuine address is a link");
  for (const t of ["js ", "another site", "lookalike host", "credentials trick", "not https", "no scheme", "data",
    "a host that ends the same way", "credentials on the real host"]) {
    assert.ok([...doc.querySelectorAll(".sample")].some((s) => s.textContent.includes(t) && !s.querySelector("a")), `${t}: shown, but not as a link`);
  }
});

test("interests: what tuning did is shown, with a slider where it was left", async () => {
  const body = interestsBody({
    interests: [
      interest({ path: "technology/ai", name: "Muted", weight: 0, share: 0.5, tunedShare: 0 }),
      interest({ path: "animals_nature/cats", name: "Up", weight: 2, share: 0.2, tunedShare: 0.4 }),
      interest({ path: "sports/baseball", name: "Down", weight: 0.5, share: 0.2, tunedShare: 0.1 }),
      interest({ path: "food/baking", name: "Same", weight: 1, share: 0.1, tunedShare: 0.1 }),
      // Moved up because others were muted: it is in the feed, but nothing was said about it.
      interest({ path: "sports/soccer", name: "Moved up", added: true, share: 0, tunedShare: 0.1, weight: 1 }),
    ],
  });
  const saved = { topics: { "technology/ai": 0, "animals_nature/cats": 2, "sports/baseball": 0.5 } };
  const p = await open({ api: signedIn, interests: { status: 200, body }, tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  await interestsLoaded(p);
  await p.until(() => p.win.document.querySelectorAll("#interest-list input").length === 5, "the sliders");
  const items = [...p.win.document.querySelectorAll("#interest-list > li")];
  assert.deepEqual(
    items.map((li) => li.querySelector(".interest-share").textContent),
    ["50% of your likes → 0% of your feed", "20% of your likes → 40% of your feed", "20% of your likes → 10% of your feed", "10% of your likes", "10% of your feed"],
  );
  assert.deepEqual(items.map((li) => [...li.querySelectorAll(".badge")].map((b) => b.textContent)), [[], [], [], [], ["moved up"]]);
  assert.deepEqual(items.map((li) => li.querySelector("input").value), ["0", "4", "2", "3", "3"]);
  assert.deepEqual(items.map((li) => li.querySelector(".weight-value").textContent), ["Muted", "More (×2)", "Less (×½)", "As you like it", "As you like it"]);
  assert.deepEqual(items.map((li) => li.querySelector("input").classList.contains("neg")), [true, false, true, false, false]);
  assert.equal(items[1].querySelector(".bar-fill").style.width, "40%", "the bar is the share of the feed");
  assert.equal(p.$("savebar").hidden, true, "nothing has been changed");
  assert.deepEqual(items.map((li) => li.querySelectorAll("button").length), [0, 0, 0, 0, 0], "topics from their likes weren't added, so there is nothing to remove");
});

test("interests: odd numbers are kept in range, and missing parts don't break the page", async () => {
  const body = interestsBody({
    interests: [
      interest({ name: "big", share: 1.5, tunedShare: 1.5 }),
      interest({ name: "negative", share: -1, tunedShare: -1 }),
      interest({ name: "nan", share: "lots", tunedShare: "lots" }),
      interest({ name: "null", share: null, tunedShare: null }),
      { path: "only/a-path" },
    ],
  });
  const p = await open({ api: signedIn, interests: { status: 200, body } });
  await interestsLoaded(p);
  const widths = [...p.win.document.querySelectorAll(".bar-fill")].map((f) => f.style.width);
  assert.deepEqual(widths, ["100%", "0%", "0%", "0%", "0%"]);
  assert.equal(p.win.document.querySelectorAll(".interest-name")[4].textContent, "only/a-path");

  // An answer with nothing in it at all.
  const q = await open({ api: signedIn, interests: { status: 200, body: {} } });
  await interestsLoaded(q);
  assert.equal(q.win.document.querySelectorAll("#interest-list > li").length, 0);
  assert.match(q.$("interests-summary").textContent, /0 posts/);
  assert.equal(q.$("interests-other").hidden, true);
});

test("interests: when the feed is a mix, the page says why", async () => {
  const few = await open({ api: signedIn, interests: { status: 200, body: interestsBody({ personalized: false, state: "generic" }) } });
  await interestsLoaded(few);
  assert.equal(few.$("interests-mix").hidden, false);
  assert.match(few.$("interests-mix").textContent, /haven't liked enough yet \(at least 5 posts with topics\)/);
  assert.equal(few.$("stat-interests").textContent, "–", "no interests of your own yet: nothing to count");

  const muted = await open({ api: signedIn, interests: { status: 200, body: interestsBody({ personalized: true, state: "generic" }) } });
  await interestsLoaded(muted);
  assert.match(muted.$("interests-mix").textContent, /every topic you like is turned off/);

  const normal = await open({ api: signedIn, interests: { status: 200, body: interestsBody({ state: "personal" }) } });
  await interestsLoaded(normal);
  assert.equal(normal.$("interests-mix").hidden, true);
});

test("interests: reading them can be slow, and the page says it is working", async () => {
  let release;
  const slow = new Promise((resolve) => (release = resolve));
  const p = await open({ api: signedIn, interests: () => slow });
  assert.deepEqual(shown(p), ["loading"]);
  assert.match(p.$("interests-loading").textContent, /Reading your likes/);
  release({ status: 200, body: interestsBody({ interests: [interest()] }) });
  await interestsLoaded(p);
  assert.deepEqual(shown(p), ["body"]);
});

test("interests: every way reading them can fail is said, and trying again works", async () => {
  const cases = [
    ["rate limited", { status: 429, body: { error: "limited" } }, /asked a lot/],
    ["unavailable", { status: 503, body: { error: "unavailable" } }, /Couldn't read your likes/],
    ["a server error with no body", { status: 500 }, /Couldn't read your likes/],
    ["success that isn't JSON", { status: 200 }, /Couldn't read your likes/],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/],
  ];
  for (const [name, failure, want] of cases) {
    let n = 0;
    const p = await open({
      api: signedIn,
      interests: () => (++n === 1 ? failure : { status: 200, body: interestsBody({ interests: [interest(), interest({ name: "Cats" })] }) }),
    });
    await interestsFailed(p);
    assert.deepEqual(shown(p), ["error"], name);
    assert.match(p.$("interests-error-text").textContent, want, name);
    assert.deepEqual(p.visible(), ["signed-in"], `${name}: still signed in`);
    assert.equal(p.reloads.length, 0, name);

    p.$("interests-retry").click();
    await interestsLoaded(p);
    assert.deepEqual(shown(p), ["body"], `${name}: after trying again`);
    assert.equal(p.win.document.querySelectorAll("#interest-list > li").length, 2, `${name}: the list is replaced, not added to`);
  }
});

test("interests: trying again shows that it is working, then the result", async () => {
  let n = 0;
  let release;
  const slow = new Promise((resolve) => (release = resolve));
  const p = await open({ api: signedIn, interests: () => (++n === 1 ? { status: 503, body: { error: "unavailable" } } : slow) });
  await interestsFailed(p);
  p.$("interests-retry").click();
  await p.until(() => n === 2, "the second request");
  assert.deepEqual(shown(p), ["loading"], "while it is reading again, it says so and the old error is gone");
  release({ status: 200, body: interestsBody({ interests: [interest()] }) });
  await interestsLoaded(p);
  assert.deepEqual(shown(p), ["body"]);
});

test("interests: showing them again replaces what was shown", async () => {
  const two = interestsBody({ interests: [interest(), interest({ name: "Cats" })], otherPosts: 4 });
  const p = await open({ api: signedIn, interests: { status: 200, body: two } });
  await interestsLoaded(p);
  p.mod.renderInterests(two);
  p.mod.renderInterests(two);
  assert.equal(p.win.document.querySelectorAll("#interest-list > li").length, 2);
  // Including the note under the list, which goes when there is nothing to say.
  p.mod.renderInterests(interestsBody({ interests: [interest()], otherPosts: 0, unclearPosts: 0 }));
  assert.equal(p.win.document.querySelectorAll("#interest-list > li").length, 1);
  assert.equal(p.$("interests-other").hidden, true);
});

test("interests: a session that ended starts again from the sign-in form", async () => {
  const p = await open({ api: signedIn, interests: { status: 401, body: { error: "not signed in" } } });
  await p.until(() => p.reloads.length > 0, "the reload");
  assert.equal(p.$("interests-body").hidden, true);
});

test("interests: nothing is read for someone who isn't signed in", async () => {
  const p = await open();
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
});
