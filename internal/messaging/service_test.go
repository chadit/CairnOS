package messaging_test

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/messaging"
)

// newServiceFixture wires a service over a registry holding one fake sender,
// returning the service and the metrics recorder so a test can assert on what
// telemetry saw. The caller keeps its own reference to the sender.
func newServiceFixture(t *testing.T, sender *fakeSender) (*messaging.Service, *fakeMetrics) {
	t.Helper()

	registry := messaging.NewRegistry()
	registry.Add(identityName, sender)

	metrics := &fakeMetrics{}

	return messaging.NewService(registry, metrics), metrics
}

// mentionText carries an @here mention, which has to reach the sender as sent.
const (
	mentionText  = "deploy finished, @here"
	threadRootID = "root-1"
)

func TestSendMessagePostsThroughTheSender(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{
		identity: messaging.Identity{Name: identityName, Transport: transportMattermost},
		ref:      messaging.PostRef{PostID: postID, ChannelID: channelID},
	}

	service, metrics := newServiceFixture(t, sender)

	resp, err := service.SendMessage(t.Context(), &cairnosv1.MessagingServiceSendMessageRequest{
		Identity: identityName,
		To:       "#" + channelName,
		Text:     mentionText,
		ThreadId: threadRootID,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if resp.GetPostId() != postID || resp.GetChannelId() != channelID {
		t.Fatalf("SendMessage returned post %q channel %q, want %s/%s",
			resp.GetPostId(), resp.GetChannelId(), postID, channelID)
	}

	if len(sender.calls) != 1 {
		t.Fatalf("sender saw %d calls, want 1", len(sender.calls))
	}

	got := sender.calls[0]
	want := fakeSend{
		target:   messaging.Target{Kind: messaging.KindChannelName, Value: channelName},
		text:     mentionText,
		threadID: threadRootID,
	}

	// The mention has to arrive unmodified; Mattermost is what renders it,
	// so any rewriting here would silently break notifications.
	if got != want {
		t.Fatalf("sender saw %+v, want %+v", got, want)
	}

	assertMetric(t, metrics, "ok")
}

func TestSendMessageRejectsBadRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		req  *cairnosv1.MessagingServiceSendMessageRequest
		name string
		want codes.Code
	}{
		{
			name: "unknown identity",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{Identity: "nope", To: "#" + channelName, Text: messageText},
			want: codes.NotFound,
		},
		{
			name: "missing identity",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{To: "#" + channelName, Text: messageText},
			want: codes.InvalidArgument,
		},
		{
			name: "bare target",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{Identity: identityName, To: channelName, Text: messageText},
			want: codes.InvalidArgument,
		},
		{
			name: "empty target",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{Identity: identityName, Text: messageText},
			want: codes.InvalidArgument,
		},
		{
			name: "empty text",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{Identity: identityName, To: "#" + channelName},
			want: codes.InvalidArgument,
		},
		{
			name: "whitespace-only text",
			req:  &cairnosv1.MessagingServiceSendMessageRequest{Identity: identityName, To: "#" + channelName, Text: "  \n "},
			want: codes.InvalidArgument,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sender := &fakeSender{identity: messaging.Identity{Name: identityName}}
			service, metrics := newServiceFixture(t, sender)

			if _, err := service.SendMessage(t.Context(), tt.req); status.Code(err) != tt.want {
				t.Fatalf("SendMessage error = %v (code %s), want %s", err, status.Code(err), tt.want)
			}

			// A rejected request never reached a transport, so counting it
			// would read as a delivery failure on the dashboard.
			if len(sender.calls) != 0 {
				t.Errorf("sender saw %d calls for a rejected request, want 0", len(sender.calls))
			}

			if len(metrics.sends) != 0 {
				t.Errorf("telemetry recorded %+v for a rejected request, want nothing", metrics.sends)
			}
		})
	}
}

func TestSendMessageMapsSenderFailure(t *testing.T) {
	t.Parallel()

	// A transport reports its failures as canonical errors, so the service has
	// to pass the code through rather than flatten everything to Internal.
	sender := &fakeSender{
		identity: messaging.Identity{Name: identityName},
		sendErr:  cairnerr.New("runner.rate_limited", "mattermost returned 429"),
	}

	service, metrics := newServiceFixture(t, sender)

	_, err := service.SendMessage(t.Context(), &cairnosv1.MessagingServiceSendMessageRequest{
		Identity: identityName,
		To:       userIDTarget,
		Text:     messageText,
	})

	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("SendMessage error = %v (code %s), want ResourceExhausted", err, got)
	}

	assertMetric(t, metrics, "error")
}

func TestSendMessageReportsAnOpaqueSenderFailure(t *testing.T) {
	t.Parallel()

	sender := &fakeSender{
		identity: messaging.Identity{Name: identityName},
		sendErr:  errors.New("dial tcp: connection refused"),
	}

	service, metrics := newServiceFixture(t, sender)

	_, err := service.SendMessage(t.Context(), &cairnosv1.MessagingServiceSendMessageRequest{
		Identity: identityName,
		To:       channelIDTarget,
		Text:     messageText,
	})

	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("SendMessage error = %v (code %s), want Internal", err, got)
	}

	assertMetric(t, metrics, "error")
}

func TestListIdentitiesDescribesTheRegistry(t *testing.T) {
	t.Parallel()

	want := messaging.Identity{
		Name:        identityName,
		Transport:   transportMattermost,
		Username:    botUsername,
		DisplayName: botDisplayName,
		URL:         serverURL,
	}

	service, _ := newServiceFixture(t, &fakeSender{identity: want})

	resp, err := service.ListIdentities(t.Context(), &cairnosv1.ListIdentitiesRequest{})
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}

	if len(resp.GetIdentities()) != 1 {
		t.Fatalf("ListIdentities returned %d identities, want 1", len(resp.GetIdentities()))
	}

	got := resp.GetIdentities()[0]
	if got.GetName() != want.Name || got.GetTransport() != want.Transport ||
		got.GetUsername() != want.Username || got.GetDisplayName() != want.DisplayName ||
		got.GetUrl() != want.URL {
		t.Fatalf("ListIdentities returned %+v, want %+v", got, want)
	}
}

// TestSendMessageWithoutMetrics covers serve running with telemetry off, where
// a nil recorder must not panic the send path.
func TestSendMessageWithoutMetrics(t *testing.T) {
	t.Parallel()

	registry := messaging.NewRegistry()
	registry.Add(identityName, &fakeSender{
		identity: messaging.Identity{Name: identityName},
		ref:      messaging.PostRef{PostID: postID, ChannelID: channelID},
	})

	service := messaging.NewService(registry, nil)

	if _, err := service.SendMessage(t.Context(), &cairnosv1.MessagingServiceSendMessageRequest{
		Identity: identityName,
		To:       usernameTarget,
		Text:     messageText,
	}); err != nil {
		t.Fatalf("SendMessage with no metrics recorder: %v", err)
	}
}

// assertMetric checks that exactly one send was counted, under the identity
// that posted and the expected outcome label.
func assertMetric(t *testing.T, metrics *fakeMetrics, outcome string) {
	t.Helper()

	want := fakeMetricSend{identity: identityName, outcome: outcome}

	if len(metrics.sends) != 1 {
		t.Fatalf("telemetry recorded %+v, want one %+v", metrics.sends, want)
	}

	if metrics.sends[0] != want {
		t.Fatalf("telemetry recorded %+v, want %+v", metrics.sends[0], want)
	}
}
