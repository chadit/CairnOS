package builtins

import (
	"context"
	"errors"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// MessageName is the registry name of the chat-message runner.
const MessageName = "message"

// Message params. to and text reuse the keys the email runner already spells
// the same two ideas with.
const (
	paramIdentity = "identity"
	paramThread   = "thread"
)

// Stable machine codes the send path reports.
const (
	codeMessageUnavailable = MessageName + ".unavailable"
	codeMessageSend        = MessageName + ".send"
)

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
// answers on (FR32).
//
// Config: none. Params: identity, to, text, and an optional thread. The target
// is one of #channel, @user, channel:<id>, or user:<id>.
type Message struct {
	sender MessageSender
}

// MessageFactory adapts a send seam to the registry contract. A nil sender is
// the ordinary one-shot case: `cairnos run`, the scheduler, and the MCP server
// hold no identity registry. Registering anyway keeps `cairnos validate` and a
// local run agreeing with serve about which runner names exist; Execute is
// where a host that cannot send says so.
func MessageFactory(sender MessageSender) runner.Factory {
	return func(_ map[string]any) (runner.Runner, error) {
		return &Message{sender: sender}, nil
	}
}

// Name implements runner.Runner.
func (*Message) Name() string {
	return MessageName
}

// Validate implements runner.Runner: params must carry string identity, to, and
// text values, with thread optional.
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

	return optionalStringParam(task, paramThread, MessageName)
}

// Execute implements runner.Runner. The created message id is the output so a
// later action can answer in the same thread without a lookup.
func (m *Message) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := m.Validate(task); err != nil {
		return runner.Result{}, err
	}

	if m.sender == nil {
		return runner.Result{}, runner.NewError(runner.ClassPermanent, codeMessageUnavailable,
			"no messaging identity is reachable; the message runner sends only from cairnos serve")
	}

	identity, _ := task.Params[paramIdentity].(string)
	target, _ := task.Params[paramTo].(string)
	text, _ := task.Params[paramText].(string)
	thread, _ := task.Params[paramThread].(string)

	start := time.Now()

	messageID, err := m.sender.Send(ctx, identity, target, text, thread)
	if err != nil {
		return runner.Result{}, classifySend(err)
	}

	return runner.Result{
		Output: messageID,
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{paramIdentity: identity, paramTo: target},
	}, nil
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
