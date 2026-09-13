package messaging_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/messaging"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    messaging.Target
		wantErr bool
	}{
		{
			name: "channel name",
			raw:  "#" + channelName,
			want: messaging.Target{Kind: messaging.KindChannelName, Value: channelName},
		},
		{
			name: "username",
			raw:  usernameTarget,
			want: messaging.Target{Kind: messaging.KindUsername, Value: "alice"},
		},
		{
			name: "channel id",
			raw:  channelIDTarget,
			want: messaging.Target{Kind: messaging.KindChannelID, Value: channelID},
		},
		{
			name: "user id",
			raw:  userIDTarget,
			want: messaging.Target{Kind: messaging.KindUserID, Value: "9bf2ka"},
		},
		{
			name: "surrounding whitespace from a CLI flag",
			raw:  "  #" + channelName + "  ",
			want: messaging.Target{Kind: messaging.KindChannelName, Value: channelName},
		},
		{
			name: "channel name keeps inner punctuation",
			raw:  "#team-ops.eu",
			want: messaging.Target{Kind: messaging.KindChannelName, Value: "team-ops.eu"},
		},
		// A bare word is the case the strictness exists for: it could be a
		// channel name or a channel id, and guessing posts to the wrong place.
		{name: "bare name", raw: channelName, wantErr: true},
		{name: "bare id", raw: "xj7q1w", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "sigil with no name", raw: "#", wantErr: true},
		{name: "at with no username", raw: "@", wantErr: true},
		{name: "prefix with no id", raw: "channel:", wantErr: true},
		{name: "user prefix with no id", raw: "user:", wantErr: true},
		{name: "unknown prefix", raw: "team:ops", wantErr: true},
		{name: "slack style link", raw: "<#C123|general>", wantErr: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := messaging.ParseTarget(tt.raw)

			if tt.wantErr {
				if !errors.Is(err, messaging.ErrTargetUnrecognized) {
					t.Fatalf("ParseTarget(%q) error = %v, want ErrTargetUnrecognized", tt.raw, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.raw, err)
			}

			if got != tt.want {
				t.Fatalf("ParseTarget(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestTargetStringRoundTrips checks that String renders the form a caller
// typed, since it is what error messages and logs echo back.
func TestTargetStringRoundTrips(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"#" + channelName, usernameTarget, channelIDTarget, userIDTarget} {
		target, err := messaging.ParseTarget(raw)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", raw, err)
		}

		if got := target.String(); got != raw {
			t.Errorf("ParseTarget(%q).String() = %q, want %q", raw, got, raw)
		}
	}
}
