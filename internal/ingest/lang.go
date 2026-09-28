package ingest

import (
	"regexp"
	"strings"

	"github.com/pemistahl/lingua-go"
)

// Posts shorter than this (after removing links, mentions, and hashtags) skip the
// detector check; their `langs` tag is trusted.
const minDetectRunes = 20

// The detector only rejects a post when it names another language with at least
// this confidence. Below it, the `langs` tag wins. Tune from the disagreement metric.
const rejectConfidence = 0.6

// The detector is restricted to languages common on Bluesky, for speed. A post in a
// language outside this set tends to land on its nearest neighbor here, which is
// still "not English" for our purposes.
var detectLanguages = []lingua.Language{
	lingua.English, lingua.Japanese, lingua.Portuguese, lingua.Spanish, lingua.German,
	lingua.French, lingua.Korean, lingua.Chinese, lingua.Italian, lingua.Dutch,
	lingua.Russian, lingua.Ukrainian, lingua.Polish, lingua.Turkish, lingua.Indonesian,
	lingua.Tagalog, lingua.Swedish, lingua.Catalan,
}

var (
	reURL     = regexp.MustCompile(`(?i)\bhttps?://\S+|\bwww\.\S+`)
	reMention = regexp.MustCompile(`@[\w.-]+`)
	reHashtag = regexp.MustCompile(`#\S+`)
)

// LangVerdict is the result of checking a post's language.
type LangVerdict struct {
	Keep     bool
	Detected string // ISO 639-1 code of the detector's pick, or "" when not checked
	Reason   string // why the post was rejected, for metrics; "" when kept
}

// LangChecker decides whether a post is English, using its `langs` tag and a local
// detector. It is safe for concurrent use.
type LangChecker struct {
	detector lingua.LanguageDetector
}

func NewLangChecker() *LangChecker {
	return &LangChecker{
		detector: lingua.NewLanguageDetectorBuilder().
			FromLanguages(detectLanguages...).
			WithPreloadedLanguageModels().
			Build(),
	}
}

// taggedEnglish reports whether the post's `langs` includes English ("en" or "en-*").
func taggedEnglish(langs []string) bool {
	for _, l := range langs {
		l = strings.ToLower(l)
		if l == "en" || strings.HasPrefix(l, "en-") {
			return true
		}
	}
	return false
}

// Check applies the language filter (plan §9.1): the post must be tagged English, and
// the detector must not confidently name another language.
func (c *LangChecker) Check(text string, langs []string) LangVerdict {
	if !taggedEnglish(langs) {
		return LangVerdict{Reason: "not_tagged_en"}
	}
	clean := strings.TrimSpace(reHashtag.ReplaceAllString(reMention.ReplaceAllString(reURL.ReplaceAllString(text, " "), " "), " "))
	if len([]rune(clean)) < minDetectRunes {
		return LangVerdict{Keep: true}
	}
	vals := c.detector.ComputeLanguageConfidenceValues(clean)
	if len(vals) == 0 {
		return LangVerdict{Keep: true}
	}
	top := vals[0]
	code := strings.ToLower(top.Language().IsoCode639_1().String())
	if top.Language() != lingua.English && top.Value() >= rejectConfidence {
		return LangVerdict{Detected: code, Reason: "detector_disagrees"}
	}
	if top.Language() != lingua.English {
		// Not confident enough to overrule the tag.
		return LangVerdict{Keep: true, Detected: "en?"}
	}
	return LangVerdict{Keep: true, Detected: code}
}
