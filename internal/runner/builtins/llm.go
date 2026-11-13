package builtins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/environ"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Config keys and params the LLM API runners share. keyModel doubles as the
// config key and the param name: a workflow param overrides the configured
// default model, mirroring the webhook's url override. keyOutputFormat works
// the same way for the plain-vs-json output choice.
const (
	keyModel          = "model"
	cfgKeyBaseURL     = "base_url"
	cfgKeyMaxTokens   = "max_tokens"
	cfgKeyTemperature = "temperature"
	cfgKeyPriceIn     = "price_in"
	cfgKeyPriceOut    = "price_out"
	paramSystem       = "system"
	keyOutputFormat   = "output_format"
)

// Output-format values for keyOutputFormat. The empty value is the default:
// the model's text lands in Result.Output verbatim, byte for byte the same as
// before this option existed. "json" parses that text as a JSON object or array
// so downstream output references resolve fields natively.
const (
	outputFormatText = ""
	outputFormatJSON = "json"
)

// codeSuffixBadJSON tags a parse failure when output_format is json: the model
// returned prose, not the JSON the workflow asked for. It is permanent because
// re-sending the same request changes nothing.
const codeSuffixBadJSON = ".bad_json"

// Chat message roles in the providers' wire formats.
const (
	roleSystem = "system"
	roleUser   = "user"
)

// Stable machine-code suffixes the API runners share, hoisted so each
// classification site composes name + suffix instead of repeating literals.
const (
	codeSuffixAuth        = ".auth"
	codeSuffixStatus      = ".status"
	codeSuffixRateLimited = ".rate_limited"
	codeSuffixRead        = ".read"
	codeSuffixBadResponse = ".bad_response"
)

// headerRetryAfter is the standard rate-limit hint header.
const headerRetryAfter = "Retry-After"

// metaStopReason is the Meta key the LLM runners use for the provider's
// finish/stop reason. It aliases the driver-convention key so tool-calling
// results land where the agent loop reads them.
const metaStopReason = runner.MetaStopReason

// defaultLLMTimeout backstops the factory-built clients. Model responses can
// run minutes on long generations; ctx and the task deadline still bound each
// call tighter when the engine sets them.
const defaultLLMTimeout = 5 * time.Minute

// maxErrorSnippet caps how much of an error response body lands in the error
// message: enough to read the provider's complaint, bounded for logs.
const maxErrorSnippet = 512

// millionTokens is the denominator for the price_in/price_out config keys,
// which quote USD per one million tokens.
const millionTokens = 1_000_000

// llmConfig is the config-block shape the LLM API runners share: a default
// model, the API base URL, optional sampling knobs, optional pricing, and an
// optional default output format. Prices of zero mean unpriced, so CostUSD
// reports zero rather than a guess.
type llmConfig struct {
	// env answers the API-key variables. It is a field rather than a direct
	// os.Getenv so a test can hand one runner a key without setting it for
	// every other runner in the process.
	env environ.Lookup

	model        string
	baseURL      string
	outputFormat string
	maxTokens    int64
	temperature  float64
	priceIn      float64
	priceOut     float64
	hasTemp      bool
}

// decodeLLMConfig reads the shared LLM config keys with per-provider
// defaults. A zero defaultMaxTokens means the field is omitted when unset.
// The lookup rides along because the key never lives in the config block.
func decodeLLMConfig(
	cfg map[string]any, name, defaultBaseURL string, defaultMaxTokens int64, lookup environ.Lookup,
) (llmConfig, error) {
	var decoded llmConfig

	model, err := configValue(cfg, keyModel, "")
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	baseURL, err := configValue(cfg, cfgKeyBaseURL, defaultBaseURL)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	maxTokens, hasMax, err := configInt(cfg, cfgKeyMaxTokens)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	if !hasMax {
		maxTokens = defaultMaxTokens
	}

	temperature, hasTemp, err := configFloat(cfg, cfgKeyTemperature)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	priceIn, _, err := configFloat(cfg, cfgKeyPriceIn)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	priceOut, _, err := configFloat(cfg, cfgKeyPriceOut)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	outputFormat, err := configValue(cfg, keyOutputFormat, outputFormatText)
	if err != nil {
		return decoded, &configError{runner: name, err: err}
	}

	if err := validateOutputFormat(name, outputFormat); err != nil {
		return decoded, err
	}

	return llmConfig{
		env:          lookup,
		model:        model,
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		maxTokens:    maxTokens,
		temperature:  temperature,
		hasTemp:      hasTemp,
		priceIn:      priceIn,
		priceOut:     priceOut,
		outputFormat: outputFormat,
	}, nil
}

// validateOutputFormat rejects an unknown output_format at config and launch
// time alike, so a typo fails loudly instead of silently behaving like text.
func validateOutputFormat(name, value string) error {
	switch value {
	case outputFormatText, outputFormatJSON:
		return nil
	default:
		return paramError(name, "output_format", "must be empty or \"json\", got "+value)
	}
}

