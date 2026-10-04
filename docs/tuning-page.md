# The "For you" page: sign in, see your interests, tune your feed

**Owner:** Hailey (`hailey.at`)
**Status:** Built and tested; the tuning controls, every setting of the feed, and the preview are not
deployed yet. Agreed with Hailey on 2026-10-01 (and "all the knobs, full width, richer previews" the
same day). Shared interests builder; tuning model, storage, and applying it in the personal feed
(`schema/010` is applied to the live database); sign-in (`internal/signin`, works in production); and
the page at `/me`: your interests with the liked posts behind them, a control for every setting of the
feed, and a preview of the posts a draft would pick, with what the feed knows about each. Hailey needs
to run `make feeds`, open `/me`, and try it with her own account.

A page on `https://feeds.hailey.at/me` where a person signs in with their Bluesky account,
sees what their likes say they're interested in, and tunes how their "For you" feed is built,
with a preview of the posts the tuned feed would pick.

Requirements (from Hailey): hosted on feeds.hailey.at; sign in with atproto OAuth using
`github.com/jcalabro/atmos`; see your own interests; tune the feed on the page.

## Decisions

- **OAuth proves who you are, nothing more.** `atmos/oauth` runs the full flow (PAR, PKCE, DPoP)
  and `Callback` verifies that the account's PDS really uses the authorization server it signed in
  through. We read the verified DID from the token set and immediately `SignOut` (revokes the
  tokens), so no Bluesky tokens are ever stored. Our own signed cookie carries the session.
- **Public OAuth client** (`token_endpoint_auth_method: none`, scope `atproto`): no signing key to
  manage. Client metadata is served at `https://feeds.hailey.at/oauth/client-metadata.json`
  (that URL is the `client_id`); redirect URI `https://feeds.hailey.at/oauth/callback`.
- **Session cookie:** `did|expiry` signed with HMAC-SHA256 using `FEEDGEN_SESSION_SECRET` (at
  least 32 bytes; the page stays off without it). `HttpOnly`, `Secure`, `SameSite=Lax`, 30 days.
- **Only your own account.** Every `/api/me/*` endpoint takes the DID from the cookie, never from a
  parameter. State-changing requests must be `application/json` from the same origin.
- **atmos's memory stores aren't concurrency-safe**, so the pending-login state lives in our own
  locked, expiring store.
- **Tuning is stored in ClickHouse** (`viewer_settings`, ReplacingMergeTree by `updated_at`), read
  when a viewer's state is built and applied when their feed is assembled. Saving rebuilds it.

## How sign-in works (built: `internal/signin`)

- **Browser binding.** Starting a login sets a short-lived `__Host-feeds_login` cookie holding the
  OAuth `state`; the callback must bring the same value back. Without it, a link that finishes
  *someone else's* login (login CSRF) would sign the victim in as the attacker.
- **The account is whatever the person's own server vouches for.** The library checks that the DID
  in the token response names the authorization server that answered; a server can't claim someone
  else's account. Typing a handle that points at someone else's DID only leads to *their* server.
  (Handle verification is therefore not security-critical here; it is on anyway, for showing the
  handle.)
- **Nothing to look after.** The tokens are revoked right after the callback and dropped from
  memory even if revoking fails. The cookie (`__Host-feeds_session`, `HttpOnly`, `Secure`,
  `SameSite=Lax`, 30 days) is `v1.<DID>.<expiry>.<HMAC-SHA256>`, with a purpose string mixed into the
  signature so it can't be reused for anything else signed with the same secret.
- **State-changing requests** (`/oauth/login`, `/oauth/logout`, and later the PUT/POST API) must come
  from our own origin (`Origin`, else `Sec-Fetch-Site`). Logins are limited to 5 at once per visitor,
  then one per 10 seconds; at most 5,000 logins can be in progress (they expire after 10 minutes).
