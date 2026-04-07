// Package refusal provides lightweight refusal detection for LLM streaming responses.
package refusal

import (
	"regexp"
	"strings"
)

// RefusalLevel indicates the confidence level of refusal detection.
type RefusalLevel int

const (
	// LevelNone means no refusal detected.
	LevelNone RefusalLevel = iota
	// LevelUncertain means weak signals found (score == 1). May or may not be a refusal.
	LevelUncertain
	// LevelConfirmed means strong match or multiple weak signals. Definitely a refusal.
	LevelConfirmed
)

var strongPhrases = []string{
	"i cannot assist", "i can't assist", "i'm unable to assist", "i cannot help with",
	"i can't help with", "i'm unable to help", "i must decline", "i must refuse",
	"i have to decline", "against my guidelines", "against my safety policy",
	"against my policy", "violates my guidelines", "violates my policy",
	"i won't help", "i won't assist", "i'm not able to help", "as an ai",
	"as a language model", "as an ai assistant", "as a helpful assistant",
	"as an artificial intelligence", "my programming prevents", "cannot fulfill this request",

	"我无法协助", "我无法帮助", "我不能协助", "我不能帮助", "我不能做", "我无法做",
	"我不会帮", "我无法提供", "我无法完成", "我没办法", "我做不到", "违反了我的",
	"违反我的", "这违反", "违反规定", "违反政策", "违反规则", "不符合我的",
	"不符合我们的", "不符合规定", "不符合政策", "我拒绝", "我必须拒绝",
	"作为ai", "作为 ai", "作为ai助手", "作为 ai 助手", "作为语言模型",
	"人工智能无法", "作为人工智能", "我的编程限制", "我的安全准则",
	"无法满足这个请求", "无法处理这个请求", "无法回答这个问题", "无法执行这个",
	"这个请求我无法", "这个请求我不能", "这个问题我无法", "这个问题我不能",
	"请注意，我", "请理解，我", "非常抱歉，", "十分抱歉，", "实在抱歉，",
	"恕我无法", "恕我不能", "帮不了你",
}

var weakSignalGroups = [][]string{
	{"sorry", "apologize"},
	{"unfortunately"},
	{"i cannot", "i can't", "i'm unable"},
	{"抱歉", "很抱歉", "非常抱歉", "十分抱歉", "实在抱歉"},
	{"对不起", "不好意思"},
	{"很遗憾"},
	{"无法", "做不到", "没办法"},
	{"不能"},
	{"禁止", "不允许"},
	{"不符合", "违反"},
}

var safePassthroughPatterns = []string{
	"```", "import ", "func ", "def ", "class ", "package ", "namespace ",
}

var thinkingStripRegex = regexp.MustCompile(`(?si)<thinking>.*?</thinking>`)

type Detector struct {
	extraStrong []string
	extraWeak   []string
}

func NewDetector(extraStrong, extraWeak []string) *Detector {
	return &Detector{extraStrong: extraStrong, extraWeak: extraWeak}
}

// IsRefusal returns true only for confirmed refusals (strong match or score >= 2).
// Uncertain cases (score == 1) return false here. Use Analyze() to get fine-grained
// levels, then call AI verify for uncertain cases at the shield layer.
func (d *Detector) IsRefusal(text string) bool {
	return d.Analyze(text) == LevelConfirmed
}

// Analyze returns the refusal confidence level for the given text.
//
//   - LevelConfirmed: strong phrase match or score >= 2. Definitely a refusal.
//   - LevelUncertain: score == 1. Might be a refusal, AI verify recommended.
//   - LevelNone: no signals found. Safe to pass through.
func (d *Detector) Analyze(text string) RefusalLevel {
	text = strings.TrimSpace(text)
	if text == "" {
		return LevelNone
	}

	cleaned := thinkingStripRegex.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return LevelNone
	}

	lower := strings.ToLower(cleaned)

	for _, safe := range safePassthroughPatterns {
		if strings.Contains(lower, safe) {
			return LevelNone
		}
	}

	for _, phrase := range strongPhrases {
		if strings.Contains(lower, phrase) {
			return LevelConfirmed
		}
	}
	for _, phrase := range d.extraStrong {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return LevelConfirmed
		}
	}

	scanEnd := len(lower)
	if scanEnd > 150 {
		scanEnd = 150
	}
	scanArea := lower[:scanEnd]

	score := 0
	for _, group := range weakSignalGroups {
		for _, sig := range group {
			if strings.Contains(scanArea, sig) {
				score++
				break
			}
		}
	}
	for _, sig := range d.extraWeak {
		if strings.Contains(scanArea, strings.ToLower(sig)) {
			score++
		}
	}

	if score >= 2 {
		return LevelConfirmed
	}

	if len(cleaned) < 40 && score >= 1 {
		for _, group := range weakSignalGroups {
			for _, sig := range group {
				if strings.HasPrefix(lower, sig) {
					return LevelConfirmed
				}
			}
		}
	}

	if score == 1 {
		return LevelUncertain
	}

	return LevelNone
}