// cost prices one call from the configured per-million-token rates. Zero
// rates yield zero, the documented value for unpriced runners.
func (c *llmConfig) cost(inputTokens, outputTokens int64) float64 {
	return (float64(inputTokens)*c.priceIn + float64(outputTokens)*c.priceOut) / millionTokens
}

// resolveModel picks the model for one task: the model param when present,
// the configured default otherwise.
func (c *llmConfig) resolveModel(task *runner.Task) string {
	if override, isString := task.Params[keyModel].(string); isString && override != "" {
		return override
	}

	return c.model
}

// resolveOutputFormat picks the output format for one task: the output_format
// param when a non-empty string, the configured default otherwise. An empty
// or absent param falls through to config, so a param never silently forces
// text over a json default.
func (c *llmConfig) resolveOutputFormat(task *runner.Task) string {
	if override, isString := task.Params[keyOutputFormat].(string); isString && override != "" {
		return override
	}

	return c.outputFormat
}

// validateTask enforces the shared LLM params contract: a string prompt,
// optional string system and model, an optional string output_format that
// resolves to a known value, a model from params or config, and the provider's
// API key in the environment. An unresolved model reference reads as a
// non-empty string, so the load-time pass accepts it.
func (c *llmConfig) validateTask(task *runner.Task, name string, keyEnvs []string) error {
	if err := requireStringParam(task, paramPrompt, name); err != nil {
		return err
	}

	if err := optionalStringParam(task, paramSystem, name); err != nil {
		return err
	}

	if err := optionalStringParam(task, keyModel, name); err != nil {
		return err
	}

	if err := optionalStringParam(task, keyOutputFormat, name); err != nil {
		return err
	}

	format := c.resolveOutputFormat(task)
	if task.Resolved || !containsRefDelimiter(format) {
		if err := validateOutputFormat(name, format); err != nil {
			return err
		}
	}

	if c.resolveModel(task) == "" {
		return taskError(runner.ClassPermanent, name+".missing_model",
			"set model in params or in the runner config")
	}

	if err := c.requireAPIKey(name, keyEnvs); err != nil {
		return err
	}

	return nil
}

// requireAPIKey enforces the provider key except on our own side of the
// network, where a llama-server wants no credential and demanding one fails
// over a secret nothing reads.
//
// The exemption keys on the address, not on differing from the vendor default:
// a typo differs too, and exempting it would trade a validate-time complaint
// for a 401 mid-run.
func (c *llmConfig) requireAPIKey(name string, keyEnvs []string) error {
	if LocalEndpoint(c.baseURL) {
		return nil
	}

	if apiKey(c.env, keyEnvs) != "" {
		return nil
	}

	return taskError(runner.ClassPermanent, name+".missing_api_key", "%s", missingKeyMessage(keyEnvs))
}

// missingKeyMessage tells the operator what to set. A block named after its
// type reads one variable, and "set one of the X environment variables" reads
// like a list with one thing in it.
func missingKeyMessage(keyEnvs []string) string {
	if len(keyEnvs) == 1 {
		return "set the " + keyEnvs[0] + " environment variable"
	}

	return "set one of the " + strings.Join(keyEnvs, " or ") + " environment variables"
}

// apiKey returns the first key the lookup carries, so a block named after
// itself wins over the variable named after its type. It reads at call time,
// which is what lets a rotation land without a restart.
func apiKey(lookup environ.Lookup, keyEnvs []string) string {
	for _, key := range keyEnvs {
		if value := lookup.Get(key); value != "" {
			return value
		}
	}

	return ""
}

// LocalEndpoint reports whether a base_url names a server this machine or its
// operators run. A bare "localhost" counts because it resolves to loopback
// everywhere. Any other name keeps the credential requirement: what it resolves
// to is unknowable at validate time, and a hosted endpoint is likelier.
//
// Exported so `cairnos secrets pull` draws the API-key line where the runner
// does.
func LocalEndpoint(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}

	addr := net.ParseIP(host)
	if addr == nil {
		return false
	}

	return isLocalNetworkIP(addr)
}

// formatModelOutput maps a model's text answer onto Result.Output for the
// resolved output format: text passes the string through verbatim, json parses
// it into a map or array so downstream output references resolve fields
// natively. A json parse failure is permanent, not a silent text fallback: a
// workflow that asked for json and got prose must fail loudly.
func formatModelOutput(name, format, text string) (any, error) {
	if format != outputFormatJSON {
		return text, nil
	}

	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: name + codeSuffixBadJSON,
			Message: "output_format=json but model output is not valid JSON: " + text,
			Cause:   err,
		}
	}

	return parsed, nil
}

// configInt reads an optional integer config key, reporting whether it was
// present so callers can apply their own defaults.
func configInt(cfg map[string]any, key string) (int64, bool, error) {
	raw, exists := cfg[key]
	if !exists {
		return 0, false, nil
	}

	value, isInt := raw.(int64)
	if !isInt {
		return 0, false, fmt.Errorf("%w: %s must be an integer", ErrConfigValue, key)
	}

	return value, true, nil
}

