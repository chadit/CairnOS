package builtins_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Param keys and fixture values every message case addresses. textKey and
// testBody are already declared beside the mcp_client and email cases.
const (
	identityKey    = "identity"
	threadKey      = "thread"
	testIdentity   = "ops"
	testChannel    = "#alerts"
	testMessagePID = "post-42"
)

// errMessageTransport is what recordingSender returns once a test makes the
// send fail for an unnamed reason.
var errMessageTransport = errors.New("connection reset")

// sentMessage is one send the recorder accepted.
type sentMessage struct {
	Identity string
	To       string
	Text     string
	Thread   string
}

// recordingSender is a builtins.MessageSender that keeps every send and can fail
// with either a classified runner error or a bare transport error.
type recordingSender struct {
	fail   error
	postID string
	sent   []sentMessage
}

// Send records the call and answers with the configured failure or post id.
func (s *recordingSender) Send(_ context.Context, identity, to, text, thread string) (string, error) {
	s.sent = append(s.sent, sentMessage{Identity: identity, To: to, Text: text, Thread: thread})

	if s.fail != nil {
		return "", s.fail
	}

	return s.postID, nil
}

// messageTask builds a resolved task over the given params.
func messageTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: "run-1", ActionName: "notify", Params: params, Resolved: true}
}

// TestMessageSendsThroughSeam proves a well-formed action reaches the send seam
// with its four params intact and returns the created message id as the output,
// so a later action can answer in the same thread (FR32).
func TestMessageSendsThroughSeam(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{postID: testMessagePID}

	built, err := builtins.MessageFactory(sender)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	result, err := built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity,
		toKey:       testChannel,
		textKey:     "deploy finished",
		threadKey:   "root-7",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Output != testMessagePID {
		t.Fatalf("output = %v, want the created message id", result.Output)
	}

	want := sentMessage{Identity: testIdentity, To: testChannel, Text: "deploy finished", Thread: "root-7"}
	if len(sender.sent) != 1 || sender.sent[0] != want {
		t.Fatalf("sends = %+v, want exactly %+v", sender.sent, want)
	}
}

// TestMessageOmitsThreadWhenAbsent proves thread is optional and reaches the
// seam empty rather than blocking the send.
func TestMessageOmitsThreadWhenAbsent(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{postID: "post-1"}

	built, err := builtins.MessageFactory(sender)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	if _, err := built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity, toKey: "@alice", textKey: "ping",
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(sender.sent) != 1 || sender.sent[0].Thread != "" {
		t.Fatalf("sends = %+v, want one send with no thread", sender.sent)
	}
}

// TestMessageValidateRejectsBadParams proves each required param is checked and
// that a non-string value is refused, so a workflow fails at load rather than at
// the first fire.
func TestMessageValidateRejectsBadParams(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params   map[string]any
		name     string
		wantCode string
	}{
		{
			name:     "missing identity",
			params:   map[string]any{toKey: testChannel, textKey: testBody},
			wantCode: "message.missing_identity",
		},
		{
			name:     "missing to",
			params:   map[string]any{identityKey: testIdentity, textKey: testBody},
			wantCode: "message.missing_to",
		},
		{
			name:     "missing text",
			params:   map[string]any{identityKey: testIdentity, toKey: testChannel},
			wantCode: "message.missing_text",
		},
		{
			name: "non-string thread",
			params: map[string]any{
				identityKey: testIdentity, toKey: testChannel, textKey: testBody, threadKey: 7,
			},
			wantCode: "message.bad_thread",
		},
	}

	built, err := builtins.MessageFactory(&recordingSender{})(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := built.Validate(messageTask(testCase.params))

			classified, ok := errors.AsType[*runner.Error](err)
			if !ok {
				t.Fatalf("Validate error = %v, want a classified runner error", err)
			}

			if classified.Code != testCase.wantCode {
				t.Fatalf("code = %q, want %q", classified.Code, testCase.wantCode)
			}

			if classified.Class != runner.ClassPermanent {
				t.Fatalf("class = %v, want permanent", classified.Class)
			}
		})
	}
}

// TestMessageWithoutSenderIsPermanent proves the one-shot hosts refuse clearly:
// a nil seam fails with a permanent error naming the host that can send, rather
// than pretending the message went out.
func TestMessageWithoutSenderIsPermanent(t *testing.T) {
	t.Parallel()

	built, err := builtins.MessageFactory(nil)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	_, err = built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity, toKey: testChannel, textKey: testBody,
	}))

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("Execute error = %v, want a classified runner error", err)
	}

	if classified.Code != "message.unavailable" || classified.Class != runner.ClassPermanent {
		t.Fatalf("error = %+v, want a permanent message.unavailable", classified)
	}

	if !strings.Contains(classified.Message, "cairnos serve") {
		t.Fatalf("message %q does not name the host that can send", classified.Message)
	}
}

// TestMessageKeepsSeamClassification proves a classification the send seam
// already made survives, and that an unclassified transport failure is reported
// transient so the engine may retry an idempotent action.
func TestMessageKeepsSeamClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fail      error
		name      string
		wantCode  string
		wantClass runner.ErrorClass
	}{
		{
			name:      "seam classification passes through",
			fail:      runner.NewError(runner.ClassPermanent, "message.identity_not_found", "no such identity"),
			wantCode:  "message.identity_not_found",
			wantClass: runner.ClassPermanent,
		},
		{
			name:      "bare transport failure is transient",
			fail:      errMessageTransport,
			wantCode:  "message.send",
			wantClass: runner.ClassTransient,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			built, err := builtins.MessageFactory(&recordingSender{fail: testCase.fail})(nil)
			if err != nil {
				t.Fatalf("build message runner: %v", err)
			}

			_, err = built.Execute(t.Context(), messageTask(map[string]any{
				identityKey: testIdentity, toKey: testChannel, textKey: testBody,
			}))

			classified, ok := errors.AsType[*runner.Error](err)
			if !ok {
				t.Fatalf("Execute error = %v, want a classified runner error", err)
			}

			if classified.Code != testCase.wantCode || classified.Class != testCase.wantClass {
				t.Fatalf("error = %+v, want code %q class %v",
					classified, testCase.wantCode, testCase.wantClass)
			}
		})
	}
}
