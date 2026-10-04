// Stand-ins for what publishing talks to, for the tests of the page at /feeds: an account's own server
// (a PDS) with its feed records, and the OAuth client of the browser. (publish.test.mjs has its own
// copies of the same, from before the page's tests needed them.)

export const DID = "did:plc:bob";
export const SERVICE = "did:web:feeds.example.com";
export const SCOPE = "atproto repo:app.bsky.feed.generator";
export const COLLECTION = "app.bsky.feed.generator";

export class FakeServer {
  constructor() {
    this.records = new Map(); // rkey -> {cid, value}
    this.calls = [];
    this.n = 0;
    this.fail = null; // {nsid, status, body}: what the next call to that method answers
    this.onCall = null; // called with the nsid before each call is answered
  }
  put(rkey, value) {
    const cid = `cid${++this.n}`;
    this.records.set(rkey, { cid, value });
    return cid;
  }
  async fetchHandler(pathname, init = {}) {
    const url = new URL(pathname, "https://pds.example.test");
    const nsid = url.pathname.replace("/xrpc/", "");
    const body = init.body === undefined ? null : JSON.parse(init.body);
    this.calls.push({ nsid, method: init.method ?? "GET", body, query: Object.fromEntries(url.searchParams) });
    if (this.onCall) await this.onCall(nsid);
    const reply = (status, b) => ({ ok: status >= 200 && status < 300, status, json: async () => { if (b === undefined) throw new SyntaxError("x"); return b; } });
    if (this.fail && this.fail.nsid === nsid) {
      const f = this.fail;
      this.fail = null;
      return reply(f.status, f.body);
    }
    switch (nsid) {
      case "com.atproto.repo.listRecords":
        return reply(200, { records: [...this.records].map(([rkey, r]) => ({ uri: `at://${DID}/${COLLECTION}/${rkey}`, cid: r.cid, value: r.value })) });
      case "com.atproto.repo.getRecord": {
        const r = this.records.get(url.searchParams.get("rkey"));
        return r ? reply(200, { uri: `at://${DID}/${COLLECTION}/${url.searchParams.get("rkey")}`, cid: r.cid, value: r.value }) : reply(400, { error: "RecordNotFound" });
      }
      case "com.atproto.repo.putRecord": {
        const have = this.records.get(body.rkey);
        if (body.swapRecord && (!have || have.cid !== body.swapRecord)) return reply(400, { error: "InvalidSwap" });
        return reply(200, { uri: `at://${DID}/${COLLECTION}/${body.rkey}`, cid: this.put(body.rkey, body.record) });
      }
      case "com.atproto.repo.deleteRecord":
        this.records.delete(body.rkey);
        return reply(200, {});
      default:
        return reply(404, { error: "MethodNotImplemented" });
    }
  }
}

/** fakeClient is a stand-in for the OAuth client of the browser, signed in (or not) to server. */
export function fakeClient(server, { sessionDid = DID, scope = SCOPE, restored = true, state, loadError, initError, signInError } = {}) {
  const log = { loads: [], signIns: [], signOuts: 0 };
  const session = {
    sub: sessionDid, did: sessionDid, fetchHandler: (p, i) => server.fetchHandler(p, i),
    getTokenInfo: async () => ({ scope }), signOut: async () => { log.signOuts++; },
  };
  class FakeClient {
    static async load(opts) {
      log.loads.push(opts);
      if (loadError) throw loadError;
      return new FakeClient();
    }
    async init() {
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