// configFloat reads an optional numeric config key. TOML decodes 2 as int64
// and 2.5 as float64, and both spellings are valid prices, so it accepts
// either.
func configFloat(cfg map[string]any, key string) (float64, bool, error) {
	raw, exists := cfg[key]
	if !exists {
		return 0, false, nil
	}

	switch value := raw.(type) {
	case float64:
		return value, true, nil
	case int64:
		return float64(value), true, nil
	default:
		return 0, false, fmt.Errorf("%w: %s must be a number", ErrConfigValue, key)
	}
}

// sendJSON posts one JSON payload and hands back the response with its
// status already classified, so callers only see 2xx bodies. The caller owns
// closing the body.
func sendJSON(ctx context.Context, client *http.Client, name, endpoint string, headers map[string]string, payload any) (*http.Response, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: name + ".bad_request",
			Message: "encode request: " + err.Error(), Cause: err,
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, &runner.Error{
			Class: runner.ClassPermanent, Code: name + ".bad_url",
			Message: "build request: " + err.Error(), Cause: err,
		}
	}

	request.Header.Set("Content-Type", "application/json")

	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, classifyAPITransport(ctx, name, err)
	}

	if err := classifyAPIStatus(name, response); err != nil {
		_ = response.Body.Close()

		return nil, err
	}

	return response, nil
}

// classifyAPITransport maps a failed HTTP round trip. Cancellation and
// deadline both surface as the timeout code; every other transport failure
// stays transient because connection problems are the retryable case.
func classifyAPITransport(ctx context.Context, name string, err error) error {
	code := name + ".network"
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		code = name + ".timeout"
	}

	return &runner.Error{
		Class: runner.ClassTransient, Code: code,
		Message: "call " + name + ": " + err.Error(), Cause: err,
	}
}

// classifyAPIStatus turns a non-2xx API response into a classified error:
// 401/403 are permanent auth failures, 429 is rate limited with the
// Retry-After hint, 408 and 5xx are transient, every other 4xx is permanent.
func classifyAPIStatus(name string, response *http.Response) error {
	status := response.StatusCode
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}

	message := name + " returned " + response.Status + bodySnippet(response)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &runner.Error{Class: runner.ClassPermanent, Code: name + codeSuffixAuth, Message: message}
	case status == http.StatusTooManyRequests:
		return &runner.Error{
			Class:      runner.ClassRateLimited,
			Code:       name + codeSuffixRateLimited,
			Message:    message,
			RetryAfter: retryAfterHint(response.Header.Get(headerRetryAfter)),
		}
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		return &runner.Error{Class: runner.ClassTransient, Code: name + codeSuffixStatus, Message: message}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: name + codeSuffixStatus, Message: message}
	}
}

// bodySnippet pulls a short prefix of an error response body into the error
// message. Read failures here are ignored: the status code already tells the
// classification story, the snippet is only color.
func bodySnippet(response *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxErrorSnippet))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}

	return ": " + string(bytes.TrimSpace(raw))
}

// sseEvent is one server-sent event from a streaming API response: the event
// name (empty when the stream only uses data lines) and the joined payload.
type sseEvent struct {
	name string
	data string
}

// readSSEEvents walks a text/event-stream body and invokes handle once per
// event. handle returns false to stop early (end-of-stream sentinels) or an
// error to abort; comment lines and unknown fields are skipped per the SSE
// format.
func readSSEEvents(body io.Reader, handle func(event sseEvent) (bool, error)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, initialScanBuffer), maxCapturedOutput)

	var decoder sseDecoder

	// Grow up front so the first event's data lines append without
	// reallocating. Reset hands the buffer to the dispatched string, so later
	// events grow their own.
	decoder.data.Grow(initialScanBuffer)

	for scanner.Scan() {
		event, complete := decoder.feed(scanner.Text())
		if !complete {
			continue
		}

		keep, err := handle(event)
		if err != nil {
			return err
		}

		if !keep {
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan event stream: %w", err)
	}

	return nil
}

// sseDecoder holds the event being assembled between the blank lines that
// delimit events, so readSSEEvents only sees whole events.
type sseDecoder struct {
	name string
	data strings.Builder
}

// feed takes one stream line and reports the event a blank line completes.
// Comment lines and unknown fields change nothing.
func (d *sseDecoder) feed(line string) (sseEvent, bool) {
	switch {
	case line == "":
		return d.flush()
	case strings.HasPrefix(line, "event:"):
		d.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		if d.data.Len() > 0 {
			d.data.WriteByte('\n')
		}

		d.data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}

	return sseEvent{}, false
}

// flush ends the current event and starts the next one empty. An event with
// no data lines dispatches nothing, per the SSE format, though its name still
// resets.
func (d *sseDecoder) flush() (sseEvent, bool) {
	name := d.name
	d.name = ""

	if d.data.Len() == 0 {
		return sseEvent{}, false
	}

	event := sseEvent{name: name, data: d.data.String()}

	d.data.Reset()

	return event, true
}
