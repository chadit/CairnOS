package builtins_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	hostKey    = "host"
	portKey    = "port"
	fromKey    = "from"
	toKey      = "to"
	subjectKey = "subject"

	testSMTPHost  = "smtp.cairn.test"
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

	configured := map[string]any{hostKey: testSMTPHost, fromKey: testSender}

	cases := []struct {
		cfg     map[string]any
		params  map[string]any
		name    string
		wantErr bool
	}{
		{name: "unconfigured runner", params: map[string]any{toKey: testRecipient, bodyKey: testBody}, wantErr: true},
		{name: caseMissingTo, cfg: configured, params: map[string]any{bodyKey: testBody}, wantErr: true},
		{name: caseMissingBody, cfg: configured, params: map[string]any{toKey: testRecipient}, wantErr: true},
		{name: "non-string subject", cfg: configured, params: map[string]any{toKey: testRecipient, bodyKey: testBody, subjectKey: int64(1)}, wantErr: true},
		{name: "complete params", cfg: configured, params: map[string]any{toKey: testRecipient, bodyKey: testBody, subjectKey: "ping"}, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mail, err := builtins.NewEmail(tt.cfg, environ.Map(nil))
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

// refusedPort needs root to bind, which is what makes a dial to it fail.
const refusedPort = 1

func TestEmailDialFailureIsTransient(t *testing.T) {
	t.Parallel()

	// Port 1 needs root to bind, so nothing in this suite can answer there and
	// the dial is refused every run. Taking an ephemeral port and closing it
	// races instead: another test's listener can pick the same port up and
	// answer the dial this case needs refused.
	cfg := map[string]any{
		hostKey: "127.0.0.1",
		portKey: int64(refusedPort),
		fromKey: testSender,
	}

	mail, err := builtins.NewEmail(cfg, environ.Map(nil))
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

	cfg := map[string]any{hostKey: testSMTPHost, fromKey: testSender}

	mail, err := builtins.NewEmail(cfg, environ.Map(nil))
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
