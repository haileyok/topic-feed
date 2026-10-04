// Tests of publish.js: publishing feeds from the browser, with a stand-in for the OAuth client and for the
// account's own server.
//
//   node --test internal/feedgen/webtest/publish.test.mjs
//
// The Go test TestPublishPageScript runs these.

import path from "node:path";
import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const { createPublisher, statusOf, recordFor, PublishError, COLLECTION } = await import(pathToFileURL(path.join(here, "..", "web", "static", "publish.js")).href);

const DID = "did:plc:bob";
const SERVICE = "did:web:feeds.example.com";
const SCOPE = "atproto repo:app.bsky.feed.generator";
const spec = (over = {}) => ({ display_name: "Cats", description: "All the cats.", accepts_interactions: true, ...over });

// ---------- an account's server ----------

class FakeServer {
  constructor() {
    this.records = new Map(); // rkey -> {cid, value}
    this.calls = [];
    this.n = 0;
    this.pageSize = 100;
    this.fail = null; // {nsid, status, body} the next matching call answers with
    this.network = false;
  }
  put(rkey, value) {
    const cid = `cid${++this.n}`;
    this.records.set(rkey, { cid, value });
    return cid;
  }
  async fetchHandler(pathname, init = {}) {
    this.calls.push({ pathname, method: init.method ?? "GET", body: init.body === undefined ? undefined : JSON.parse(init.body), headers: init.headers });
    if (this.network) throw new TypeError("Failed to fetch");
    const url = new URL(pathname, "https://pds.example.test");
    const nsid = url.pathname.replace("/xrpc/", "");
    const q = url.searchParams;
    const reply = (status, body) => ({ ok: status >= 200 && status < 300, status, json: async () => { if (body === undefined) throw new SyntaxError("x"); return body; } });
    if (this.fail && this.fail.nsid === nsid) {
      const f = this.fail;
      this.fail = null;
      return reply(f.status, f.body);
    }
    const body = init.body === undefined ? null : JSON.parse(init.body);
    switch (nsid) {
      case "com.atproto.repo.listRecords": {
        assert.equal(q.get("repo"), DID);
        assert.equal(q.get("collection"), COLLECTION);
        const all = [...this.records].map(([rkey, r]) => ({ uri: `at://${DID}/${COLLECTION}/${rkey}`, cid: r.cid, value: r.value }));
        const from = q.get("cursor") ? Number(q.get("cursor")) : 0;
        const page = all.slice(from, from + this.pageSize);
        return reply(200, { records: page, ...(from + this.pageSize < all.length ? { cursor: String(from + this.pageSize) } : {}) });
      }
      case "com.atproto.repo.getRecord": {
        const r = this.records.get(q.get("rkey"));
        if (!r) return reply(400, { error: "RecordNotFound", message: "Could not locate record" });
        return reply(200, { uri: `at://${DID}/${COLLECTION}/${q.get("rkey")}`, cid: r.cid, value: r.value });
      }
      case "com.atproto.repo.putRecord": {
        if (body.repo !== DID || body.collection !== COLLECTION) return reply(400, { error: "InvalidRequest", message: "wrong repo" });
        const have = this.records.get(body.rkey);
        if (body.swapRecord && (!have || have.cid !== body.swapRecord)) return reply(400, { error: "InvalidSwap", message: "Record was at another cid" });
        const cid = this.put(body.rkey, body.record);
        return reply(200, { uri: `at://${DID}/${COLLECTION}/${body.rkey}`, cid });
      }
      case "com.atproto.repo.deleteRecord":
        this.records.delete(body.rkey);
        return reply(200, {});
      default:
        return reply(404, { error: "MethodNotImplemented" });
    }
  }
}

// ---------- the OAuth client ----------

