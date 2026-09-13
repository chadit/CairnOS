package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/notify"
)

// Fixture values the Mattermost sink cases share.
const (
	sinkIdentity = "ops-bot"
	sinkChannel  = "#alerts"
)

// errSendRefused stands in for a transport-level send failure.
var errSendRefused = errors.New("mattermost refused the post")

// fakeSender is a messaging.Sender that records the one send the notify sink
// makes, so a test can prove the sink passed the parsed channel and the line
// through unmodified.
type fakeSender struct {
	sendErr error
	sends   []fakeSend
}

// fakeSend is one recorded call to fakeSender.Send.
type fakeSend struct {
	target   messaging.Target
	text     string
	threadID string
}

func (f *fakeSender) Send(_ context.Context, to messaging.Target, text, threadID string) (messaging.PostRef, error) {
	f.sends = append(f.sends, fakeSend{target: to, text: text, threadID: threadID})

	if f.sendErr != nil {
		return messaging.PostRef{}, f.sendErr
	}

	return messaging.PostRef{PostID: "p1", ChannelID: "c1"}, nil
}

func (*fakeSender) Describe() messaging.Identity {
	return messaging.Identity{Name: sinkIdentity, Transport: "mattermost"}
}

func TestMattermostSinkPostsThroughTheRegisteredIdentity(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{}
	registry := messaging.NewRegistry()
	registry.Add(sinkIdentity, sender)

	sink := notify.MattermostSink(registry, sinkIdentity, sinkChannel)

	if err := sink.Post(t.Context(), lineHeld); err != nil {
		t.Fatalf("Post: %v", err)
	}

	if len(sender.sends) != 1 {
		t.Fatalf("sender saw %d sends, want 1", len(sender.sends))
	}

	got := sender.sends[0]

	if got.text != lineHeld {
		t.Errorf("sent text %q, want the fan-out line %q", got.text, lineHeld)
	}

	if got.target.Kind != messaging.KindChannelName || got.target.Value != "alerts" {
		t.Errorf("sent to %#v, want the parsed channel name from %q", got.target, sinkChannel)
	}

	if got.threadID != "" {
		t.Errorf("sent thread id %q; a notification is a new post, never a reply", got.threadID)
	}
}

func TestMattermostSinkResolvesTheIdentityAtPostTime(t *testing.T) {
	t.Parallel()

	// The bridge registers its identities after serve builds the fan-out, so a
	// sink built over an empty registry has to start working once the identity
	// lands rather than capturing a sender that does not exist yet.
	registry := messaging.NewRegistry()
	sink := notify.MattermostSink(registry, sinkIdentity, sinkChannel)

	err := sink.Post(t.Context(), lineHeld)
	if err == nil {
		t.Fatal("Post before the identity registered succeeded; want NotFound")
	}

	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("Post returned code %s, want %s", code, codes.NotFound)
	}

	if msg := status.Convert(err).Message(); !strings.Contains(msg, sinkIdentity) {
		t.Errorf("error %q does not name the missing identity %q", msg, sinkIdentity)
	}

	sender := &fakeSender{}
	registry.Add(sinkIdentity, sender)

	if err := sink.Post(t.Context(), lineHeld); err != nil {
		t.Fatalf("Post after the identity registered: %v", err)
	}

	if len(sender.sends) != 1 {
		t.Fatalf("late-registered sender saw %d sends, want 1", len(sender.sends))
	}
}

func TestMattermostSinkReportsAFailedSend(t *testing.T) {
	t.Parallel()

	registry := messaging.NewRegistry()
	registry.Add(sinkIdentity, &fakeSender{sendErr: errSendRefused})

	sink := notify.MattermostSink(registry, sinkIdentity, sinkChannel)

	err := sink.Post(t.Context(), lineHeld)
	if !errors.Is(err, errSendRefused) {
		t.Fatalf("Post returned %v, want the transport error wrapped", err)
	}
}

func TestMattermostSinkRejectsAnUnparseableChannel(t *testing.T) {
	t.Parallel()

	// A bare name is ambiguous between a channel name and a channel id, so the
	// sink refuses it rather than guessing where the alert lands.
	registry := messaging.NewRegistry()
	registry.Add(sinkIdentity, &fakeSender{})

	sink := notify.MattermostSink(registry, sinkIdentity, "alerts")

	err := sink.Post(t.Context(), lineHeld)
	if !errors.Is(err, messaging.ErrTargetUnrecognized) {
		t.Fatalf("Post returned %v, want ErrTargetUnrecognized", err)
	}
}