- **Cookies must obey the browser's prefix rules.** A `__Host-` cookie is kept only if it is `Secure`,
  has path `/`, and has no `Domain`; a browser discards any other without a word. The first deploy
  gave the login-state cookie path `/oauth`, so no login could ever finish (the log said "a callback
  that this browser didn't start", reason "the browser sent no login cookie"). The tests now drive
  logins through a cookie jar that enforces these rules (`internal/signin/browser_test.go`); a test
  that only reads the `Set-Cookie` header can't see this.
- **Off unless asked for.** `FEEDGEN_SESSION_SECRET` (32+ characters) turns it on; set but too short
  is a startup error, not a quiet "off".
- **Not testable without a browser:** the real sign-in against Bluesky's own servers. It is tested end
  to end against fake PDS and authorization servers (including PKCE and the DID-vouching rule), and
  Hailey needs to try it once after deploying.

### One JWT library per program

`atmos/oauth` imports `atmos/serviceauth`, whose `init` registers `ES256`/`ES256K` signing methods in
the JWT library's global table, replacing the ones indigo's `atproto/auth` registered. The feed
service checked viewers' credentials with indigo, so linking atmos made **every viewer's credential
fail**: all viewers would have looked anonymous and seen only the welcome post, with no error at
startup. The existing credential tests caught it. The fix:

- Viewers' credentials are now checked by `verifyCredential` (`internal/feedgen/credential_verify.go`),
  which uses the signing methods atmos's `serviceauth` registers (same checks as before: audience
  with or without `#bsky_fg`, method binding, expiry with 30 s leeway, retry after refreshing the
  account's key on a bad signature; plus a token-type allowlist and a 5-minute limit on how far ahead
  a credential may expire, and on its age when it says when it was issued).
- **It does not call `serviceauth.VerifyToken`, on purpose.** That function requires the `iat` and
  `jti` claims, but atproto's own verifier asks only for `iss`, `aud` and `exp` (`xrpc-server`
  `parsePayload`), and servers older than atproto PR #2743 don't send `iat`. Every viewer of such a
  server was refused (`invalid viewer credential: serviceauth: missing iat claim`, visible only with
  `LOG_LEVEL=debug`) and saw just the welcome post, while sign-in and the tuning page worked. Found on
  2026-10-01. Tests must build such credentials with `testViewer.tokenWith`, because
  `serviceauth.CreateToken` always adds `iat` and `jti`.
- `feedgen.CheckCredentials()` runs at startup and refuses to start if a credential signed with
  each key type can't be checked, or if one signed with the wrong key is accepted, whatever the
  cause. It is also tested in the `cmd/feedgen` test binary, which links everything the service does.
- Don't import indigo's `atproto/auth` into the feed service again.

## The tuning model

All bounded and validated (`Tuning.Validate`); anything unknown is rejected. A setting left out, or at
zero where zero isn't a value (a count), is the feed's own.

| Setting | What it does |
|---|---|
| `topics` | Per subtopic, multiplies the topic's share of the feed: 0 mutes it, 1 is natural, up to 5. Any taxonomy subtopic can be added (adult ones aren't offered); it starts as strong as the viewer's median interest, then the weight applies. |
| `freshness` | `popular`, `balanced`, `fresh`: starting points for the ranking's gravity and fresh slots. |
| `authorGap` | Slots between one author's posts (0 is a value: no spacing). |
| `halfLifeDays`, `lookbackDays` | How fast old likes fade (1-30), and how far back likes count (1-30). |
| `minLikes`, `interests` | How many liked, classified posts it takes to use the viewer's own interests (1-500), and how many interests the feed is made from (1-100). |
| `windowHours`, `minTopicProb`, `listSize` | Only posts newer than this (in hours, down to 0.5), only posts whose topic the model is this sure of, and the list's length. These can only be made *stricter* than the feed's own: the feed's pool holds nothing older or less sure, and its list size is the most it keeps (`Tuning.Config`). |
| `maxServes` | How many times a post may be sent before it counts as seen (1-20). Doesn't apply while `showSeen` is on. |
| `showSeen` | `true` keeps posts the viewer has already seen in the feed: the ones Bluesky reported as seen and the ones sent `maxServes` times. Their own posts and the ones they liked or reposted stay out either way. Off (the default) is the feed's own rule. With it on nothing wears out, so every refresh starts again from the best posts. The page words it the other way round, as a switch "Leave out posts I've already seen" that is on until turned off. |
| `minEngagement` | How much reaction a post needs before it is shown, in likes' worth: likes, reposts, replies and quotes at the **feed's** ranking weights, not the viewer's, so a viewer who makes every kind count for nothing in their own ranking still has posts able to meet it (0-200; 0 is a value: no minimum). Posts below it are left out even when nothing else is left, so a topic that has run out gives its slots to the others and, when all have, the feed ends rather than filling with posts nobody has reacted to. |
| `ranking` | The numbers the feed ranks with, each optional (0 is a value): `gravity` (0-10), `freshEvery` (0-50), `promoPenalty` (0-10), the weights of a `like`, `repost`, `reply` and `quote` (0-20), and `engagementPower` (0.2-1: how much popularity counts; engagement is raised to it, so below 1 a post with thousands of likes can't stay on top for hours). They apply after `freshness`. |
| `hidePromo` | Leaves out posts the model scores as ads, engagement bait, spam, or self-promotion. |
| `tone`, `signals` | Cutoffs and boosts on every score the model gives, as in a feed's own configuration (`Rules`): `max` and `min` (0-1, and a minimum can't be above its maximum) leave posts out, `weights` (-10 to 10) lift or sink them. |

