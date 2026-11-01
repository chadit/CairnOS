package builtins

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// EmailName is the registry name of the SMTP notification runner.
const EmailName = "email"

// emailPasswordEnv names where the SMTP password comes from. The config file
// never carries it, matching the repo-wide rule that secrets live in the
// environment.
const emailPasswordEnv = "CAIRNOS_RUNNER_EMAIL_PASSWORD" // #nosec G101 -- environment variable name, not a credential

// Email config keys and defaults.
const (
	cfgKeyHost = "host"
	cfgKeyPort = "port"
	cfgKeyFrom = "from"
	cfgKeyUser = "user"

	defaultSMTPPort = "587"
)

// Email params.
const (
	paramTo      = "to"
	paramSubject = "subject"
	paramBody    = "body"
)

// Stable machine codes the SMTP conversation reuses.
const (
	codeEmailAddress = EmailName + ".address"
	codeEmailSend    = EmailName + ".send"
)

// crlf is the line terminator RFC 5322 message headers require.
const crlf = "\r\n"

// headerOverhead is the Grow headroom for the fixed header labels and CRLF
// separators around the variable message parts.
const headerOverhead = 64

// Email sends a notification message over SMTP, one of the terminal-action
// sinks in the built-ins table.
//
// Config: host, port (default 587), from, and an optional auth user; the
// password comes from the CAIRNOS_RUNNER_EMAIL_PASSWORD environment variable
// at send time. Params: to, body, and an optional subject. STARTTLS engages
// whenever the server advertises it, mirroring smtp.SendMail's default.
type Email struct {
	// env answers the password variable at send time, injected so a test can
	// supply one without setting it for the whole process.
	env environ.Lookup

	host string
	port string
	from string
	user string
}

// NewEmail builds the runner from its config block. An empty block still
// builds (the registry constructs every built-in at startup); Validate
// reports the missing settings when a workflow actually references the
// runner.
func NewEmail(cfg map[string]any, lookup environ.Lookup) (*Email, error) {
	host, err := configValue(cfg, cfgKeyHost, "")
	if err != nil {
		return nil, err
	}

	port, err := configPortText(cfg)
	if err != nil {
		return nil, err
	}

	from, err := configValue(cfg, cfgKeyFrom, "")
	if err != nil {
		return nil, err
	}

	user, err := configValue(cfg, cfgKeyUser, "")
	if err != nil {
		return nil, err
	}

	return &Email{host: host, port: port, from: from, user: user, env: lookup}, nil
}

// EmailFactory adapts NewEmail to the registry contract.
func EmailFactory(lookup environ.Lookup) runner.Factory {
	return func(cfg map[string]any) (runner.Runner, error) {
		return NewEmail(cfg, lookup)
	}
}

// Name implements runner.Runner.
func (*Email) Name() string {
	return EmailName
}

// Validate implements runner.Runner: the runner must be configured, and
// params must carry string to and body values (subject optional).
func (e *Email) Validate(task *runner.Task) error {
	if e.host == "" || e.from == "" {
		return taskError(runner.ClassPermanent, EmailName+".not_configured",
			"runner config must set host and from")
	}

	if err := requireStringParam(task, paramTo, EmailName); err != nil {
		return err
	}

	if err := requireStringParam(task, paramBody, EmailName); err != nil {
		return err
	}

	return optionalStringParam(task, paramSubject, EmailName)
}

// Execute implements runner.Runner.
func (e *Email) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := e.Validate(task); err != nil {
		return runner.Result{}, err
	}

	recipient, _ := task.Params[paramTo].(string)
	body, _ := task.Params[paramBody].(string)
	subject, _ := task.Params[paramSubject].(string)

	// Line breaks in header values would let a workflow param smuggle extra
	// headers (Bcc, a second To) into the message.
	if strings.ContainsAny(recipient+subject, crlf) {
		return runner.Result{}, taskError(runner.ClassPermanent, EmailName+".bad_params",
			"to and subject must not contain line breaks")
	}

	start := time.Now()

	if err := e.send(ctx, recipient, subject, body); err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: "sent",
		Usage:  runner.Usage{Latency: time.Since(start)},
		Meta:   map[string]string{paramTo: recipient},
	}, nil
}