function fakeClientFor(server, { sessionDid = DID, scope = SCOPE, restored = true, state, loadError, initError, signInError, tokenInfoError } = {}) {
  const log = { loads: [], signIns: [], signOuts: 0, inits: 0 };
  const session = {
    sub: sessionDid, did: sessionDid,
    fetchHandler: (p, i) => server.fetchHandler(p, i),
    getTokenInfo: async () => { if (tokenInfoError) throw new Error("nope"); return { scope }; },
    signOut: async () => { log.signOuts++; },
  };
  class FakeClient {
    static async load(opts) {
      log.loads.push(opts);
      if (loadError) throw loadError;
      return new FakeClient();
    }
    async init() {
      log.inits++;
      if (initError) throw initError;
      return restored ? { session, ...(state !== undefined ? { state } : {}) } : undefined;
    }
    async signIn(handle, opts) {
      log.signIns.push({ handle, opts });
      if (signInError) throw signInError;
    }
  }
  return { ClientClass: FakeClient, log, session };
}

const publisherFor = (server, over = {}) => {
  const f = fakeClientFor(server, over);
  const p = createPublisher({ ClientClass: f.ClientClass, clientId: "https://feeds.example.com/oauth/browser-client-metadata.json", handleResolver: "https://feeds.example.com", did: DID, serviceDid: SERVICE, scope: SCOPE });
  return { p, ...f };
};

const connected = async (server, over) => {
  const r = publisherFor(server, over);
  const init = await r.p.init();
  assert.equal(init.state, "connected");
  return r;
};

// ---------- what a feed's record says ----------

test("where a feed stands against its record", () => {
  const rec = (over = {}) => ({ did: SERVICE, displayName: "Cats", description: "All the cats.", acceptsInteractions: true, ...over });
  assert.equal(statusOf(spec(), null, SERVICE), "draft");
  assert.equal(statusOf(spec(), undefined, SERVICE), "draft");
  assert.equal(statusOf(spec(), rec(), SERVICE), "published");
  assert.equal(statusOf(spec(), rec({ displayName: "Cats!" }), SERVICE), "changed");
  assert.equal(statusOf(spec(), rec({ description: "Others." }), SERVICE), "changed");
  assert.equal(statusOf(spec(), rec({ acceptsInteractions: undefined }), SERVICE), "changed");
  assert.equal(statusOf(spec({ accepts_interactions: false }), rec({ acceptsInteractions: undefined }), SERVICE), "published");
  assert.equal(statusOf(spec({ description: "" }), rec({ description: undefined }), SERVICE), "published", "no description is no description");
  assert.equal(statusOf(spec(), rec({ did: "did:web:elsewhere.example" }), SERVICE), "conflict");
  assert.equal(statusOf(spec(), {}, SERVICE), "conflict", "a record that names no service is not ours");
});

test("the record written is the existing one with what the feed says set in it", () => {
  const now = new Date("2026-10-04T12:00:00Z");
  const fresh = recordFor(spec(), null, SERVICE, now);
  assert.deepEqual(fresh, { $type: COLLECTION, did: SERVICE, displayName: "Cats", description: "All the cats.", acceptsInteractions: true, createdAt: "2026-10-04T12:00:00.000Z" });
  const old = {
    $type: COLLECTION, did: "did:web:old.example", displayName: "Old", description: "All the cats.", createdAt: "2025-01-01T00:00:00.000Z",
    avatar: { $type: "blob", ref: { $link: "bafy" }, mimeType: "image/png", size: 1 }, labels: { values: [] }, contentMode: "app.bsky.feed.defs#contentModeVideo",
    descriptionFacets: [{ index: { byteStart: 0, byteEnd: 3 }, features: [] }],
  };
  const same = recordFor(spec(), old, SERVICE, now);
  assert.equal(same.createdAt, "2025-01-01T00:00:00.000Z", "it keeps when it began");
  assert.deepEqual([same.avatar, same.labels, same.contentMode], [old.avatar, old.labels, old.contentMode], "what was added in the app stays");
  assert.deepEqual([same.did, same.displayName], [SERVICE, "Cats"]);
  assert.ok(same.descriptionFacets, "the same description keeps its formatting");
  assert.ok(!("descriptionFacets" in recordFor(spec({ description: "New words." }), old, SERVICE, now)), "a new description doesn't");
  const bare = recordFor(spec({ description: "", accepts_interactions: false }), { ...old, acceptsInteractions: true }, SERVICE, now);
  assert.ok(!("description" in bare) && !("acceptsInteractions" in bare), "what is switched off is taken out");
  assert.deepEqual(old.did, "did:web:old.example", "the existing record is not changed");
  assert.equal(recordFor(spec(), { createdAt: "" }, SERVICE, now).createdAt, "2026-10-04T12:00:00.000Z");
});