## How the feed applies a tuning (built, steps 1-2)

- **Topics.** A weight multiplies how much of a subtopic the viewer liked, *before* the strongest
  20 interests are picked. So muting an interest lets the next-strongest one into the feed, and a
  topic they never liked can be added (it starts at the median of their top interests, then the
  weight applies). `feedgen.ProfileFor` is the one place that works this out; the feed and the
  interests view both call it.
- **Too few likes.** Under `min_likes` the viewer normally gets a mix of every topic. If they turned
  topics on, those topics *are* their interests (their few likes are ignored: they chose). If they
  only muted, they get the mix without the muted topics.
- **Muting is always honoured.** A viewer who mutes every interest gets the mix minus what they
  muted; if that leaves nothing, the feed is empty rather than quietly showing what they muted.
- **Freshness.** The feed's posts are ranked three ways when they are read (popular, the feed's own
  ranking, fresh), and a viewer's setting picks one. No per-viewer ranking work.
- **Filters and author gap.** Hidden posts are skipped as the feed is assembled; the author gap is
  passed to the assembler.
- **Memory of likes (half-life)** changes how likes are weighted in the database query, so a change
  reads the viewer's likes again (about 250 ms) while saving. If that read fails the new settings
  apply with the old weighting and the next request after a minute retries it.
- **Saving** (`Personal.SetTuning`) validates, stores in `viewer_settings`, then applies to the
  viewer's feeds in memory: their next request assembles a new feed from the start, and what they
  were already sent is still left out. A tuning saved while their first build is running wins over
  the one that build read.
- **When things fail.** If the saved tuning can't be read, the feed works untuned and retries at
  the next refresh (`feedgen_personal_tuning_errors_total`). A saved tuning that isn't valid
  counts as none.
- **Preview** (`Personal.Preview`) assembles the first posts of a *draft* tuning. It ignores what the
  viewer has already seen (it shows what the settings do, not what is left to read), but still
  leaves out their own posts and what they liked, and it sends and records nothing.

- **Ranking of their own.** The feed keeps its pool of posts both ranked three ways (for the three
  freshness settings, once for everyone) and unranked. A viewer whose tuning changes the ranking's
  numbers or boosts tones or signals (`Tuning.customRanking`) gets a pool ranked for them alone each
  time their feed is assembled (a few milliseconds for the whole pool; nothing is kept per viewer, and
  nobody else's feed is touched). Freshness alone just picks one of the shared pools.
- **Filters.** Cutoffs on tone and signals, "hide promotional", a shorter window and a surer topic are
  applied as the feed is assembled (`Tuning.Excludes`), so they leave posts out without reordering.
- **Likes are read again** when the tuning changes how they are read: the half-life or how far back
  they count (`likeKey`). Minimum likes and the number of interests only change what is made of the
  likes already read.
- **The interests view uses the same settings** (`Tuning.Config`), so the page shows what the feed does.
- **Preview** also says which topics the draft builds the feed from and each one's share, strongest
  first, which is what the shares next to each interest show while a draft is being edited.

`viewer_settings` rows are one per save; the newest row (by `updated_at`) is the viewer's tuning.
The store test runs the SQL on a real ClickHouse in a database of its own, and is skipped unless
asked for:

    set -a; . ~/.config/topic-feed/env; set +a
    TOPICFEED_CLICKHOUSE_TEST=1 go test ./internal/feedgen/ -run TestTuningStoreAgainstClickHouse -v

## The page (`web/me.html` and `web/static/`: `me.js`, `tuning.js`, `draft.js`, `knobs.js`, `stats.js`, `scores.js`, `dom.js`)

You change a *draft*; nothing touches your feed until you press save. The page uses the whole width:
interests, settings and preview side by side from 1500 px, interests and settings stacked beside the
preview from 1000 px. Below 1000 px they are three views you switch between with tabs (Interests,
Tune, Preview), because one column would be thousands of pixels long; "See the preview" in the save bar
switches to the preview.

- **At the top** are four tiles with the numbers behind the interests (posts liked, posts with a topic,
  interests in the feed, days until a like counts half).
- **The save bar floats at the bottom** of the window while there is something to say, so it is never
  scrolled out of sight.
- **Each interest has a colour** from its broad topic (a hue from the name, set through the style
  object), and its share of the feed is the big number in its heading.
- **Settings groups** show how many of their settings differ from the feed's own. "How your feed is
  mixed" and "Which posts" start open; Tone, Quality signals and Ranking start folded unless something
  in them is set. Fresh or popular is three cards, and the on/off settings (hide promotional, leave out
  posts you have seen) are rows with a switch at the end.

- **Each interest has a slider** with seven stops: muted, a lot less (×¼), less (×½), as you like it,
  more (×2), a lot more (×3), much more (×5). A weight of 1 on a topic from your likes is no setting
  and isn't sent; on a topic you added it is what adds it. The share next to each interest (of your
  likes, and of the feed) follows the draft as it is previewed, without redrawing the list.
