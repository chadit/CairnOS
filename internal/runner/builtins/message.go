package builtins

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// MessageName is the registry name of the chat-message runner.
const MessageName = "message"

// Message params. to and text reuse the keys the email runner already spells
// the same two ideas with.
const (
	paramIdentity  = "identity"
	paramThread    = "thread"
	paramThreadKey = "thread_key"
)

// Stable machine codes the send path reports.
const (
	codeMessageUnavailable = MessageName + ".unavailable"
	codeMessageSend        = MessageName + ".send"
	codeMessageThreadBoth  = MessageName + ".thread_conflict"
	codeMessageThreadStore = MessageName + ".thread_store"
)

// MessageThreads remembers the root post each thread_key opened; the run store
// satisfies it. A lookup miss is store.ErrMessageThreadNotFound.
type MessageThreads interface {
	LookupMessageThread(ctx context.Context, identity, target, key string) (string, error)
	BindMessageThread(ctx context.Context, identity, target, key, rootID string) error
}

// MessageSender is the send seam this runner drives. It is declared here rather
// than taken from the messaging package so pkg/runner and its built-ins stay the
// boundary an external runner author sees: serve adapts its identity registry
// onto this one call, and a test substitutes a recorder.
type MessageSender interface {
	// Send posts text as identity to a target, rooted on thread when one is
	// given, and returns the created message's id.
	Send(ctx context.Context, identity, to, text, thread string) (string, error)
}

// Message posts a chat message through one of serve's configured identities, so
// a workflow reaches a channel or a person on the same connection the bridge
// answers on.
//
// Config: none. Params: identity, to, text, and an optional thread or
// thread_key. The target is one of #channel, @user, channel:<id>, or user:<id>.
type Message struct {
	sender  MessageSender
	threads MessageThreads
}

// MessageFactory adapts a send seam to the registry contract. A nil sender is
// the ordinary one-shot case: `cairnos run`, the scheduler, and the MCP server
// hold no identity registry. Registering anyway keeps `cairnos validate` and a
// local run agreeing with serve about which runner names exist; Execute is
// where a host that cannot send says so.
func MessageFactory(sender MessageSender, threads MessageThreads) runner.Factory {
	return func(_ map[string]any) (runner.Runner, error) {
		return &Message{sender: sender, threads: threads}, nil
	}
}

// Name implements runner.Runner.
func (*Message) Name() string {
	return MessageName
}

// Validate implements runner.Runner: params must carry string identity, to, and
// text values, with at most one of thread and thread_key.
func (*Message) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramIdentity, MessageName); err != nil {
		return err
	}

	if err := requireStringParam(task, paramTo, MessageName); err != nil {
		return err
	}

	if err := requireStringParam(task, paramText, MessageName); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramThread, MessageName); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramThreadKey, MessageName); err != nil {
		return err
	}

	// Validate treats a null as absent, so a null thread beside a thread_key
	// names one root, not two.
	if task.Params[paramThread] != nil && task.Params[paramThreadKey] != nil {
		return taskError(runner.ClassPermanent, codeMessageThreadBoth,
			"%s and %s are mutually exclusive: pick a fixed root or a remembered one", paramThread, paramThreadKey)
	}

	return nil
}

// Execute implements runner.Runner. The created message id is the output so a
// later action can answer in the same thread without a lookup.
func (m *Message) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := m.Validate(task); err != nil {
		return runner.Result{}, err
	}

	if m.sender == nil {
		return runner.Result{}, taskError(runner.ClassPermanent, codeMessageUnavailable,
			"no messaging identity is reachable; the message runner sends only from cairnos serve")
	}

	identity, _ := task.Params[paramIdentity].(string)
	target, _ := task.Params[paramTo].(string)
	text, _ := task.Params[paramText].(string)
	thread, _ := task.Params[paramThread].(string)
	key, _ := task.Params[paramThreadKey].(string)

	start := time.Now()

	messageID, err := m.dispatch(ctx, identity, target, text, thread, key)
	if err != nil {
		return runner.Result{}, classifySend(err)
	}

	return runner.Result{
		Output: messageID,
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{paramIdentity: identity, paramTo: target},
	}, nil
}

// dispatch sends a keyed message through the thread store so every run of the
// workflow lands under the root post its key opened.
func (m *Message) dispatch(ctx context.Context, identity, target, text, thread, key string) (string, error) {
	if key != "" {
		return m.sendKeyed(ctx, identity, target, text, key)
	}

	messageID, err := m.sender.Send(ctx, identity, target, text, thread)
	if err != nil {
		return "", fmt.Errorf("post to %s: %w", target, err)
	}

	return messageID, nil
}

// sendKeyed replies under the root this key opened, or opens one. A root the
// server lost is replaced rather than failing every later run of the workflow.
func (m *Message) sendKeyed(ctx context.Context, identity, target, text, key string) (string, error) {
	if m.threads == nil {
		return "", taskError(runner.ClassPermanent, codeMessageThreadStore,
			"%s needs the run store, which this host did not hand the message runner", paramThreadKey)
	}

	rootID, err := m.threads.LookupMessageThread(ctx, identity, target, key)
	if err != nil && !errors.Is(err, store.ErrMessageThreadNotFound) {
		return "", runner.Wrapf(err, runner.ClassTransient, codeMessageThreadStore, "look up thread: %v", err)
	}

	if rootID != "" {
		messageID, err := m.sender.Send(ctx, identity, target, text, rootID)
		if err == nil {
			return messageID, nil
		}

		if !errors.Is(err, ErrMessageThreadMissing) {
			return "", fmt.Errorf("reply in thread %s: %w", rootID, err)
		}
	}

	return m.openThread(ctx, identity, target, text, key)
}

// openThread posts a new root and remembers it. A failed bind is permanent,
// because a retry would post the same root a second time.
func (m *Message) openThread(ctx context.Context, identity, target, text, key string) (string, error) {
	rootID, err := m.sender.Send(ctx, identity, target, text, "")
	if err != nil {
		return "", fmt.Errorf("open thread %s: %w", key, err)
	}

	if err := m.threads.BindMessageThread(ctx, identity, target, key, rootID); err != nil {
		return "", runner.Wrapf(err, runner.ClassPermanent, codeMessageThreadStore,
			"posted %s but could not remember it for %s %s: %v", rootID, paramThreadKey, key, err)
	}

	return rootID, nil
}

// classifySend keeps whatever classification the send seam already made, since
// that is the layer that knows an unknown identity from a dropped connection,
// and treats anything unclassified as transient because a send that failed for
// an unnamed reason is usually a connection that comes back.
func classifySend(cause error) error {
	if classified, ok := errors.AsType[*runner.Error](cause); ok {
		return classified
	}

	return &runner.Error{
		Class: runner.ClassTransient, Code: codeMessageSend,
		Message: "send message: " + cause.Error(), Cause: cause,
	}
}
