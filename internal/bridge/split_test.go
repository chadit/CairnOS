package bridge_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/bridge/bridgetest"
)

// TestSplitMessageCountsRunes drives the paragraph-preferring split through a
// delivered turn against the recording outbound's cap. The multibyte cases are
// the point: the limit is a rune count because that is what Mattermost's
// MaxPostSize and Telegram's 4096 both are, so a reply of accented characters
// that fits must arrive as one message rather than being cut at half the text.
func TestSplitMessageCountsRunes(t *testing.T) {
	t.Parallel()

	const shortReply = "hello"

	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{
			name:  "under the cap",
			reply: shortReply,
			want:  []string{shortReply},
		},
		{
			name:  "at the cap",
			reply: strings.Repeat("a", testMaxChars),
			want:  []string{strings.Repeat("a", testMaxChars)},
		},
		{
			name:  "multibyte text at the cap stays one message",
			reply: strings.Repeat("é", testMaxChars),
			want:  []string{strings.Repeat("é", testMaxChars)},
		},
		{
			name:  "paragraph boundary",
			reply: strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 30),
			want:  []string{strings.Repeat("a", 30), strings.Repeat("b", 30)},
		},
		{
			name:  "hard cut one paragraph",
			reply: strings.Repeat("c", testMaxChars+5),
			want:  []string{strings.Repeat("c", testMaxChars), strings.Repeat("c", 5)},
		},
		{
			name:  "hard cut never splits a rune",
			reply: strings.Repeat("é", testMaxChars+5),
			want:  []string{strings.Repeat("é", testMaxChars), strings.Repeat("é", 5)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rig := newFixture(t, grantChat())
			rig.chat.SetReply(tt.reply)

			rig.handle(t, rig.message(msgGo))

			bridgetest.WaitFor(t, func() bool { return len(rig.out.Sent()) == len(tt.want) })

			if got := rig.out.Texts(); !slices.Equal(got, tt.want) {
				t.Errorf("split into %d parts, want %d", len(got), len(tt.want))
			}
		})
	}
}