- **Add a topic** you haven't liked from a list grouped by broad topic (every subtopic but the adult
  ones). An added topic can be removed again.
- **Every other setting** is in `knobs.js`, built from what the server says each is by default and how
  far it can go: *How your feed is mixed* (fresh or popular, variety of authors, memory, how far back
  likes count, number of interests, likes needed), *Which posts* (how new, how sure the topic is, length
  of the feed, whether to leave out posts you have seen, repeats (greyed out while seen posts are
  shown), hide promotional), *Tone* (six scores) and *Quality signals* (ten), each with a
  boost and an allowed range as on the feed builder, and *Ranking* (folded away: gravity, brand-new
  slots, promotional penalty, and the weights of likes, reposts, replies and quotes). A setting that
  differs from the feed's own is marked and has a "reset"; each group resets on its own. The ranking's
  numbers start from the chosen freshness and follow it until they are set. A slider reaches a saved
  value beyond where it usually ends, as far as the server allows.
- **The preview** shows the first 30 posts the draft would pick. Each is the post as Bluesky shows it
  (author, text, pictures and counts, fetched from Bluesky's public API, which the page's security
  policy allows) or, if Bluesky doesn't have it or can't be reached (and the page says so), as we know
  it. Under each is what the feed knows: the topic and how sure the model is, tone, score, slot (and
  whether it is a brand-new-post slot), age, and the engagement the ranking counted; "Show the scores"
  opens every post's bars for its topics, signals and tone, with a mark on any score the settings put a
  cutoff or a boost on. It updates 0.4 s after the last change; an answer for settings that have since
  changed is dropped and its requests abandoned, and it tries again by itself when the feed is still
  loading or the viewer is rate limited (a button for other failures).
- **The save bar** is only there when there is something to say: unsaved changes, saving, saved, or
  what went wrong. After saving, the interests are read again with the tuning applied (the list stays
  while that happens).
- **Everything shown goes through `textContent`**, and a post's address is a link only if it is a
  `https://bsky.app/` address. The page's security policy forbids inline script and style, so the
  sliders' colours are set through the style object.
- The stops of "angry" and "substance" that an earlier version offered are gone: they are the
  `outraged` tone's maximum and the `substance` signal's minimum. For reference, across a day of posts
  about 10% score above 0.8 for anger and 20% above 0.2; a quarter score above 0.47 for substance and
  half above 0.27.

### Files served from the edge are named by version

Cloudflare caches `/static/*` and tells browsers to keep it for four hours, whatever the service says.
After a deploy a browser can then hold an old script next to a new page, which is how `/me` once sat on
"Reading your likes…" forever. So the page and its scripts and styles are named by the hash of the
static files (`/static/v/<hash>/me.js`), served as immutable; the pages are never cached; scripts
import each other by relative path, so a page always gets matching files; old versions answer 404
uncached. Anything new that the edge could cache should be named by its contents the same way.

## Endpoints (on the feed service)

