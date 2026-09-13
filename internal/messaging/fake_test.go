package messaging_test

import (
	"context"

	"github.com/chadit/CairnOS/internal/messaging"
)

// Fixture values the package's tests share. They are constants because the
// same identity, server, and target appear in the parse, registry, and service
// tests, and a drifting copy would make two tests disagree about one send.
const (
	transportMattermost = "mattermost"
	identityName        = "ops-bot"
	serverURL           = "https://mm.example"
	channelName         = "general"
	usernameTarget      = "@alice"
	userIDTarget        = "user:9bf2ka"
	channelIDTarget     = "channel:c1"
	messageText         = "hi"
	postID              = "p1"
	channelID           = "c1"
)

// fakeSender is the second implementation of messaging.Sender: the Mattermost
// identity is the first, and this one substitutes for it in the registry and
// service tests, which is what keeps the interface earning its place.
//
// It records each send so a test can assert the service passed the parsed
// target and the text through unmodified, and fails with sendErr when set.
type fakeSender struct {
	identity messaging.Identity
	ref      messaging.PostRef
	sendErr  error

	calls []fakeSend
}

// fakeSend is one recorded call to fakeSender.Send.
type fakeSend struct {
	target   messaging.Target
	text     string
	threadID string
}

func (f *fakeSender) Send(_ context.Context, to messaging.Target, text, threadID string) (messaging.PostRef, error) {
	f.calls = append(f.calls, fakeSend{target: to, text: text, threadID: threadID})

	if f.sendErr != nil {
		return messaging.PostRef{}, f.sendErr
	}

	return f.ref, nil
}

func (f *fakeSender) Describe() messaging.Identity {
	return f.identity
}

// fakeMetrics records the labels the service reports, so a test can prove a
// failed send is counted as an error rather than silently dropped.
type fakeMetrics struct {
	sends []fakeMetricSend
}

// fakeMetricSend is one recorded MessagingSend call.
type fakeMetricSend struct {
	identity string
	outcome  string
}

func (m *fakeMetrics) MessagingSend(_ context.Context, identity, outcome string) {
	m.sends = append(m.sends, fakeMetricSend{identity: identity, outcome: outcome})
}
