# Filtered feeds

A filtered feed shows another feed's posts, in that feed's order, minus the ones its filters leave
out. Nothing is added, ranked or boosted. The two that exist are the owner's, configured with
`filtered:` in `config/feeds.yaml`:

| rkey | source |
|---|---|
| `discover-filter` | Bluesky's Discover (`at://did:plc:z72i7hdynmk6r22z27h6tvur/app.bsky.feed.generator/whats-hot`, served by `did:web:discover.bsky.app`) |
| `foryou-filter` | spacecowboy's For You (`at://did:plc:3guzzweuqraryl3rdkimjamk/app.bsky.feed.generator/for-you`, served by `did:web:foryou.club`) |

Code: `internal/feedgen/filtered*.go` (config, filters, the request, the page's API) and
`internal/signin/connect.go` (the sign-in that is kept, and the tokens).

## Who the source sees

The source must see the viewer, or it can't personalize the feed or leave out what they've already
seen. For You without a viewer answers one pinned post and nothing else.

The token Bluesky sends us with a request is signed by the viewer's own server, but it is addressed
to us: when the app asks for a feed, the viewer's server reads the feed's record and signs a token
for the service the record names (`did:web:feeds.hailey.at`). Only the viewer's server holds the key,
so we can't sign one for the source. Discover would accept ours passed through (it checks only the
signature and expiry, `tango/iris/iris/service_auth.py`), but the common feed generator code checks
that a token is addressed to itself, and For You's code isn't public.

So a viewer signs in for the filtered feeds on `/filtered`, and the service keeps that sign-in. For
each request it asks the viewer's server for a token addressed to the source
(`com.atproto.server.getServiceAuth`, valid 30 minutes, reused until 5 minutes before it runs out),
and reads the source with it. The viewer is whoever signed the token Bluesky sent us, checked as for
every feed; the token for the source is always that viewer's own.

- **The permission asked for** is only `app.bsky.feed.getFeedSkeleton` and
  `app.bsky.feed.sendInteractions`, for the sources' services: no posting, no reading messages. Each
  service is named twice. Bluesky's servers only accept a service named with its `#bsky_fg` part
  (`rpc?lxm=...&aud=did:web:foryou.club%23bsky_fg`); some other servers (cocoon) compare a token
  request with the bare DID instead. A server ignores the form it can't read. Tokens are asked for
  with the `#bsky_fg` form first, then the bare DID. The services are the ones the sources' records
  name when the service starts: a source moved to another service needs a restart, and viewers must
  sign in again.
- **A confidential client.** The sign-in is its own OAuth client (`/oauth/connect-metadata.json`)
  that proves itself with a key made from `FEEDGEN_FILTER_SECRET`. Bluesky ends a public client's
  sign-in after two weeks no matter what. A confidential client's lasts while it is used at least
  every three months, up to two years.
- **Kept sealed.** Sign-ins (tokens and their DPoP key) are encrypted with AES-GCM under another key
  made from the same secret, with the viewer's DID bound in, in `filter_logins`
  (`schema/015_filtered_feeds.sql`). The newest row per viewer wins. Rows are dropped 180 days after
  the last save. A refresh token can be used once, so all of one viewer's token work happens one
  request at a time.
- **When it stops working.** A sign-in the viewer's server refuses (revoked, run out, refresh
  refused) is forgotten. The feed then shows only `FEEDGEN_FILTER_SIGNIN_POST`, the post asking them
  to sign in, until they do. The same goes for viewers who never signed in, and for requests with no
  valid credential. A server that is down (5xx) forgets nothing: that request fails with a 500.
- Signing in also signs the browser in on the site, like `/me`. "Sign out of the filtered feeds"
  revokes and forgets the kept sign-in, and leaves the browser signed in.

## A request

1. The viewer's filters for the feed (`viewer_filters`), or the feed's own: exclude, tone, signals,
   topic_rules (cutoffs only), and whether to drop posts the model never scored. They are cached
   for 30 seconds, and saving clears the cache.
2. Read the source with the viewer's token (`limit` = what was asked, at least 30), passing on the
   viewer's `Accept-Language` and `X-Bsky-Topics`. Look up the page's posts in `post_pipeline` and
   keep the ones the filters let through. Repeat until there are as many as were asked for, the
   source ends, or 10 pages / 6 seconds have gone by. Only then can a page come back short.