// send drives one SMTP conversation. Dial and handshake failures are
// transient (the server may come back); auth and addressing rejections are
// permanent because retrying resends the same bad credentials or address.
func (e *Email) send(ctx context.Context, recipient, subject, body string) error {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(e.host, e.port))
	if err != nil {
		return &runner.Error{
			Class: runner.ClassTransient, Code: EmailName + ".dial",
			Message: "dial " + e.host + ": " + err.Error(), Cause: err,
		}
	}

	client, err := smtp.NewClient(conn, e.host)
	if err != nil {
		// Closing the raw conn directly: the client never took ownership.
		_ = conn.Close()

		return &runner.Error{
			Class: runner.ClassTransient, Code: EmailName + ".greeting",
			Message: "smtp greeting: " + err.Error(), Cause: err,
		}
	}

	// Quit below ends the session; this close only matters on early error
	// returns, and closing a finished session twice is harmless.
	defer func() { _ = client.Close() }()

	if err := e.handshake(client); err != nil {
		return err
	}

	if err := submitMessage(client, e.from, recipient, composeMessage(e.from, recipient, subject, body)); err != nil {
		return err
	}

	// The server accepted the message when the data writer closed; a failed
	// QUIT after that point is not a delivery failure, so it is only logged.
	if err := client.Quit(); err != nil {
		slog.WarnContext(ctx, "smtp quit after delivery", "host", e.host, "error", err)
	}

	return nil
}

// handshake upgrades to TLS when offered and authenticates when a user is
// configured.
func (e *Email) handshake(client *smtp.Client) error {
	if hasTLS, _ := client.Extension("STARTTLS"); hasTLS {
		tlsCfg := &tls.Config{ServerName: e.host, MinVersion: tls.VersionTLS12}

		if err := client.StartTLS(tlsCfg); err != nil {
			return &runner.Error{
				Class: runner.ClassTransient, Code: EmailName + ".tls",
				Message: "starttls: " + err.Error(), Cause: err,
			}
		}
	}

	if e.user == "" {
		return nil
	}

	auth := smtp.PlainAuth("", e.user, e.env.Get(emailPasswordEnv), e.host)

	if err := client.Auth(auth); err != nil {
		return &runner.Error{
			Class: runner.ClassPermanent, Code: EmailName + ".auth",
			Message: "authenticate as " + e.user + ": " + err.Error(), Cause: err,
		}
	}

	return nil
}

// submitMessage runs the MAIL/RCPT/DATA sequence. Addressing rejections are
// permanent; an interrupted DATA exchange is transient because the
// connection, not the message, failed.
func submitMessage(client *smtp.Client, from, recipient string, message []byte) error {
	if err := client.Mail(from); err != nil {
		return &runner.Error{
			Class: runner.ClassPermanent, Code: codeEmailAddress,
			Message: "sender rejected: " + err.Error(), Cause: err,
		}
	}

	if err := client.Rcpt(recipient); err != nil {
		return &runner.Error{
			Class: runner.ClassPermanent, Code: codeEmailAddress,
			Message: "recipient rejected: " + err.Error(), Cause: err,
		}
	}

	writer, err := client.Data()
	if err != nil {
		return &runner.Error{
			Class: runner.ClassTransient, Code: codeEmailSend,
			Message: "open data: " + err.Error(), Cause: err,
		}
	}

	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()

		return &runner.Error{
			Class: runner.ClassTransient, Code: codeEmailSend,
			Message: "write message: " + err.Error(), Cause: err,
		}
	}

	if err := writer.Close(); err != nil {
		return &runner.Error{
			Class: runner.ClassTransient, Code: codeEmailSend,
			Message: "finish message: " + err.Error(), Cause: err,
		}
	}

	return nil
}

// composeMessage assembles a minimal RFC 5322 message. Execute already
// rejected line breaks in the header values.
func composeMessage(from, recipient, subject, body string) []byte {
	var msg strings.Builder

	msg.Grow(len(from) + len(recipient) + len(subject) + len(body) + headerOverhead)

	msg.WriteString("From: ")
	msg.WriteString(from)
	msg.WriteString(crlf)
	msg.WriteString("To: ")
	msg.WriteString(recipient)
	msg.WriteString(crlf)

	if subject != "" {
		msg.WriteString("Subject: ")
		msg.WriteString(subject)
		msg.WriteString(crlf)
	}

	msg.WriteString(crlf)
	msg.WriteString(body)
	msg.WriteString(crlf)

	return []byte(msg.String())
}

// configPortText reads the port config key, accepting the TOML integer form
// and the string form so both port = 587 and port = "587" work.
func configPortText(cfg map[string]any) (string, error) {
	raw, exists := cfg[cfgKeyPort]
	if !exists {
		return defaultSMTPPort, nil
	}

	switch value := raw.(type) {
	case int64:
		return strconv.FormatInt(value, 10), nil
	case string:
		return value, nil
	default:
		return "", fmt.Errorf("%w: %s must be an integer or string", ErrConfigValue, cfgKeyPort)
	}
}
