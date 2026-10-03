# Taxonomy v2: what changed and why

**Status:** v2 (`taxonomy/v2.yaml`) was drafted and tested on 2026-10-02 and **failed two of its four
automatic checks (Jev and Clef agree less often than under v1, and 'unclear' grew by 3 points)**. The
full relabel runs under **v2.1** (`taxonomy/v2.1.yaml`): v2 with two phrases removed from the
description of `unclear`. See "v2.1" at the end of this file. The policy choices below are what v2
tried; choices 3 and 4 (humor, unclear) are the doubtful ones, and the `unclear` additions of
choice 4 are what v2.1 removes. The test results of v2 are in the section after them.

v2 is v1 with seven broad-topic descriptions reworded. No topic or subtopic ID, name, subtopic
description, or example changed, so feeds, `config/feeds.yaml` paths, v1 labels, and the deployed
classifier keep working, and v1 and v2 labels can be compared one to one. Only the descriptions
change what a labelling model sees: both Jev (`internal/labeler/questions.go`) and Clef
(`trainer/clef_labels.py`) are given each broad topic's ID and description, not its examples.
The question wording itself is unchanged (still `q3`).

## Why these seven

On 6,322 text-only posts that both Jev and Clef-flash labelled under v1, they disagree on the
broad topic for 1,607 (25%). Ten pairs of topics account for a large share of that:

| Pair | Posts | Who says what | What the posts look like |
|---|---|---|---|
| humor / personal_life | 102 | Jev humor, Clef personal_life: 92 | relatable quips about one's own life |
| us_politics / world_news | 56 | 31 Jev world_news, 25 Jev us_politics | foreign politics in another language; US officials on foreign wars |
| personal_life / unclear | 49 | Jev personal_life: 36 | very short remarks |
| online_culture / personal_life | 46 | Jev online_culture: 33 | "ask me anything", chat rules, returning to the site |
| humor / unclear | 41 | Jev humor: 30 | one-liners, "lol" |
| personal_life / society | 38 | Jev society: 37 | personal stories with a social angle |
| society / us_politics | 37 | Jev society: 33 | rants about billionaires, Epstein, fascism |
| health / personal_life | 32 | Jev personal_life: 30 | being ill while describing the day |
| online_culture / technology | 28 | Jev online_culture: 23 | Telegram bans, labelers, app settings |
| entertainment / humor | 26 | Jev humor: 17 | jokes about films |

