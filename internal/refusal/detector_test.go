package refusal

import "testing"

func TestIsRefusal_StrongPhrases(t *testing.T) {
	d := NewDetector(nil, nil)

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"english canonical", "I cannot assist with this request.", true},
		{"english contraction", "I can't help with that.", true},
		{"english policy", "This is against my safety policy.", true},
		{"english identity", "As an AI, I cannot do that.", true},
		{"english mid-sentence", "Well, I must decline your request here.", true},
		{"chinese canonical", "我无法协助你完成这个任务。", true},
		{"chinese identity", "作为AI助手，我不能帮助你。", true},
		{"chinese violation", "这违反了相关规定。", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsRefusal(tc.text)
			if got != tc.want {
				t.Errorf("IsRefusal(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestIsRefusal_WeakPrefixMatch(t *testing.T) {
	d := NewDetector(nil, nil)

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"sorry at start", "Sorry, I cannot do this for you.", true},
		{"apologize at start", "I apologize, but this is not possible.", true},
		{"chinese sorry start", "抱歉，我无法执行该操作。", true},
		{"chinese regret start", "很遗憾，这是做不到的。", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.IsRefusal(tc.text)
			if got != tc.want {
				t.Errorf("IsRefusal(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestIsRefusal_NormalResponses(t *testing.T) {
	d := NewDetector(nil, nil)

	cases := []struct {
		name string
		text string
	}{
		{"code response", "Here is the implementation:\n```go\nfunc main() {}\n```"},
		{"explanation with sorry mid-text", "I've analyzed the code. Sorry, there's a bug on line 10 where the nil check is missing."},
		{"import statement", "import os\nimport sys\n\ndef main():\n    pass"},
		{"normal helpful response", "Sure! Let me help you with that. The function should return a boolean value."},
		{"go function", "func handleRequest(w http.ResponseWriter, r *http.Request) {\n\tw.WriteHeader(200)\n}"},
		{"chinese normal", "好的，让我来分析一下这段代码。问题出在第15行。"},
		{"sorry in middle of long text", "The configuration file has several issues. Sorry about the confusion earlier, but the real problem is the missing semicolon on line 42."},
		{"class definition", "class MyHandler:\n    def __init__(self):\n        pass"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d.IsRefusal(tc.text) {
				t.Errorf("IsRefusal(%q) = true, want false (false positive)", tc.text)
			}
		})
	}
}

func TestIsRefusal_ThinkingStripping(t *testing.T) {
	d := NewDetector(nil, nil)

	// Model thinks about refusing but ultimately provides a helpful answer.
	text := `<thinking>
I'm not sure if I should help with this. This might violate my policy.
Let me think about this carefully... I must decline... no wait, this is fine.
</thinking>
Here is the security analysis you requested. The vulnerability is in the auth module.`

	if d.IsRefusal(text) {
		t.Error("should not flag as refusal when thinking contains refusal but answer is helpful")
	}

	// Model thinks and then actually refuses.
	text2 := `<thinking>
This seems like a harmful request.
</thinking>
I'm sorry, but I cannot assist with this request as it violates my guidelines.`

	if !d.IsRefusal(text2) {
		t.Error("should flag as refusal when answer text is a refusal")
	}
}

func TestIsRefusal_EmptyAndWhitespace(t *testing.T) {
	d := NewDetector(nil, nil)

	if d.IsRefusal("") {
		t.Error("empty string should not be refusal")
	}
	if d.IsRefusal("   \n\t  ") {
		t.Error("whitespace-only string should not be refusal")
	}
}

func TestIsRefusal_OnlyThinkingBlock(t *testing.T) {
	d := NewDetector(nil, nil)

	text := `<thinking>I cannot assist with this harmful request.</thinking>`
	if d.IsRefusal(text) {
		t.Error("text that is only a thinking block should not be flagged")
	}
}

func TestIsRefusal_ExtraPatterns(t *testing.T) {
	d := NewDetector(
		[]string{"custom refusal phrase"},
		[]string{"custom weak"},
	)

	if !d.IsRefusal("This contains a custom refusal phrase in the middle.") {
		t.Error("extra strong pattern should match anywhere")
	}
	if !d.IsRefusal("Custom weak start of response") {
		t.Error("extra weak pattern should match in prefix")
	}
	if d.IsRefusal("Some long text that eventually says custom weak at the end, way past the prefix window.") {
		t.Error("extra weak pattern should not match outside prefix window")
	}
}
