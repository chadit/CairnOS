package builtins_test

import (
	"errors"
	"net"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	hostKey    = "host"
	portKey    = "port"
	fromKey    = "from"
	toKey      = "to"
	subjectKey = "subject"

	testRecipient = "dev@cairn.test"
	testSender    = "cairn@cairn.test"
	testBody      = "hi"

	// actionNotify is the fixture action name the notification tests share.
	actionNotify = "notify"
)

func emailTask(params map[string]any) *runner.Task {
	return &runner.Task{RunID: testRunID, ActionName: actionNotify, Params: params, Resolved: true}
}

func TestEmailValidate(t *testing.T) {
	t.Parallel()

	configured := map[string]any{hostKey: "smtp.cairn.test", fromKey: testSender}

	cases := []struct {
		name    string
		cfg     map[string]any
		params  map[string]any
		wantErr bool
	}{
		{name: "unconfigured runner", params: map[string]any{toKey: testRecipient, bodyKey: testBody}, wantErr: true},
		{name: "missing to", cfg: configured, params: map[string]any{bodyKey: testBody}, wantErr: true},
		{name: "missing body", cfg: configured, params: map[string]any{toKey: testRecipient}, wantErr: true},
		{name: "non-string subject", cfg: configured, params: map[string]any{toKey: testRecipient, bodyKey: testBody, subjectKey: int64(1)}, wantErr: true},
		{name: "complete params", cfg: configured, params: map[string]any{toKey: testRecipient, bodyKey: testBody, subjectKey: "ping"}, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mail, err := builtins.NewEmail(tt.cfg)
			if err != nil {
				t.Fatalf("new email: %v", err)
			}

			err = mail.Validate(emailTask(tt.params))

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestEmailDialFailureIsTransient(t *testing.T) {
	t.Parallel()

	// Grabbing a loopback port and closing it leaves an address that refuses
	// connections, standing in for an unreachable SMTP server.
	var listenCfg net.ListenConfig

	listener, err := listenCfg.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr, isTCP := listener.Addr().(*net.TCPAddr)
	if !isTCP {
		t.Fatalf("listener address type %T", listener.Addr())
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	cfg := map[string]any{
		hostKey: "127.0.0.1",
		portKey: int64(addr.Port),
		fromKey: testSender,
	}

	mail, err := builtins.NewEmail(cfg)
	if err != nil {
		t.Fatalf("new email: %v", err)
	}

	_, err = mail.Execute(t.Context(), emailTask(map[string]any{toKey: testRecipient, bodyKey: testBody}))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != "email.dial" {
		t.Fatalf("error = %v, want transient email.dial", err)
	}
}

func TestEmailRejectsHeaderInjection(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{hostKey: "smtp.cairn.test", fromKey: testSender}

	mail, err := builtins.NewEmail(cfg)
	if err != nil {
		t.Fatalf("new email: %v", err)
	}

	params := map[string]any{
		toKey:   testRecipient + "\r\nBcc: hidden@evil.test",
		bodyKey: testBody,
	}

	_, err = mail.Execute(t.Context(), emailTask(params))

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassPermanent || classified.Code != "email.bad_params" {
		t.Fatalf("error = %v, want permanent email.bad_params", err)
	}
}