// ---------- connecting ----------

test("starting the client gives it where to find the metadata and handles, and says how things stand", async () => {
  const server = new FakeServer();
  const r = publisherFor(server);
  assert.deepEqual(await r.p.init(), { state: "connected", returned: false, did: DID });
  assert.deepEqual(r.log.loads, [{ clientId: "https://feeds.example.com/oauth/browser-client-metadata.json", handleResolver: "https://feeds.example.com" }]);
  assert.equal(r.p.connected, true);
  await assert.rejects(() => r.p.init(), /once per page load/);
  assert.equal(r.log.inits, 1, "the client's init is for once");

  const back = publisherFor(new FakeServer(), { state: "feeds" });
  assert.equal((await back.p.init()).returned, true, "back from Bluesky after allowing it");

  const none = publisherFor(new FakeServer(), { restored: false });
  assert.deepEqual(await none.p.init(), { state: "disconnected", returned: false });
  assert.equal(none.p.connected, false);
});

test("an account that is not the one signed in here is not connected, and is let go", async () => {
  const r = publisherFor(new FakeServer(), { sessionDid: "did:plc:someone-else" });
  assert.deepEqual(await r.p.init(), { state: "wrong_account", returned: false, did: "did:plc:someone-else" });
  assert.equal(r.p.connected, false);
  assert.equal(r.log.signOuts, 1);
  await assert.rejects(() => r.p.records(), (e) => e instanceof PublishError && e.code === "not_connected");
});

test("a connection that was not allowed to manage feeds is not connected either", async () => {
  const r = publisherFor(new FakeServer(), { scope: "atproto" });
  assert.equal((await r.p.init()).state, "declined");
  assert.equal(r.p.connected, false);
  assert.equal(r.log.signOuts, 1);
  const unknown = publisherFor(new FakeServer(), { tokenInfoError: true });
  assert.equal((await unknown.p.init()).state, "connected", "when the client can't say what was allowed, the server will");
  const more = publisherFor(new FakeServer(), { scope: `${SCOPE} blob:image/*` });
  assert.equal((await more.p.init()).state, "connected");
});

