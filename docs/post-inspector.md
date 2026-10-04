# Post inspector: why is this post (not) in a feed?

**Owner:** Hailey (`hailey.at`)
**Status:** Built and tested; not deployed yet. Requested 2026-10-04: a page on `feeds.hailey.at`
where she pastes a post's link and sees the post, how the topic model scored it, and which feeds
would have taken it.

`https://feeds.hailey.at/inspect` takes a bsky.app link to a post (or its `at://` address), and shows:

- **a verdict** in one line: how many topic feeds hold the post right now, how many would take it,
  or why none does;
- **the post**, as Bluesky shows it (ours, from what we stored, if Bluesky won't answer), and what we
  know of it: when we saw it, which model scored it and when, the label policy, language, pictures
  the model saw, reactions, labels on it or its author now, whether it or its author is gone;
- **what the model saw**: every subtopic and broad topic it scored (the best eight subtopics), tone
  and signals;
- **each feed**, grouped as *would take it*, *wouldn't take it* and *For you*: for every feed, each
  rule it checked with the numbers (topic and its threshold, topics left out, tone and signal
  cutoffs, labels, deleted or inactive author, the time window), where the post stands in the feed
  right now (`#14 of 439`) or why it isn't there, and its ranking score broken into prior,
  reactions and age.

A post we hold nothing of is explained instead: a reply (replies are dropped at ingest), a post not
tagged English, one older than our data, one deleted, one stored but not yet processed, or one the
label policy dropped. For a post we don't hold, we ask Bluesky's public API what kind of post it is.

## Who can use it

Only the owner of the feeds (`FEEDGEN_OWNER_DID`). The page signs in with the same session as `/me`
(so `FEEDGEN_SESSION_SECRET` must be set; the inspector is off without it). `/api/inspect` answers
`401` when nobody is signed in and `403` to anyone but the owner, before reading anything. It lays
bare how posts are judged and every answer reads the database, so it is not for viewers. The page
itself (`/inspect`) holds nothing private. Answers are limited to ten at once and then one every two
seconds.

## How it stays true to the feeds

- The rules are the feeds' own. `EvaluateFeed` restates the SQL conditions of `Store.Build`, one
  check at a time, and `TestEvaluateFeedAgreesWithTheSQLAgainstClickHouse` runs both against a real
  ClickHouse on the same posts and fails if they ever disagree. If you change what a feed
  admits in `Store.Build`, change `EvaluateFeed` with it.
- The ranking score uses `ScoreBreakdown`, which `Score` itself uses, so the explanation can't drift
  from the ranking.
- "In the feed right now" is read from the live build in memory (`Feeds.Posts`), not recomputed.
- *For you* is built per viewer from their likes and settings, so for it the page says only whether
  the post can be in the pool every viewer's feed picks from; whether one viewer is shown it depends on
  their interests and what they've already seen.

## Details worth knowing

- **Adult and graphic posts aren't shown as Bluesky shows them.** The page reuses the post card of
  the rest of the site, which leaves out posts carrying sexual-content or graphic-media labels (the
  inspector has no way to switch adult content on). For those the page shows our own card (the stored
  text and a link to the post) and everything else as usual.
- **What goes to Bluesky:** when we hold nothing of a post, the server makes one request to
  `public.api.bsky.app` with that post's address and nothing of the owner's. It waits at most five
  seconds; if that fails the page explains what it can without it. Separately, the browser asks
  `public.api.bsky.app` for the post card, as the other pages do, after the answer is on the page.
- **Handles:** a link with a handle (`bsky.app/profile/alice.example/post/…`) is resolved to a DID
  first. Text that isn't a handle is never looked up. A handle that names no account, or that its
  account doesn't name back, is reported as such; a failure to look is reported as a failure.
- **Text is shown as text.** Nothing from a post or from the server is put on the page as HTML.

## Where it lives

- `internal/feedgen`: `inspect_ref.go` (reading an address), `inspect_eval.go` (the rules and the
  live position), `inspect_store.go` (what is stored about a post), `inspect.go` (the API),
  `personal_inspect.go`, `rank.go` (`ScoreBreakdown`); routes in `server.go` and `web.go`.
- `cmd/feedgen/inspect.go`: wiring (owner, handle lookup, the Bluesky lookup).
- `internal/feedgen/web/inspect.html`, `web/static/inspect.js`, `inspect.css`; it reuses `me.css`,
  `posts.js`, `stats.js` and `scores.js`.

## Tests

```
go test ./internal/feedgen ./cmd/feedgen                      # API, rules, page structure
TOPICFEED_JSDOM=/path/to/node_modules/jsdom \
  go test ./internal/feedgen -run TestInspectPageScript      # the page's script in jsdom
CLICKHOUSE_ADDR=localhost:9000 TOPICFEED_CLICKHOUSE_TEST=1 \
  go test ./internal/feedgen -run 'Inspect|EvaluateFeed|PostFacts'   # against a real ClickHouse
```

To look at the page without signing in or touching data, run the preview server and open
`/inspect` (made-up answers; add a word to the post address to pick a state: `unscored`,
`unprocessed`, `notstored`, `none`, `mid`, `pictures`, `long`, `evil`, `bad`, `limited`, `broken`;
and `?signedout` or `?forbidden` for the other pages):

```
cd internal/feedgen && TOPICFEED_JSDOM=/path/to/node_modules/jsdom PORT=8761 node webtest/preview.mjs
```
