// Publishing feeds from the browser: the page signs in to the person's own Bluesky account with OAuth
// (the tokens never leave the browser), and writes, changes and removes the feed records in their own
// repo itself. The service never holds anything that could write to anyone's account.
//
// The OAuth client is given to createPublisher (it is the library bundled in vendor/), which is what
// lets the tests use a stand-in for it.

export const COLLECTION = "app.bsky.feed.generator";

/** PublishError is something that went wrong, with a message for the person who was publishing. */
export class PublishError extends Error {
  constructor(message, code = "") {
    super(message);
    this.name = "PublishError";
    this.code = code;
  }
}

const str = (v) => (typeof v === "string" ? v : "");

/**
 * statusOf is where a saved feed stands against its record in the person's repo (null: there is none):
 *   draft      not published
 *   published  the record says what the feed says
 *   changed    the record is ours but says something else: the feed has been changed since
 *   conflict   there is a record with this key that is not ours (it names another service)
 */
export function statusOf(spec, record, serviceDid) {
  if (!record) return "draft";
  if (str(record.did) !== serviceDid) return "conflict";
  const same = str(record.displayName) === str(spec.display_name)
    && str(record.description) === str(spec.description)
    && (record.acceptsInteractions === true) === (spec.accepts_interactions === true);
  return same ? "published" : "changed";
}

/**
 * recordFor is the record to write for a feed: the existing one with what the feed says set in it,
 * so what the person has added in the app (a picture, labels) is kept. When the description changes
 * its formatting (links, mentions) no longer fits it, so that goes.
 */
export function recordFor(spec, existing, serviceDid, now = new Date()) {
  const rec = existing ? { ...existing } : {};
  const changedDescription = str(rec.description) !== str(spec.description);
  rec.$type = COLLECTION;
  rec.did = serviceDid;
  rec.displayName = str(spec.display_name);
  if (str(spec.description)) rec.description = spec.description;
  else delete rec.description;
  if (changedDescription) delete rec.descriptionFacets;
  if (spec.accepts_interactions === true) rec.acceptsInteractions = true;
  else delete rec.acceptsInteractions;
  if (typeof rec.createdAt !== "string" || !rec.createdAt) rec.createdAt = now.toISOString();
  return rec;
}

// What the person is told when a request to their account's server fails.
function explain(status, data, doing) {
  const name = str(data && data.error);
  const text = str(data && data.message);
  if (/scope/i.test(name) || /scope/i.test(text) || /permission/i.test(text)) {
    return "Bluesky didn't let this page change your feeds. Disconnect and connect again, and allow it to manage your feed records.";
  }
  if (status === 401 || name === "ExpiredToken" || name === "InvalidToken" || name === "AuthMissing") {
    return "Your connection to Bluesky has ended. Connect again to go on.";
  }
  if (name === "InvalidSwap") return "That feed was changed somewhere else while you were looking. Reload the page and try again.";
  if (status === 429) return "Bluesky asked to slow down. Wait a moment and try again.";
  if (name === "InvalidRequest" && text) return `Bluesky refused it: ${text}`;
  return `Couldn't ${doing} just now: Bluesky said ${status || "nothing"}${text ? `: ${text}` : ""}. Try again in a moment.`;
}

// Signing in goes wrong in ways worth telling apart.
function explainSignIn(err) {
  const text = `${str(err && err.name)} ${str(err && err.message)}`;
  if (/Unsupported scope|invalid_scope|invalid_client_metadata/i.test(text)) {
    return "Your Bluesky server doesn't support permissions that narrow yet, so this page can't ask for just the right to manage feeds. Publish from the Bluesky app's feed settings for now, or move to a server that supports them.";
  }
  if (/access_denied|denied|cancel/i.test(text)) return "You didn't allow it, so nothing was connected.";
  if (/handle|resolve|identity|not found|invalid.*(did|handle)/i.test(text)) return "Couldn't find that account. Check the handle and try again.";
  return "Couldn't connect to Bluesky just now. Try again in a moment.";
}

/**
 * createPublisher makes what the page publishes with.
 *   ClientClass  the OAuth client (BrowserOAuthClient): ClientClass.load({clientId, handleResolver}) gives one
 *   clientId     the address of the client metadata the service serves
 *   handleResolver  where handles are looked up: this service, so Bluesky isn't told who signs in
 *   did          the account that is signed in to the service: the only one that may publish here
 *   serviceDid   what feed records point at
 *   scope        what to ask the account for
 */