Most of the disagreement is one-sided: Clef uses humor (3.6% of these posts against Jev's 6.9%) and
society (1.8% against 4.6%) far less than Jev, and personal_life more (11.9% against 9.7%). Some
of it is plain model error that no wording can fix (a Greek post about Greek politics filed as
US politics, now-playing bots filed as music).

## The wording changes

| Topic | v1 | v2 |
|---|---|---|
| us_politics | US government, politicians, parties, elections, and policy. Not other countries' politics (see world_news). Not social issues without a government angle (see society). | US government, politicians, parties, elections, and policy, including partisan posts that name a US politician, party, or law, and US scandals whoever is involved. Politics with no country given defaults here. Not other countries' politics (see world_news). Not social, class, or ideology commentary that names no politician, party, or law (see society). |
| world_news | News, politics, and conflicts outside the US, including US foreign wars. Not US domestic politics (see us_politics). | News, politics, and conflicts of other countries, in any language, including foreign wars and diplomacy the US takes part in. Judge by whose politics the post is about, not whose people it mentions. Not US domestic politics, including the US argument over a foreign war (see us_politics). |
| personal_life | Personal and everyday life with no other clear topic, such as life updates, feelings, greetings, and routines. Posts about a specific hobby or topic go to that topic. | Sincere posts about the author's own life with no other clear topic, such as life updates, feelings, greetings, and routines. Posts about a specific hobby or topic go to that topic. A joke or relatable quip about one's own life goes to humor. A reaction too short to tell what it is about goes to unclear. |
| humor | Posts whose main purpose is to be funny. Humor about a specific topic still goes here unless it is mainly commentary on that topic. | Posts whose main purpose is to be funny, such as jokes, one-liners, memes, and absurd or relatable observations, including jokes about the author's own life. Humor about a specific topic still goes here unless it is mainly commentary on that topic. A bare 'lol' or emoji reaction is unclear. |
| society | Social issues, identity, crime, and community life discussed outside partisan politics. Government action on these issues goes to us_politics or world_news. | Social issues, identity, class, crime, and community life discussed outside partisan politics, such as general posts on billionaires, capitalism, or injustice that name no politician, party, or law. Posts about politicians, parties, legislation, or elections go to us_politics or world_news, even when the issue is social. A personal story goes to personal_life unless its point is the social issue. |
| online_culture | Bluesky and social media life, such as platform talk, follower milestones, prompts, and viral discourse. Not general tech news (see technology). | Bluesky and social media life, such as platform talk, app settings and bans, follower milestones, prompts, viral discourse, and posts that ask followers or mutuals to interact (ask me anything, chat rules, callouts). Not general tech news (see technology). Not a personal update that only happens to be posted here (see personal_life). |
| unclear | No discernible topic, or not enough content to tell. Includes bare links, emoji-only reactions, random code strings such as 'macro: …', and untranslatable fragments. | No discernible topic, or not enough content to tell. Includes bare links, emoji-only or one-word reactions, replies that only make sense inside a conversation we cannot see, random code strings such as 'macro: …', and untranslatable fragments. Not a clear joke (see humor). Not a clear remark about the author's own day or state (see personal_life). |

## Policy choices and the reasons

1. **Keep every ID, name, subtopic, and example.** Feeds use 13 paths, the student classifier is
   trained on paths, and v1 labels stay comparable. A reworded description changes which posts land
   in a topic, not what the topic is called. Because no ID changed there is no ID mapping; none is
   needed for the import either.
2. **Edit descriptions only, not the questions.** The signals (tone, substance, news, promo, ...)
   and the subtopic question stay as they were, so calibrating Clef's signals to Jev's carries over
   in kind (the numbers are still refitted on v2 data).
3. **A joke about one's own life is humor, not personal_life.** Largest pair (102), and 92 of the 102
   are Jev humor against Clef personal_life. v1 already said humor is for posts "whose main purpose
   is to be funny"; v2 states that it applies when the subject is the author. Sincere posts stay
   personal_life.
4. **Unclear is the leftover, not a rival.** A clear one-liner joke is humor, a clear remark about the
   author's own day or state is personal_life, a bare "lol" or emoji, a one-word reaction, or a reply
   that only makes sense inside a conversation we cannot see is unclear. This settles
   personal_life/unclear (49) and humor/unclear (41) by giving each side a test, and adds to unclear
   only what neither model could place. It must not raise the share of unclear (checked in the
   test: v1 Jev 8.3% of these posts).
5. **Asking followers to interact is online_culture; a personal update posted here is personal_life.**
   "Ask me anything", chat rules and callouts address the audience. Settings, bans, and labelers of a
   social app count as platform talk, not technology, matching v1's rule that Bluesky platform talk
   is online_culture. Birthday shoutouts remain online_culture/shoutouts for users; a greeting to a
   celebrity is not one.
6. **Named politicians, parties, bills, and elections decide politics; their absence decides
   society.** A post that names a US politician, party, or law is us_politics (or world_news for
   another country) even if the issue is social. General class, billionaire, or ideology commentary
   that names none stays society (matches what Jev did on 33 of 37 society/us_politics posts and
   the existing social_commentary subtopic). Epstein-files posts stay us_politics (v1's
   courts_legal already lists them).
7. **A personal story goes to personal_life unless its point is the social issue.** Jev chose
   society for 37 of the 38 personal_life/society posts, Clef personal_life. Without this the
   daycare-crash story or the sidewalk-blocker rant is a social issue; with it the author is the
   subject.
8. **Whose politics decides us_politics versus world_news.** US scandals that involve foreigners are
   us_politics; posts in any language about another country's politics are world_news. v1's rule
   that US foreign wars are world_news (approved on 2026-09-28) is kept, narrowed: the war itself
   and diplomacy are world_news; the US domestic argument about it (Congress, approval, blame) is
   us_politics.
9. **Politics with no country given defaults to us_politics.** Most political posts on Bluesky are
   about the US and the pair is a near coin-flip otherwise (56 posts, split 31/25). Known cost: an
   unmarked post about another country will be filed as us_politics.
10. **Left alone on purpose.** health/personal_life (32): v1's health example ("Finally getting over
    this cold") already makes illness the subject; the posts are ambiguous and Jev chose
    personal_life in 30. automated_feeds/music (32): now-playing bots are already in
    automated_feeds; Clef's answer is the slip. humor/sports (25) is balanced both ways (12/13) and
    the owner's judgments favored Jev (3 of 6). lifestyle/personal_life (22, Jev lifestyle in 21) is
    covered by personal_life's existing "posts about a specific hobby or topic go to that topic",
    and entertainment/humor (26) by humor's existing "unless it is mainly commentary on that topic".
11. **No `v2-equivalences.yaml` yet.** v1's equivalence file excuses boundaries the v1 wording did not
    draw ([unclear, humor/shitposts, online_culture/other]); v2 tries to draw them, so carrying the
    groups over would hide whether it worked. Scoring against v2 uses exact paths until a human
    reviews v2 answers.
12. **Descriptions got longer: about +214 words over the 25 broad topics (479 to 693), roughly +300
    tokens on each broad question.** A request was about 2,400 tokens under v1, so expect about 12%
    more input tokens for the broad question, around $0.11 to $0.12 per 1,000 posts (v1: $0.102).
13. **The file is frozen once testing starts.** The label_config hash includes a hash of the
    taxonomy file's bytes, so any edit, even a comment, gives a new label_config and makes earlier v2
    labels unreusable. If the gates fail, the wording changes in a new version, not in this file.

## Test result (2026-10-02)

Both models labelled the same 2,500 posts under v2: 2,000 text-only posts (the 108 text posts the
owner judged by hand plus 1,892 drawn at random from the 6,322 that both models had labelled
under v1) and the 500 picture posts. Jev saw each post exactly as under v1 (plain or with the
pipeline's findings, by the source of its v1 label). Clef-flash ran on the RTX 5090 with the v2
file. Nothing failed: 2,500 of 2,500 answers from each. The Jev test cost about $0.31 (4,901
requests, 7.4 million input tokens) and took 9 min 48 s at 255 posts a minute; the Clef test
took 16 min (2.95 posts/s on text, 1.7 on pictures).

The checks were fixed before the v2 answers were looked at (`trainer/v2_gates.py`); the full
output is `/data/reports/taxonomy-v2-test/report.md`.

| Check | Result |
|---|---|
| A. Jev and Clef disagree on fewer random text posts, one-sided McNemar p < 0.05 | **FAIL**: they agree on the broad topic for 76.5% under v1 and 74.1% under v2 (445 -> 490 disagreements; 160 posts agree only under v1, 115 only under v2). |
| B. Picture posts agree no more than 3 points less | pass: 64.6% -> 63.0%. |
| C. 'unclear' grows by at most 1.5 points for either model | **FAIL**: Jev 8.2% -> 11.2%, Clef 8.5% -> 11.4%. |
| D. No topic collapses, none above 25%, none disappears | pass. |
| E. The judged posts read | The accepted-answer score of Jev on the 95 scored judged text posts fell from 87% to 78%; Clef's stayed 49% -> 51%. On the 29 scored picture posts Clef went 100% -> 97%. |

What moved, from the report (random text posts):

- **Better:** personal_life/society splits 16 -> 8, personal_life/unclear 14 -> 8, online_culture/technology
  9 -> 6, online_culture/personal_life 11 -> 10. Those are policy choices 7 and 5 and part of 4.
- **Worse:** humor/unclear 8 -> 24, humor/personal_life 28 -> 34, us_politics/world_news 17 -> 23,
  health/personal_life 8 -> 13, entertainment/humor 9 -> 15, society/us_politics 8 -> 13.
- **Jev followed the new humor wording, Clef did not.** Jev's humor share doubled (6.7% -> 12.2%; 106
  posts moved into it, mostly out of personal_life, 41) while Clef's barely moved (3.6% -> 4.3%).
  Policy choice 3 therefore widened the gap it was meant to close.
- **The 'unclear' growth is the new unclear wording** (choices 4): Jev sent 22 personal_life posts
  and 5 sports posts to unclear, Clef 32 personal_life posts. Three short sports reactions among
  the judged posts moved from sports to unclear in Jev's answers ("Nice push off, McConkey." and
  "Damn it Kyler you had him", where the owner had accepted Jev's sports answer, and "lol they
  are booing him in Knoxville.", where the owner accepted neither model's).
- Policy choices 8 and 9 (US argument over a foreign war; no country given defaults to
  us_politics) did not help: Jev moved 10 world_news posts to us_politics, such as "Excellent
  deterrence sir. Well played", which Clef kept as world_news.

These attributions to individual choices are inferred from which topics gained and lost posts, not
tested one edit at a time; the test cannot separate the edits that were made together.

The 2,500 v2 labels from this test are in `jev_labels` (taxonomy_version v2, label_config
c8c15812c7ab, source relabel). A revised wording must get a new version number, not reuse v2, so
these rows are never mixed with it.

## v2.1 (the version the full relabel uses)

**What changed from v2:** the description of `unclear` lost "or one-word reactions" and "replies that
only make sense inside a conversation we cannot see". Nothing else differs from v2 (checked
field by field: same IDs, names, subtopics, examples, and the other 24 descriptions). The version
string is `v2.1`; Jev's label_config is `30cf2c546019`. The rejected v2 stays as it was, with its
2,500 test labels (`label_config c8c15812c7ab`).