| Method and path | What |
|---|---|
| `GET /me` | The page (a static file; it asks `/api/me` whether you are signed in). |
| `GET /oauth/client-metadata.json` | The OAuth client metadata. |
| `POST /oauth/login` | Form field `handle`; starts the flow and redirects to the account's authorization server. |
| `GET /oauth/callback` | Finishes the flow, sets the cookie, redirects to `/me`. |
| `POST /oauth/logout` | Clears the cookie. |
| `GET /api/me` | `{did, handle}`, or 401. |
| `GET /api/me/interests` | Interests (natural and tuned), coverage of likes, and the liked posts behind each. |
| `GET /api/me/tuning` | `{tuning, defaults, limits, maxWeight, tones, signals, topics}`: the saved tuning (one that isn't valid counts as none, as it does for the feed); the feed's own value of every setting, with the ranking numbers for each freshness setting (`defaults`); how far each can go, which for the window, topic certainty and list length is the feed's own (`limits`); the names of the model's tone and signal scores; and the topics that can be added. |
| `PUT /api/me/tuning` | The body is the whole tuning, which replaces the saved one and is applied to the viewer's feed at once. Answers `{tuning}`. |
| `POST /api/me/preview` | The body is a draft tuning; the answer is `{state, interests: [{path, name, broad, share}], posts: [{uri, url, did, text, topic, topicPath, broad, top: [{path, name, p}], tone, signals, labels, score, indexedAt, likes, reposts, replies, quotes}], tookMs}`. Nothing is saved or marked as seen. |

`PUT` and `POST` must come from our own origin (`Origin`, else `Sec-Fetch-Site`) as `application/json`
(a browser won't send that cross-site without asking first), at most 16 KB, with no setting the
tuning doesn't know and nothing after it. Every value is checked (`Tuning.Validate`; topics must be
offered ones). Errors are `{error, message?}`: `not signed in` (401), `forbidden` (403), `unsupported`
(415), `invalid` (400, with a message for people), `too_large` (413), `limited` (429, with
`Retry-After`), `unavailable` and `loading` (503). What went wrong inside is logged, never sent.
Limits per viewer: reading or saving a tuning, 10 at once then one every 2 s; previews, 10 at once
then one a second (a preview assembles a feed from memory); the interests, 5 then one every 2 s.

## Build order

1. Move the interests builder from `cmd/profile` into `internal/feedgen` so the LAN tool and the
   public page share it.
2. Tuning model, validation, and applying it in the personal feed service; `viewer_settings`.
3. OAuth sign-in, sessions, and the `/api/me` endpoints.
4. The page, with the preview.
5. Docs; Hailey sets `FEEDGEN_SESSION_SECRET`, restarts the service, and tries a real sign-in.

A real sign-in needs a browser and the person's consent, so it can't be tested automatically: the
handlers are tested with a fake authenticator, and the atmos adapter is a thin wrapper around
`Authorize`, `Callback`, and `SignOut`.

## Testing

- Go: `tuning_test.go` (the model: validation of every setting, `Config`, `RankingFor`, `Excludes`,
  ranking with boosts), `personal_knobs_test.go` and `personal_tuning_test.go` (the feed: each setting
  changes the feed, one viewer's ranking leaves others alone, likes are read again when they should
  be), `me_tuning_test.go` (the three endpoints over a real personal feed service: signed in or not,
  refusals, rate limits, defaults and limits, what a preview says about each post, a save changes the
  feed and a preview changes nothing), and `me_page_test.go` (the page's scripts are all served by
  version and every element they use exists). `post_texts_test.go` and `tuning_store_test.go` run the
  SQL on a real ClickHouse in a database of its own when `TOPICFEED_CLICKHOUSE_TEST=1`.
- The page's scripts, in jsdom, against the real HTML (`webtest/me.test.mjs`, `tuning.test.mjs`):
  `TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen/` (or run the two files with
  `node --test`). Bluesky's API is stubbed.
- Mutation checks: the new code was broken on purpose in over a hundred ways (a limit not enforced, a
  setting not applied, a stale answer used, a link allowed to go anywhere...) and each change had to
  fail a test; the ones that didn't got a test.
- jsdom has no layout, so the CSS was checked in headless Chromium against a stub of the API: no
  horizontal overflow at 1920, 1280, 1100, 768 and 390 px, three, two and one columns where they should
  be, the save bar stays in view, a real click on a slider changes the draft, text contrast is at least
  4.5:1 in light and dark, no tap target under 24 px on a phone, nothing cut off inside a card, and no
  script errors or security-policy violations. That was done for the page with likes, with posts as
  Bluesky shows them (long text, pictures, link cards, quotes, video), with too few likes, signed out,
  with an empty preview and with a failing one.
- To look at the page in a browser without signing in or touching any data,
  `TOPICFEED_JSDOM=/path/to/node_modules/jsdom node internal/feedgen/webtest/preview.mjs` serves the real
  page and scripts, with the real security policy, over made-up answers from the four `/api/me*`
  endpoints (port 8760; `?signedout`, `?slow`, `?empty`, `?broken`, `?few` show the other states). The
  preview answers follow the sliders, so the shares change as they would.
