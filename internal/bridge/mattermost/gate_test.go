package mattermost_test

import (
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/bridge/mattermost"
)

// Prop names and the value Mattermost writes for a boolean prop, shared with
// the fixture assertions in ws_test.go.
const (
	propFromBot     = "from_bot"
	propFromWebhook = "from_webhook"
	propActivateAI  = "activate_ai"
	propTrue        = "true"
)

func TestMentionsInText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		message  string
		username string
		want     bool
	}{
		{name: "plain mention", message: "@cairn what broke?", username: botUsername, want: true},
		{name: "mention mid sentence", message: "hey @cairn what broke?", username: botUsername, want: true},
		{name: "case folded", message: "hey @Cairn what broke?", username: botUsername, want: true},
		{name: "trailing punctuation", message: "thanks @cairn!", username: botUsername, want: true},
		{name: "trailing period", message: "thanks @cairn.", username: botUsername, want: true},
		{name: "newline boundary", message: "context first\n@cairn go", username: botUsername, want: true},
		{name: "no mention", message: "nothing to see", username: botUsername, want: false},
		{name: "longer username", message: "@cairnbot is a different account", username: botUsername, want: false},
		{name: "hyphenated sibling username", message: "@cairn-bot is a different account", username: botUsername, want: false},
		{name: "dotted sibling username", message: "@cairn.ops is a different account", username: botUsername, want: false},
		{name: "hyphenated username matches itself", message: "@cairn-bot what broke?", username: "cairn-bot", want: true},
		{name: "dotted username matches itself", message: "@cairn.ops what broke?", username: "cairn.ops", want: true},
		{name: "email address", message: "mail ops@cairn about it", username: botUsername, want: false},
		{name: "inside inline code", message: "run `@cairn help` to see this", username: botUsername, want: false},
		{name: "inside a fence", message: "```\n@cairn help\n```", username: botUsername, want: false},
		{name: "inside a tagged fence", message: "```sh\ncurl -d '@cairn hi'\n```", username: botUsername, want: false},
		{name: "inside a tilde fence", message: "~~~\n@cairn restart prod\n~~~", username: botUsername, want: false},
		{name: "inside an indented block", message: "example:\n\n    @cairn restart prod", username: botUsername, want: false},
		{name: "inside a tab indented block", message: "example:\n\n\t@cairn restart prod", username: botUsername, want: false},
		{name: "indented continuation line is not code", message: "ping\n    @cairn restart prod", username: botUsername, want: true},
		{name: "outside a closed fence", message: "```\ncode\n```\nand @cairn look", username: botUsername, want: true},
		{name: "after an unterminated fence", message: "```\n@cairn swallowed", username: botUsername, want: false},
		{name: "stray backtick keeps the mention", message: "What does ` mean, @cairn?", username: botUsername, want: true},
		{name: "fenced mention with a hyphenated sibling", message: "@cairn-bot see ```@cairn restart prod```", username: botUsername, want: false},
		{name: "between two inline spans", message: "`a` @cairn `b`", username: botUsername, want: true},
		{name: "no username", message: "@cairn hello", username: "", want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := mattermost.MentionsInText(testCase.message, testCase.username)
			if got != testCase.want {
				t.Fatalf("MentionsInText(%q, %q) = %v, want %v", testCase.message, testCase.username, got, testCase.want)
			}
		})
	}
}

func TestStripMention(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		message  string
		username string
		want     string
	}{
		{name: "leading mention", message: "@cairn summarize this", username: botUsername, want: "summarize this"},
		{name: "case folded", message: "@Cairn summarize this", username: botUsername, want: "summarize this"},
		{name: "leading whitespace", message: "  @cairn go", username: botUsername, want: "go"},
		{name: "only the mention", message: "@cairn", username: botUsername, want: ""},
		{name: "one space only", message: "@cairn   padded", username: botUsername, want: "  padded"},
		{name: "newline after the mention", message: "@cairn\ndo the thing", username: botUsername, want: "\ndo the thing"},
		{name: "mention not first", message: "hey @cairn go", username: botUsername, want: "hey @cairn go"},
		{name: "longer username", message: "@cairnbot go", username: botUsername, want: "@cairnbot go"},
		{
			name:     "hyphenated sibling username keeps the message whole",
			message:  "@ops-team can @ops summarize last night?",
			username: "ops",
			want:     "@ops-team can @ops summarize last night?",
		},
		{name: "dotted sibling username", message: "@cairn.ops go", username: botUsername, want: "@cairn.ops go"},
		{name: "hyphenated username strips itself", message: "@cairn-bot go", username: "cairn-bot", want: "go"},
		{name: "no mention", message: "plain text", username: botUsername, want: "plain text"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := mattermost.StripMention(testCase.message, testCase.username); got != testCase.want {
				t.Fatalf("StripMention(%q, %q) = %q, want %q", testCase.message, testCase.username, got, testCase.want)
			}
		})
	}
}

func TestStripMentionKeepsMessageWithoutAUsername(t *testing.T) {
	t.Parallel()

	const message = "@cairn go"

	if got := mattermost.StripMention(message, ""); got != message {
		t.Fatalf("StripMention with no username = %q, want %q", got, message)
	}
}