**Why:** the v2 test showed the two phrases sent short posts whose topic came only from a
name ("Damn it Kyler you had him") out of their topic and into `unclear` (see the test result
above). Posts of that kind had low-confidence topic answers under v1 too, so the change moves
them back toward the v1 behaviour without touching the rest of v2.

**Agreed with Hailey on 2026-10-02:** text agreement between Jev and Clef is not a check (Jev is
the text teacher; Clef labels the posts with pictures), and the humor wording is looked at after
the full run rather than before it. Humor therefore stays as v2 has it: Jev puts more posts in
humor than under v1 (12.2% against 6.7% of the random test posts), mostly out of personal_life.

**Test of v2.1** (Jev only, the 2,000 text-only test posts; run 14:45 to 14:54 UTC, 0 failures;
`trainer/v21_check.py`, output in `/data/reports/taxonomy-v2-test/v21-check.txt`):

| Check | v1 | v2 | v2.1 | Result |
|---|---|---|---|---|
| C. share of Jev's answers that are `unclear`, 1,892 random posts (must be at most v1 + 1.5 points) | 8.2% | 11.2% | 9.4% | pass, 1.2 points above v1 |
| D. no topic collapses, none above 25%, none disappears | | | | pass |
| E. Jev's accepted-answer score on the 95 scored judged text posts (must be at most 5 points below v1) | 87% | 78% | 83% | pass, 4 points below v1 |
| sports posts that left sports under v2 and are back under v2.1 | | 16 left | 6 back | informational |

Both passes are close to their limits. `unclear` is still 1.2 points above v1, and the judged
score is 4 points below. The ten sports posts still out of sports under v2.1 are mostly the ones
with the weakest cue ("Woof", "VAMOS Carlos!!", "Damn it Kyler you had him") plus a few mixed
posts that name a politician or a wrestling storyline.

**Tag:** the final labels carry `taxonomy_version = 'v2.1'` (the goal said v2; v2 is the rejected
draft and keeping its name for a different wording would mix two label sets).
