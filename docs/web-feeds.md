# Feeds made and published on the web

**Owner:** Hailey (`hailey.at`)
**Status:** Built and tested; not deployed yet. Requested 2026-10-04: keep the feeds' configuration in a
database instead of `config/feeds.yaml`, let signed-in people manage their own feeds, and publish and
update them from the browser.

## What a person does

1. Sign in at `/me` (the same sign-in as the For you page).
2. Build a feed at `/`, the feed builder: topics, how sure a post's topic must be, tone and quality
   scores, ranking, with a live preview. Signed in, the side panel has **Save as my feed**: a description,
   and a key (the feed's address on Bluesky; made from the name unless you type one). Saving keeps the
   feed as theirs and does not put it on Bluesky.
3. Open `/feeds`: every feed of theirs, with where it stands (**Not published**, **Published**,
   **Changed since you published it**), and buttons to **Publish** / **Update on Bluesky**,
   **Unpublish**, **Edit** (back in the builder, `/?edit=<key>`) and **Delete**.
4. The first time they publish, **Connect to publish** sends them to their own Bluesky server to allow one
   thing: managing their feed records. They come back to `/feeds` and publish.

A feed shows up in the Bluesky app within a minute or so of being published. Taking one off is best-effort:
Bluesky can keep showing a removed feed for a while.

## What is stored, and where

- **`user_feeds`** (`schema/014_user_feeds.sql`, ClickHouse): one row per version of a feed, the newest
  row per (owner, key) wins, a row with `deleted = 1` removes it. The feed is JSON (`feedgen.FeedSpec`):
  the same fields as a `config/feeds.yaml` entry (name, description, topics, `min_prob`, `exclude`, tone
  and signal rules, ranking, `max_posts`, `allow_adult`) minus the key.
- **The first time** feedgen starts with that table, the topic feeds of `config/feeds.yaml` are copied
  into it as the service owner's, in the file's order (`Store.SeedFeeds`). From then on the file's topic
  feeds are **not looked at**: editing one there changes nothing, and a feed taken down on the web is not
  brought back by a restart. The file still supplies the `personal:` For you feed and documents the
  fields.
- **`make feeds-publish`** now writes the owner's feeds from the database.
- **The record in Bluesky** (an `app.bsky.feed.generator` record in the person's own repo, pointing at this
  service's DID) is not stored here at all: the browser reads and writes it.

## Who may do what

| | anyone signed in | the service owner |
|---|---|---|
| feeds | 5 (`UserLimits.MaxFeeds`) | no limit |
| topics per feed / topics left out / candidates | 40 / 60 / 3000 | no limit |
| a feed that takes adult posts | no | yes (the builder's adult switch) |
| a personal (For you style) feed | no | only from the config file |

Every request names its account from the signed cookie and nothing else: no parameter, header or path
says whose feeds they are, so nobody can read or change anyone else's.

## How publishing works, and why it is in the browser

Writing a feed record needs permission to write to the person's repo. This service never holds that:

- The page at `/feeds` signs in to the person's account with **OAuth, in the browser**, using Bluesky's own
  client library (`web/static/vendor/atproto-oauth-client-browser.js`, a bundle of
  `@atproto/oauth-client-browser`; how it was built: `internal/feedgen/webvendor/README.md`). The tokens
  live in the browser's storage (the keys can't even be read out of it) and are never sent here.
- It asks for **one permission**: `atproto repo:app.bsky.feed.generator`, the right to create, change and
  delete feed records, and nothing else (never the broad `transition:*` permissions). The consent screen at
  the person's own server says so.
- Its OAuth client is described by `/oauth/browser-client-metadata.json` (client ID = that address, redirect
  = `/feeds`). It is a client of its own, apart from the one that signs people in (`/oauth/client-metadata.json`).
- A browser can't look up a handle's DNS record, so handle lookups go to this service
  (`/xrpc/com.atproto.identity.resolveHandle`): Bluesky is not told who signs in here.
- **Publishing** reads the feed's record as it is now, sets what the feed says in it (name, description,
  `acceptsInteractions`, this service's DID), keeps everything else (a picture added in the app, labels),
  and writes it back only if nobody changed it meanwhile (`swapRecord`). A record under the same key that
  points at another service is never written over.
- The page refuses a connection to an account other than the one signed in here.

**The `/feeds` page has a security policy of its own** (`feedsContentSecurityPolicy` in `web.go`): it may
connect to any `https` address, because a person's server (and the one that signs them in) can be anywhere.
That is a real loosening, and no other page has it. What keeps it small: scripts are only the site's own
(no inline script, no `eval`; a test checks the vendored bundle for both), every piece of text from a person
or an account goes on the page as text, and the page can't be framed or send a form anywhere.

Connections made this way last about two weeks at most (Bluesky's rule for browser apps); then the page
asks to connect again.

**Not built:** uploading a feed's picture (it would need a second permission). A picture set in the Bluesky
app is kept when the page updates the feed.

## How the service serves other people's feeds

- A feed's URI is `at://<account>/app.bsky.feed.generator/<key>`. The service serves exactly the feeds in
  the database: a request for any other URI is `400 UnknownFeed`, which matters because anyone can write a
  record that points at this service.
- The owner's feeds are built at start and every 20 seconds, as before. **Other people's feeds are built
  when someone first asks for them** (a request waits up to 4 seconds for that first build), rebuilt every
  minute only while someone keeps asking, and let go after 10 minutes of silence; at most 4 are built at
  once. A feed costs a query over a day of posts, and there is no limit to how many people can make.
- Feeds saved on the web take effect within seconds (`SyncNow` after a save; otherwise a 30 second timer).
  The loader refuses an empty list from the database and skips a row that is no longer valid, so one bad
  row does not stop the service.
- Metrics and request IDs name the owner's feeds and call everyone else's `user`, so they stay bounded.
- `describeFeedGenerator` lists the owner's feeds and up to 200 of everyone else's.

## Code map

- `internal/feedgen`: `feed_spec.go` (`FeedSpec`, limits), `feed_store.go` (the table), `feed_load.go`
  (what is served), `feeds.go` (the registry: pinned and lazy feeds), `feeds_api.go` (`/api/me/feeds`,
  client metadata), `resolve_handle.go`, `server.go`, `web.go`.
- `cmd/feedgen`: `main.go` (seeding, loading, `SyncFrom`, `make feeds-publish`), `webfeeds.go`.
- Pages: `web/index.html` + `static/app.js` + `static/mine.js` (the builder's saving),
  `web/feeds.html` + `static/feeds.js` + `static/feeds-page.js` + `static/publish.js` + `static/feeds.css`.

## Tests

```
go test ./internal/feedgen ./cmd/feedgen                          # API, store, registry, server, pages
TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen    # also the pages' scripts in jsdom
CLICKHOUSE_ADDR=localhost:9000 TOPICFEED_CLICKHOUSE_TEST=1 \
  go test ./internal/feedgen -run 'FeedStore|Seeding'            # the store against a real ClickHouse
```

The scripts' tests are `webtest/mine.test.mjs`, `publish.test.mjs` (publishing against a fake account
server and fake OAuth client) and `feeds.test.mjs`. `webtest/preview.mjs` serves the pages with made-up
data for looking at them (`/`, `/feeds`, with `?empty ?many ?long ?broken ?signedout`).

What the tests can't do is the real sign-in to a real Bluesky server: that needs a person's account and
the public address of this service. Try it once after deploying: build and save a feed, open `/feeds`,
**Connect to publish**, publish, and look for the feed in the Bluesky app.

## Deploying

1. `make schema` (applies `schema/014_user_feeds.sql`; idempotent). feedgen refuses to start without it
   and says so.
2. `make feeds`. The first start copies the owner's topic feeds into the database.
3. Check `docker logs topic-feed-feedgen` for "copied the feeds of the config file into the database" and
   that feed sizes in the metrics (`feedgen_feed_posts`) are as before.