func TestIsAutomated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		props map[string]string
		user  *mattermost.User
		name  string
		want  bool
	}{
		{name: "human post", want: false},
		{name: "from_bot", props: map[string]string{propFromBot: propTrue}, want: true},
		{name: "from_webhook", props: map[string]string{propFromWebhook: propTrue}, want: true},
		{name: "from_plugin", props: map[string]string{"from_plugin": propTrue}, want: true},
		{name: "from_oauth_app", props: map[string]string{"from_oauth_app": propTrue}, want: true},
		{name: "prop set to false", props: map[string]string{propFromBot: "false"}, want: false},
		{name: "unrelated prop", props: map[string]string{"override_username": "ci"}, want: false},
		{name: "bot user no props", user: &mattermost.User{ID: "u1", IsBot: true}, want: true},
		{name: "human user no props", user: &mattermost.User{ID: "u1"}, want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			post := &mattermost.Post{ID: "p1", Props: testCase.props}

			if got := mattermost.IsAutomated(post, testCase.user); got != testCase.want {
				t.Fatalf("IsAutomated(props=%v, user=%v) = %v, want %v", testCase.props, testCase.user, got, testCase.want)
			}
		})
	}
}

func TestIsAutomatedHandlesNilPost(t *testing.T) {
	t.Parallel()

	if mattermost.IsAutomated(nil, nil) {
		t.Fatal("IsAutomated(nil, nil) = true, want false")
	}

	if !mattermost.IsAutomated(nil, &mattermost.User{IsBot: true}) {
		t.Fatal("IsAutomated(nil, bot) = false, want true: the user alone settles it")
	}
}

func TestActivateAI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		props map[string]string
		name  string
		want  bool
	}{
		{name: "opted in", props: map[string]string{propActivateAI: propTrue}, want: true},
		{name: "opted out", props: map[string]string{propActivateAI: "false"}, want: false},
		{name: "absent", props: map[string]string{propFromBot: propTrue}, want: false},
		{name: "no props", want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := mattermost.ActivateAI(&mattermost.Post{Props: testCase.props}); got != testCase.want {
				t.Fatalf("ActivateAI(%v) = %v, want %v", testCase.props, got, testCase.want)
			}
		})
	}

	if mattermost.ActivateAI(nil) {
		t.Fatal("ActivateAI(nil) = true, want false")
	}
}

func TestLoopLimiterTripsAtMaxAndServesCooldown(t *testing.T) {
	t.Parallel()

	const (
		maxEvents = 3
		window    = time.Minute
		cooldown  = 10 * time.Minute
	)

	limiter := mattermost.NewLoopLimiter(mattermost.LoopLimitConfig{
		MaxEvents: maxEvents, Window: window, Cooldown: cooldown,
	})

	start := time.Date(2026, time.September, 12, 9, 0, 0, 0, time.UTC)

	for i := range maxEvents {
		at := start.Add(time.Duration(i) * time.Second)
		if !limiter.Admit("ops:thread", at) {
			t.Fatalf("admission %d refused, want the first %d admitted", i+1, maxEvents)
		}
	}

	tripped := start.Add(maxEvents * time.Second)
	if limiter.Admit("ops:thread", tripped) {
		t.Fatal("admission past the max was allowed, want the window to trip")
	}

	stillCooling := tripped.Add(cooldown - time.Second)
	if limiter.Admit("ops:thread", stillCooling) {
		t.Fatal("admission inside the cooldown was allowed")
	}

	recovered := tripped.Add(cooldown + time.Second)
	if !limiter.Admit("ops:thread", recovered) {
		t.Fatal("admission after the cooldown was refused")
	}
}

func TestLoopLimiterForgetsAdmissionsOutsideTheWindow(t *testing.T) {
	t.Parallel()

	const window = time.Minute

	limiter := mattermost.NewLoopLimiter(mattermost.LoopLimitConfig{MaxEvents: 2, Window: window, Cooldown: time.Hour})

	start := time.Date(2026, time.September, 12, 9, 0, 0, 0, time.UTC)

	if !limiter.Admit("ops:thread", start) || !limiter.Admit("ops:thread", start.Add(time.Second)) {
		t.Fatal("the first two admissions were refused")
	}

	// Both earlier admissions have aged out, so the conversation starts over
	// instead of tripping.
	if !limiter.Admit("ops:thread", start.Add(window+time.Second)) {
		t.Fatal("admission after the window aged out was refused")
	}
}

func TestLoopLimiterKeepsConversationsApart(t *testing.T) {
	t.Parallel()

	limiter := mattermost.NewLoopLimiter(mattermost.LoopLimitConfig{MaxEvents: 1, Window: time.Minute, Cooldown: time.Hour})

	now := time.Date(2026, time.September, 12, 9, 0, 0, 0, time.UTC)

	if !limiter.Admit("ops:thread", now) {
		t.Fatal("the first admission on the first key was refused")
	}

	if limiter.Admit("ops:thread", now) {
		t.Fatal("the second admission on the first key was allowed")
	}

	if !limiter.Admit("alerts:thread", now) {
		t.Fatal("a different conversation was refused by the first one's cooldown")
	}
}

func TestLoopLimiterDefaultsAdmitTwentyPerWindow(t *testing.T) {
	t.Parallel()

	limiter := mattermost.NewLoopLimiter(mattermost.LoopLimitConfig{})

	now := time.Date(2026, time.September, 12, 9, 0, 0, 0, time.UTC)

	const defaultMax = 20

	for i := range defaultMax {
		if !limiter.Admit("key", now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("default limiter refused admission %d", i+1)
		}
	}

	if limiter.Admit("key", now.Add(defaultMax*time.Second)) {
		t.Fatal("default limiter admitted a twenty-first post inside the window")
	}
}
