package labeler

import (
	"fmt"

	typesafe "github.com/haileyok/typesafe-client/go"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// QuestionsVersion identifies the question wording below. Bump it on any change; it
// is part of every label_config.
//   - q1: the broad question lists text, alt text, link, quoted post, and tags.
//   - q2: it also lists attachments, text in images, image descriptions, and labels (pd2).
//   - q3: adds sentiment, critical, and the kinds of promotion (ad, engagement bait,
//     spam, self-promotion).
const QuestionsVersion = "q3"

// Ranking signal question IDs (suffixes after "p{i}_"), plan §10.3.
const (
	sigSubstance       = "substance"
	sigNews            = "news"
	sigPromo           = "promo"
	sigGeneralInterest = "general_interest"
	sigTone            = "tone"
	// q3
	sigSentiment      = "sentiment"
	sigCritical       = "critical"
	sigAd             = "ad"
	sigEngagementBait = "engagement_bait"
	sigSpam           = "spam"
	sigSelfPromo      = "self_promo"
)

// yesNoSignals are the signals asked as yes/no questions, read as P(yes).
var yesNoSignals = []string{sigNews, sigPromo, sigGeneralInterest, sigCritical, sigAd, sigEngagementBait, sigSpam, sigSelfPromo}

var sentimentLevels = []string{
	"Very negative",
	"Somewhat negative",
	"Neutral or mixed",
	"Somewhat positive",
	"Very positive",
}

var substanceLevels = []string{
	"Low effort: a few words, a reaction, or filler",
	"Some substance: a clear thought, opinion, or share",
	"Substantive: informative, insightful, creative, or detailed",
}

var toneOptions = []typesafe.ChoiceOption{
	typesafe.Opt("informative", "Mainly conveys information, news, facts, or explanation."),
	typesafe.Opt("humorous", "Mainly a joke, meme, wordplay, or playful."),
	typesafe.Opt("personal", "Mainly about the author's own life, feelings, or experiences."),
	typesafe.Opt("outraged", "Mainly angry, indignant, or outraged."),
	typesafe.Opt("supportive", "Mainly encouraging, grateful, celebratory, or kind."),
	typesafe.Opt("other", "None of the above describes the main tone."),
}

func broadOptions(t *taxonomy.Taxonomy) []typesafe.ChoiceOption {
	opts := make([]typesafe.ChoiceOption, 0, len(t.Broad))
	for _, b := range t.Broad {
		opts = append(opts, typesafe.Opt(b.ID, b.Description))
	}
	return opts
}

func subOptions(b taxonomy.Topic) []typesafe.ChoiceOption {
	opts := make([]typesafe.ChoiceOption, 0, len(b.Subtopics))
	for _, s := range b.Subtopics {
		opts = append(opts, typesafe.Opt(s.ID, s.Description))
	}
	return opts
}

// pass1Questions asks the broad topic and the ranking signals for post i.
func pass1Questions(t *taxonomy.Taxonomy, i int, q typesafe.Questions) {
	p := fmt.Sprintf("`posts[%d]`", i)
	id := func(s string) string { return fmt.Sprintf("p%d_%s", i, s) }
	q[id("broad")] = typesafe.Choice(
		fmt.Sprintf("Which broad topic best describes the post %s (its text, alt text, attachments, text in images, "+
			"image descriptions, labels, link, quoted post, and tags)?", p),
		broadOptions(t)...)
	q[id(sigSubstance)] = typesafe.Score(fmt.Sprintf("How substantive is the post %s?", p), substanceLevels...)
	q[id(sigNews)] = typesafe.Noul(fmt.Sprintf("Is the post %s about a current event or breaking news?", p))
	q[id(sigPromo)] = typesafe.Noul(fmt.Sprintf("Is the post %s mainly self-promotion, an advertisement, or a request for follows, likes, or reposts?", p))
	q[id(sigGeneralInterest)] = typesafe.Noul(fmt.Sprintf("Would someone who doesn't know the author find the post %s interesting?", p))
	q[id(sigTone)] = typesafe.Choice(fmt.Sprintf("What is the main tone of the post %s?", p), toneOptions...)
	q[id(sigSentiment)] = typesafe.Score(fmt.Sprintf("What is the overall sentiment of the post %s?", p), sentimentLevels...)
	q[id(sigCritical)] = typesafe.Noul(fmt.Sprintf("Is the post %s negative about, critical of, or mocking the main thing it is about "+
		"(for example a post about AI that says AI is bad)?", p))
	q[id(sigAd)] = typesafe.Noul(fmt.Sprintf("Is the post %s an advertisement, sales pitch, or deal for a product or service?", p))
	q[id(sigEngagementBait)] = typesafe.Noul(fmt.Sprintf("Does the post %s mainly ask for follows, likes, reposts, or replies "+
		"(follow trains, \"like if you agree\", repost-to-win giveaways)?", p))
	q[id(sigSpam)] = typesafe.Noul(fmt.Sprintf("Is the post %s spam: a scam, a crypto or money scheme, repetitive or automated junk, "+
		"a link farm, or piles of unrelated hashtags?", p))
	q[id(sigSelfPromo)] = typesafe.Noul(fmt.Sprintf("Is the post %s the author sharing or promoting their own work "+
		"(their art, writing, music, stream, shop, or research)?", p))
}

// subQuestionID is the question ID for post i's subtopic within a broad topic.
func subQuestionID(i int, broad string) string { return fmt.Sprintf("p%d_%s_sub", i, broad) }

// pass2Question asks which subtopic of broad topic b post i is about.
func pass2Question(b taxonomy.Topic, i int) typesafe.Question {
	return typesafe.Choice(
		fmt.Sprintf("Which kind of %s content is the post `posts[%d]` about?", b.Name, i),
		subOptions(b)...)
}
