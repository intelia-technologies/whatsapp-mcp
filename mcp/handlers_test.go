package mcp

import "testing"

func TestValidateReaction(t *testing.T) {
	accepted := []string{
		"",              // removes an existing reaction
		"\U0001F44D",    // 👍
		"\U0001F37B",    // 🍻
		"\u2764\ufe0f",  // ❤️ with variation selector
		"1\ufe0f\u20e3", // 1️⃣ keycap: ASCII digit plus combining marks
		"\U0001F468\u200D\U0001F469\u200D\U0001F467", // 👨‍👩‍👧 ZWJ family
	}
	for _, emoji := range accepted {
		if err := validateReaction(emoji); err != nil {
			t.Errorf("validateReaction(%q) = %v, want nil", emoji, err)
		}
	}

	rejected := []string{
		"ok",
		":)",
		"1",
		"\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D\U0001F44D",
	}
	for _, emoji := range rejected {
		if err := validateReaction(emoji); err == nil {
			t.Errorf("validateReaction(%q) = nil, want error", emoji)
		}
	}
}