export function createPublisher({ ClientClass, clientId, handleResolver, did, serviceDid, scope }) {
  let client = null;
  let session = null;
  let started = false;

  const requireSession = () => {
    if (!session) throw new PublishError("Connect your Bluesky account first.", "not_connected");
    return session;
  };

  async function xrpc(nsid, { method = "GET", query, body } = {}, doing = "do that") {
    const s = requireSession();
    const qs = query ? `?${new URLSearchParams(query)}` : "";
    let res;
    try {
      res = await s.fetchHandler(`/xrpc/${nsid}${qs}`, {
        method,
        headers: body === undefined ? { Accept: "application/json" } : { Accept: "application/json", "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch {
      throw new PublishError("Couldn't reach Bluesky. Check your connection and try again.", "network");
    }
    let data = null;
    try {
      data = await res.json();
    } catch {
      // not JSON: the status says enough
    }
    if (!res.ok) throw new PublishError(explain(res.status, data, doing), str(data && data.error) || String(res.status));
    return data;
  }

  const publisher = {
    /** connected is whether the account is connected, and it is the account that is signed in here. */
    get connected() {
      return session !== null;
    },

    /**
     * init starts the client, once per page load: it is what takes in the answer when Bluesky sends the
     * person back after they allowed (or refused) the connection. It says how that stands:
     * {state: "connected" | "disconnected" | "wrong_account", returned: bool, did}.
     */
    async init() {
      if (started) throw new Error("init is for once per page load");
      started = true;
      try {
        client = await ClientClass.load({ clientId, handleResolver });
        const result = await client.init();
        if (!result || !result.session) return { state: "disconnected", returned: false };
        const got = result.session.did || result.session.sub;
        if (got !== did) {
          // Connected as somebody else: not usable here, and not left connected.
          await publisher.disconnectSession(result.session);
          return { state: "wrong_account", returned: result.state != null, did: got };
        }
        session = result.session;
        const granted = await publisher.grantedScope();
        if (granted !== null && !granted.split(/\s+/).includes(`repo:${COLLECTION}`)) {
          await publisher.disconnectSession(result.session);
          session = null;
          return { state: "declined", returned: result.state != null, did: got };
        }
        return { state: "connected", returned: result.state != null, did: got };
      } catch (err) {
        throw new PublishError(explainSignIn(err), "init");
      }
    },

    /** grantedScope is what the account allowed, or null when the client doesn't say. */
    async grantedScope() {
      try {
        const info = await session.getTokenInfo();
        return info && typeof info.scope === "string" ? info.scope : null;
      } catch {
        return null;
      }
    },

    async disconnectSession(s) {
      try {
        await s.signOut();
      } catch {
        // the connection is dropped here whatever the account's server says
      }
    },

    /** connect sends the person to Bluesky to allow it; they come back to this page. */
    async connect(handle) {
      if (!client) throw new PublishError("Not ready yet. Reload the page.", "not_ready");
      const who = str(handle).trim().replace(/^@/, "");
      if (!who) throw new PublishError("Type your Bluesky handle first.", "no_handle");
      try {
        await client.signIn(who, { scope, state: "feeds" });
      } catch (err) {
        throw new PublishError(explainSignIn(err), "sign_in");
      }
    },

    /** disconnect ends the connection: the tokens are revoked and forgotten. */
    async disconnect() {
      if (!session) return;
      const s = session;
      session = null;
      await publisher.disconnectSession(s);
    },

    /** records are the account's feed records, by rkey: {rkey: {uri, cid, value}}. */
    async records() {
      const out = {};
      let cursor = "";
      for (let page = 0; page < 5; page++) {
        const data = await xrpc("com.atproto.repo.listRecords", {
          query: { repo: did, collection: COLLECTION, limit: "100", ...(cursor ? { cursor } : {}) },
        }, "read your feeds");
        for (const r of (data && data.records) || []) {
          const rkey = str(r.uri).split("/").pop();
          if (rkey) out[rkey] = { uri: r.uri, cid: r.cid, value: r.value };
        }
        cursor = str(data && data.cursor);
        if (!cursor) break;
      }
      return out;
    },

    /** record is one feed's record as it is now, or null. */
    async record(rkey) {
      try {
        const data = await xrpc("com.atproto.repo.getRecord", { query: { repo: did, collection: COLLECTION, rkey } }, "read the feed");
        return data && data.value ? { uri: data.uri, cid: data.cid, value: data.value } : null;
      } catch (err) {
        if (err instanceof PublishError && err.code === "RecordNotFound") return null;
        throw err;
      }
    },

    /**
     * publish writes the feed's record: it makes it, or changes the one there is (it is looked at just
     * now, so a change made elsewhere is not written over). It refuses a record that is not ours.
     */
    async publish(rkey, spec) {
      const existing = await publisher.record(rkey);
      if (existing && statusOf(spec, existing.value, serviceDid) === "conflict") {
        throw new PublishError("Your account already has a feed with this key that was made with another service. Choose another key for this feed.", "conflict");
      }
      const body = {
        repo: did, collection: COLLECTION, rkey, record: recordFor(spec, existing && existing.value, serviceDid),
        ...(existing ? { swapRecord: existing.cid } : {}),
      };
      const out = await xrpc("com.atproto.repo.putRecord", { method: "POST", body }, "publish the feed");
      return { uri: out && out.uri, cid: out && out.cid, value: body.record };
    },

    /** unpublish removes the feed's record (the feed stays saved here). A record that isn't there is fine. */
    async unpublish(rkey) {
      const existing = await publisher.record(rkey);
      if (!existing) return;
      if (statusOf({}, existing.value, serviceDid) === "conflict") {
        throw new PublishError("That record isn't from this service, so it is left alone.", "conflict");
      }
      await xrpc("com.atproto.repo.deleteRecord", { method: "POST", body: { repo: did, collection: COLLECTION, rkey } }, "unpublish the feed");
    },
  };
  return publisher;
}
