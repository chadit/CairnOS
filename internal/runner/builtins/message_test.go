package builtins_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
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
	testRootID     = "root-7"
	threadKeyParam = "thread_key"
	testThreadKey  = "usage:build_box"
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

// recordingSender keeps every send and fails with the configured error. A reply
// on goneRoot fails the way serve reports a root the chat server deleted.
type recordingSender struct {
	fail     error
	postID   string
	goneRoot string
	sent     []sentMessage
}

// Send records the call and answers with the configured failure or post id.
func (s *recordingSender) Send(_ context.Context, identity, to, text, thread string) (string, error) {
	s.sent = append(s.sent, sentMessage{Identity: identity, To: to, Text: text, Thread: thread})

	if s.fail != nil {
		return "", s.fail
	}

	if thread != "" && thread == s.goneRoot {
		return "", fmt.Errorf("reply on %s: %w", thread, builtins.ErrMessageThreadMissing)
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

	built, err := builtins.MessageFactory(sender, nil)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	result, err := built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity,
		toKey:       testChannel,
		textKey:     testBody,
		threadKey:   testRootID,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Output != testMessagePID {
		t.Fatalf("output = %v, want the created message id", result.Output)
	}

	want := sentMessage{Identity: testIdentity, To: testChannel, Text: testBody, Thread: testRootID}
	if len(sender.sent) != 1 || sender.sent[0] != want {
		t.Fatalf("sends = %+v, want exactly %+v", sender.sent, want)
	}
}

// TestMessageOmitsThreadWhenAbsent proves thread is optional and reaches the
// seam empty rather than blocking the send.
func TestMessageOmitsThreadWhenAbsent(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{postID: testMessagePID}

	built, err := builtins.MessageFactory(sender, nil)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	if _, err := built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity, toKey: "@alice", textKey: testBody,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(sender.sent) != 1 || sender.sent[0].Thread != "" {
		t.Fatalf("sends = %+v, want one send with no thread", sender.sent)
	}
}

// TestMessageValidateNullThreadIsAbsent proves a null thread beside a
// thread_key names one root, so the pair is not refused as a conflict.
func TestMessageValidateNullThreadIsAbsent(t *testing.T) {
	t.Parallel()

	built, err := builtins.MessageFactory(&recordingSender{}, nil)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	task := messageTask(map[string]any{
		identityKey: testIdentity, toKey: testChannel, textKey: testBody,
		threadKey: nil, threadKeyParam: testThreadKey,
	})

	if err := built.Validate(task); err != nil {
		t.Fatalf("Validate: %v, want nil", err)
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
			name:     caseMissingTo,
			params:   map[string]any{identityKey: testIdentity, textKey: testBody},
			wantCode: "message.missing_to",
		},
		{
			name:     "missing text",
			params:   map[string]any{identityKey: testIdentity, toKey: testChannel},
			wantCode: "message.missing_text",
		},
		{
			name: "thread and thread_key together",
			params: map[string]any{
				identityKey: testIdentity, toKey: testChannel, textKey: testBody,
				threadKey: testRootID, threadKeyParam: testThreadKey,
			},
			wantCode: "message.thread_conflict",
		},
		{
			name: "non-string thread_key",
			params: map[string]any{
				identityKey: testIdentity, toKey: testChannel, textKey: testBody, threadKeyParam: 7,
			},
			wantCode: "message.bad_thread_key",
		},
		{
			name: "non-string thread",
			params: map[string]any{
				identityKey: testIdentity, toKey: testChannel, textKey: testBody, threadKey: 7,
			},
			wantCode: "message.bad_thread",
		},
	}

	built, err := builtins.MessageFactory(&recordingSender{}, nil)(nil)
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

	built, err := builtins.MessageFactory(nil, nil)(nil)
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

			built, err := builtins.MessageFactory(&recordingSender{fail: testCase.fail}, nil)(nil)
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

// threadStore is an in-memory builtins.MessageThreads keyed the way the run
// store keys its rows, answering a miss with the store's own sentinel.
type threadStore struct {
	roots map[string]string
}

// threadRow joins the three parts a root is remembered under.
func threadRow(identity, target, key string) string {
	return identity + "|" + target + "|" + key
}

// LookupMessageThread reads one remembered root.
func (s *threadStore) LookupMessageThread(_ context.Context, identity, target, key string) (string, error) {
	rootID, known := s.roots[threadRow(identity, target, key)]
	if !known {
		return "", store.ErrMessageThreadNotFound
	}

	return rootID, nil
}

// BindMessageThread remembers one root, replacing any earlier one.
func (s *threadStore) BindMessageThread(_ context.Context, identity, target, key, rootID string) error {
	s.roots[threadRow(identity, target, key)] = rootID

	return nil
}

// keyedPost runs one thread_key post through a runner over the given seams.
func keyedPost(t *testing.T, sender *recordingSender, threads *threadStore) any {
	t.Helper()

	built, err := builtins.MessageFactory(sender, threads)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	result, err := built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity, toKey: testChannel, textKey: testBody, threadKeyParam: testThreadKey,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	return result.Output
}

func TestMessageThreadKeyOpensARootThenReplies(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{postID: testMessagePID}
	threads := &threadStore{roots: map[string]string{}}

	if output := keyedPost(t, sender, threads); output != testMessagePID {
		t.Fatalf("first post output = %v, want the root id %s", output, testMessagePID)
	}

	if got := threads.roots[threadRow(testIdentity, testChannel, testThreadKey)]; got != testMessagePID {
		t.Fatalf("remembered root = %q, want %q", got, testMessagePID)
	}

	keyedPost(t, sender, threads)

	if len(sender.sent) != 2 || sender.sent[0].Thread != "" || sender.sent[1].Thread != testMessagePID {
		t.Fatalf("sends = %+v, want a root post and then a reply on %s", sender.sent, testMessagePID)
	}
}

func TestMessageThreadKeyReplacesAMissingRoot(t *testing.T) {
	t.Parallel()

	const goneRoot = "post-deleted"

	sender := &recordingSender{postID: testMessagePID, goneRoot: goneRoot}
	threads := &threadStore{roots: map[string]string{threadRow(testIdentity, testChannel, testThreadKey): goneRoot}}

	if output := keyedPost(t, sender, threads); output != testMessagePID {
		t.Fatalf("output = %v, want the fresh root %s", output, testMessagePID)
	}

	if len(sender.sent) != 2 || sender.sent[0].Thread != goneRoot || sender.sent[1].Thread != "" {
		t.Fatalf("sends = %+v, want a failed reply on %s and then a fresh root", sender.sent, goneRoot)
	}

	if got := threads.roots[threadRow(testIdentity, testChannel, testThreadKey)]; got != testMessagePID {
		t.Fatalf("remembered root = %q, want the fresh %q", got, testMessagePID)
	}
}

func TestMessageThreadKeyKeepsOtherSendFailures(t *testing.T) {
	t.Parallel()

	sender := &recordingSender{fail: errMessageTransport}
	threads := &threadStore{roots: map[string]string{threadRow(testIdentity, testChannel, testThreadKey): testRootID}}

	built, err := builtins.MessageFactory(sender, threads)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	_, err = built.Execute(t.Context(), messageTask(map[string]any{
		identityKey: testIdentity, toKey: testChannel, textKey: testBody, threadKeyParam: testThreadKey,
	}))
	if !errors.Is(err, errMessageTransport) {
		t.Fatalf("Execute error = %v, want the transport failure", err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("sends = %+v, want one reply and no fresh root after an unrelated failure", sender.sent)
	}
}
