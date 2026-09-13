package messaging

import (
	"fmt"
	"strings"
)

// TargetKind says how a Target's value addresses a destination. The values are
// strings so a log line or a test failure reads as the form the caller typed.
type TargetKind string

// The four target kinds, one per accepted input form (FR27).
const (
	KindChannelName TargetKind = "channel_name"
	KindChannelID   TargetKind = "channel_id"
	KindUsername    TargetKind = "username"
	KindUserID      TargetKind = "user_id"
)

// The sigils and prefixes that select a kind.
const (
	sigilChannelName = "#"
	sigilUsername    = "@"
	prefixChannelID  = "channel:"
	prefixUserID     = "user:"
)

// Target is a parsed destination. Value never carries the sigil or prefix that
// selected the kind, so a Sender uses it as an id or a name directly.
type Target struct {
	Kind  TargetKind
	Value string
}

// ParseTarget reads one of the four accepted target forms. Everything else,
// including a bare id, is ErrTargetUnrecognized: a bare "general" is both a
// plausible channel name and a plausible channel id, and the wrong guess posts
// to the wrong place (FR27).
func ParseTarget(raw string) (Target, error) {
	kind, value, ok := splitTarget(strings.TrimSpace(raw))
	if !ok || value == "" {
		return Target{}, fmt.Errorf("%w: %q", ErrTargetUnrecognized, raw)
	}

	return Target{Kind: kind, Value: value}, nil
}

// splitTarget strips the sigil or prefix that selects a kind, reporting false
// when the input carries none of them.
func splitTarget(trimmed string) (TargetKind, string, bool) {
	if value, found := strings.CutPrefix(trimmed, sigilChannelName); found {
		return KindChannelName, value, true
	}

	if value, found := strings.CutPrefix(trimmed, sigilUsername); found {
		return KindUsername, value, true
	}

	if value, found := strings.CutPrefix(trimmed, prefixChannelID); found {
		return KindChannelID, value, true
	}

	if value, found := strings.CutPrefix(trimmed, prefixUserID); found {
		return KindUserID, value, true
	}

	return "", "", false
}

// String renders a target back into the form a caller would type, so error
// messages and logs echo the input rather than an internal spelling.
func (t Target) String() string {
	switch t.Kind {
	case KindChannelName:
		return sigilChannelName + t.Value
	case KindUsername:
		return sigilUsername + t.Value
	case KindChannelID:
		return prefixChannelID + t.Value
	case KindUserID:
		return prefixUserID + t.Value
	default:
		return t.Value
	}
}