3. Answer **exactly** the number asked for. Bluesky's AppView cuts every page to the limit
   (`feedSkele.slice(0, params.limit)`), so posts beyond it would be lost. The extra posts are held
   for 30 minutes under a key in the cursor, for this viewer and feed only. The cursor is
   base64 JSON: the source's cursor, that key, and whether the source has ended. After a restart the
   held posts are gone and the feed carries on from the source's cursor. Asking for the same cursor
   again gets the same posts.
4. A post's `reason` (repost, pin) is passed on as is. Its `feedContext` becomes
   `fr1:<base64 of the source page's reqId>:<the source's feedContext>`, so an interaction that comes
   back can be sent on with the request ID and context the source gave that post, without anything
   kept on our side.

**Adult content** is what the topic model calls it: `adult_content` and its subtopics (explicit posts &
promo, horny & thirst posts, kink & fetish, NSFW art, other), in the topic list like any other topic
(`/api/taxonomy?for=filters` lists the adult topics, which the builder hides), so a viewer can leave
out explicit posts and keep NSFW art. Posts
the model never scored (not in English, replies) can't be judged; "leave out posts the model can't
judge" covers those.

**What was left out.** Every post a viewer's filters leave out is recorded in `filtered_left_out` with
the filter that did it (`Filters.Why`: the topic or score with its value and cutoff, and the topic
whose own rules decided). It is written in the background and kept a week. The page
`/filtered/left-out?feed=<rkey>` (linked from each feed on `/filtered`) lists them, newest first, each
once, 50 at a time as the viewer scrolls (`GET /api/me/filtered/{rkey}/left-out?cursor=`, the cursor
being the last post's time and URI). Only the signed-in viewer's own are read. Posts with labels this
site never shows (`posts.js`: adult and graphic labels) appear as the reason and a link to Bluesky.

A filter judges a post as a topic feed's exclude and cutoffs do: a topic's probability is the
subtopic's for a subtopic and the broad topic's for a broad topic, and a score the model didn't give
is 0. Topic rules apply to the post's most likely subtopic.

Posts the model never scored are kept unless the viewer asks otherwise. Ingest classifies only
English posts that aren't replies, which is about a third of Discover (68 of its top 100 were scored
on 2026-10-05).

## Interactions

The app sends third-party feeds likes, reposts, replies, quotes, "show more/less" and **seen**
(`THIRD_PARTY_ALLOWED_INTERACTIONS` in the app). Clickthroughs only go to Discover itself, so we
never get them. Interactions with a filtered feed's posts are stored like any feed's, and sent on to
the source in the background with the viewer's own `sendInteractions` token, the feed changed to the
source's URI and each post's context and request ID put back. Interactions with the sign-in post are
not sent on. A source gets them when its record sets `acceptsInteractions`, or when it is Discover:
Discover's record doesn't set it, but the app sends Discover interactions anyway. At most 64 sends
run at once; more are dropped (and counted).

## The page

`/filtered`: the sign-in for these feeds (the same form as the other pages, posting to
`/oauth/connect`), then a card per feed with topics to leave out (each with how sure the model must
be, adult topics included), tone and signal ranges, per-topic ranges, the switch for
unscored posts, and a link to what was left out. The feeds have no filters of their own,
so a viewer starts with nothing left out; "Clear all filters" saves `null` (with filters set in
feeds.yaml it reads "Use the feed's own filters"). API: `GET /api/me/filtered`,
`PUT /api/me/filtered/{rkey}`, `GET /api/me/filtered/{rkey}/left-out`.

## Running it

1. `make schema` (adds `filter_logins`, `viewer_filters` and `filtered_left_out`).
2. Set `FEEDGEN_FILTER_SECRET` (`openssl rand -base64 36`) in `~/.config/topic-feed/env`.
3. `make feeds-welcome FOR=filtered` posts the sign-in post. Put the `FEEDGEN_FILTER_SIGNIN_POST=` line
   it prints in the env file.
4. `make feeds`, then `make feeds-publish` for the two feeds' records.

Metrics: `feedgen_filtered_pages_total{feed,state}` (`state` is `signin` for the sign-in post),
`feedgen_filtered_posts_total{feed,kind}` (read, dropped),
`feedgen_filtered_source_requests_total{feed,status}`, `feedgen_filtered_source_seconds`,
`feedgen_forwarded_interactions_total{feed,result}`. A source that refuses a request is logged with
its answer.

## Not known yet

Whether For You accepts a token addressed to `did:web:foryou.club#bsky_fg` (the form Bluesky's servers
sign for this permission) or wants the bare DID. Its code isn't public. If it refuses, the log shows
`the source feed refused a request` with its answer, and the feed answers 502.