test("the client failing to start is said in words", async () => {
  for (const [opts, want] of [
    [{ initError: new Error('Unsupported scope "repo:app.bsky.feed.generator"') }, /doesn't support permissions that narrow/],
    [{ initError: new Error("invalid_client_metadata") }, /doesn't support permissions that narrow/],
    [{ loadError: new Error("network down") }, /Couldn't connect to Bluesky just now/],
  ]) {
    await assert.rejects(() => publisherFor(new FakeServer(), opts).p.init(), (e) => e instanceof PublishError && want.test(e.message));
  }
});

test("connecting sends the person to Bluesky for the permission and nothing else", async () => {
  const r = publisherFor(new FakeServer(), { restored: false });
  await r.p.init();
  await r.p.connect("  @bob.example.com ");
  assert.deepEqual(r.log.signIns, [{ handle: "bob.example.com", opts: { scope: SCOPE, state: "feeds" } }]);
  assert.doesNotMatch(r.log.signIns[0].opts.scope, /transition|blob:/);
  for (const bad of ["", "   ", "@", undefined]) await assert.rejects(() => r.p.connect(bad), (e) => e.code === "no_handle");
  for (const [err, want] of [
    [new Error("access_denied"), /You didn't allow it/],
    [new Error("Unsupported scope"), /doesn't support permissions that narrow/],
    [new Error("Failed to resolve identity: handle not found"), /Couldn't find that account/],
    [new Error("boom"), /Couldn't connect to Bluesky just now/],
  ]) {
    const x = publisherFor(new FakeServer(), { restored: false, signInError: err });
    await x.p.init();
    await assert.rejects(() => x.p.connect("bob.example.com"), (e) => e instanceof PublishError && want.test(e.message), String(err));
  }
  const early = createPublisher({ ClientClass: fakeClientFor(new FakeServer()).ClientClass, clientId: "x", handleResolver: "y", did: DID, serviceDid: SERVICE, scope: SCOPE });
  await assert.rejects(() => early.connect("bob.example.com"), (e) => e.code === "not_ready");
});

test("disconnecting forgets the account", async () => {
  const server = new FakeServer();
  const r = await connected(server);
  await r.p.disconnect();
  assert.equal(r.p.connected, false);
  assert.equal(r.log.signOuts, 1);
  await assert.rejects(() => r.p.publish("cats", spec()), (e) => e.code === "not_connected");
  await r.p.disconnect(); // twice is fine
  assert.equal(r.log.signOuts, 1);
});

// ---------- reading ----------

test("the account's feed records are listed by key, across pages", async () => {
  const server = new FakeServer();
  server.pageSize = 2;
  for (const k of ["a", "b", "c", "d", "e"]) server.put(k, { did: SERVICE, displayName: k });
  const { p } = await connected(server);
  const recs = await p.records();
  assert.deepEqual(Object.keys(recs).sort(), ["a", "b", "c", "d", "e"]);
  assert.equal(recs.c.value.displayName, "c");
  assert.match(recs.c.uri, /\/app\.bsky\.feed\.generator\/c$/);
  assert.equal(server.calls.filter((c) => c.pathname.includes("listRecords")).length, 3);
  assert.ok(server.calls.every((c) => c.pathname.startsWith("/xrpc/")), "calls go to the account's own server");
});

test("one feed's record, or nothing when there is none", async () => {
  const server = new FakeServer();
  server.put("cats", { did: SERVICE, displayName: "Cats" });
  const { p } = await connected(server);
  assert.equal((await p.record("cats")).value.displayName, "Cats");
  assert.equal(await p.record("dogs"), null);
  server.fail = { nsid: "com.atproto.repo.getRecord", status: 500, body: { error: "InternalServerError" } };
  await assert.rejects(() => p.record("cats"), (e) => e instanceof PublishError && /Couldn't read the feed just now/.test(e.message));
});

// ---------- publishing ----------

test("a feed that is not published yet is made", async () => {
  const server = new FakeServer();
  const { p } = await connected(server);
  const out = await p.publish("cats", spec());
  const put = server.calls.find((c) => c.pathname.endsWith("putRecord"));
  assert.equal(put.method, "POST");
  assert.deepEqual([put.body.repo, put.body.collection, put.body.rkey], [DID, COLLECTION, "cats"]);
  assert.ok(!("swapRecord" in put.body), "there is nothing to swap");
  assert.equal(put.body.record.did, SERVICE);
  assert.equal(put.body.record.displayName, "Cats");
  assert.match(put.body.record.createdAt, /^\d{4}-\d\d-\d\dT/);
  assert.equal(put.headers["Content-Type"], "application/json");
  assert.equal(server.records.get("cats").value.displayName, "Cats");
  assert.equal(out.value.displayName, "Cats");
  assert.equal(statusOf(spec(), out.value, SERVICE), "published");
});

test("a feed that is published is changed in place, keeping what the person added in the app", async () => {
  const server = new FakeServer();
  const cid = server.put("cats", { $type: COLLECTION, did: SERVICE, displayName: "Old name", createdAt: "2025-01-01T00:00:00.000Z", avatar: { $type: "blob", ref: { $link: "bafy" } } });
  const { p } = await connected(server);
  await p.publish("cats", spec());
  const put = server.calls.find((c) => c.pathname.endsWith("putRecord"));
  assert.equal(put.body.swapRecord, cid, "it replaces the record it just looked at, and no other");
  const now = server.records.get("cats").value;
  assert.deepEqual([now.displayName, now.createdAt, now.avatar.ref.$link], ["Cats", "2025-01-01T00:00:00.000Z", "bafy"]);
});

test("a record of another service is never written over", async () => {
  const server = new FakeServer();
  server.put("cats", { $type: COLLECTION, did: "did:web:elsewhere.example", displayName: "Not ours" });
  const { p } = await connected(server);
  await assert.rejects(() => p.publish("cats", spec()), (e) => e.code === "conflict" && /another service/.test(e.message));
  assert.equal(server.calls.some((c) => c.pathname.endsWith("putRecord")), false);
  await assert.rejects(() => p.unpublish("cats"), (e) => e.code === "conflict");
  assert.equal(server.calls.some((c) => c.pathname.endsWith("deleteRecord")), false);
  assert.equal(server.records.get("cats").value.displayName, "Not ours");
});

test("a change made somewhere else while publishing is not written over", async () => {
  const server = new FakeServer();
  server.put("cats", { $type: COLLECTION, did: SERVICE, displayName: "Old" });
  const { p } = await connected(server);
  server.fail = { nsid: "com.atproto.repo.putRecord", status: 400, body: { error: "InvalidSwap", message: "Record was at another cid" } };
  await assert.rejects(() => p.publish("cats", spec()), (e) => e.code === "InvalidSwap" && /changed somewhere else/.test(e.message));
});

test("everything that can go wrong with the account's server is said in words", async () => {
  const cases = [
    [{ status: 403, body: { error: "ScopeMissingError", message: "Missing required scope" } }, /didn't let this page change your feeds/],
    [{ status: 400, body: { error: "InvalidRequest", message: "Permission denied" } }, /didn't let this page change your feeds/],
    [{ status: 401, body: { error: "ExpiredToken" } }, /connection to Bluesky has ended/],
    [{ status: 401, body: {} }, /connection to Bluesky has ended/],
    [{ status: 429, body: { error: "RateLimitExceeded" } }, /slow down/],
    [{ status: 400, body: { error: "InvalidRequest", message: "Invalid displayName" } }, /Bluesky refused it: Invalid displayName/],
    [{ status: 502 }, /Couldn't publish the feed just now: Bluesky said 502/],
  ];
  for (const [fail, want] of cases) {
    const server = new FakeServer();
    const { p } = await connected(server);
    server.fail = { nsid: "com.atproto.repo.putRecord", ...fail };
    await assert.rejects(() => p.publish("cats", spec()), (e) => e instanceof PublishError && want.test(e.message), JSON.stringify(fail));
  }
  const server = new FakeServer();
  const { p } = await connected(server);
  server.network = true;
  await assert.rejects(() => p.publish("cats", spec()), (e) => e.code === "network" && /Couldn't reach Bluesky/.test(e.message));
  await assert.rejects(() => p.records(), (e) => e.code === "network");
});

// ---------- unpublishing ----------

test("unpublishing removes the record and nothing else", async () => {
  const server = new FakeServer();
  server.put("cats", { $type: COLLECTION, did: SERVICE, displayName: "Cats" });
  server.put("dogs", { $type: COLLECTION, did: SERVICE, displayName: "Dogs" });
  const { p } = await connected(server);
  await p.unpublish("cats");
  assert.deepEqual([...server.records.keys()], ["dogs"]);
  const del = server.calls.find((c) => c.pathname.endsWith("deleteRecord"));
  assert.deepEqual(del.body, { repo: DID, collection: COLLECTION, rkey: "cats" });
  await p.unpublish("cats"); // already gone
  assert.equal(server.calls.filter((c) => c.pathname.endsWith("deleteRecord")).length, 1, "nothing to delete the second time");
});
