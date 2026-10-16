package builtins

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"
)

// Request ids the two handshakes carry. They are fixed rather than generated
// because nothing else shares the pipe, and a constant makes a captured
// transcript readable.
const (
	claudeInitID  = "cairnos-init"
	claudeUsageID = "cairnos-usage"

	codexInitID  int64 = 1
	codexUsageID int64 = 2
)

// The two control requests the Claude CLI answers on its stream-json stdin.
const (
	claudeInitRequest  = `{"type":"control_request","request_id":"` + claudeInitID + `","request":{"subtype":"initialize"}}`
	claudeUsageRequest = `{"type":"control_request","request_id":"` + claudeUsageID + `","request":{"subtype":"get_usage"}}`
)

// The JSON-RPC version both requests carry, the notification that needs no id,
// and the method names.
const (
	jsonRPCVersion         = "2.0"
	codexInitializedNotice = `{"jsonrpc":"` + jsonRPCVersion + `","method":"initialized"}`
	codexInitMethod        = "initialize"
	codexUsageMethod       = "account/rateLimits/read"
)

// claudeStreamFormat is the line-delimited JSON mode the Claude CLI reads and
// writes for the usage exchange.
const claudeStreamFormat = "stream-json"

// Line markers the decoders match on.
const (
	claudeResponseType = "control_response"
	claudeSuccess      = "success"
)

// Short window labels the report uses for the two windows a plan has.
const (
	windowFiveHour = "5h"
	windowSevenDay = "7d"
)

// Window lengths the Codex app-server reports in minutes.
const (
	fiveHourMinutes = 300
	sevenDayMinutes = 10080
)

// usageDetail is what one successful exchange yields.
type usageDetail struct {
	plan    string
	windows []UsageWindow
}

// decodeLine parses one stdout line into target, answering false when the line
// is not a JSON object. Both streams carry text that is not a protocol message,
// so a line that will not parse is one to skip rather than a probe failure.
func decodeLine(line []byte, target any) bool {
	return json.Unmarshal(line, target) == nil
}

// usageDecoder reads one stdout line. It answers false for the notifications
// and system lines that share the stream, true with a detail for the answer,
// and an error for an answer that reports a problem with the login.
type usageDecoder func(line []byte) (usageDetail, bool, error)

// usageDialog is one CLI's stdio contract: the argv that puts it in protocol
// mode, the lines written to its stdin, whether stdin closes before the read,
// and how the answer is recognized.
type usageDialog struct {
	decode     usageDecoder
	args       []string
	stdinLines []string
	closeStdin bool
}

// usageDialogFor builds the exchange for one protocol. version rides in the
// Codex handshake, which is the only part of either dialog that is not fixed.
func usageDialogFor(protocol, version string) (usageDialog, error) {
	switch protocol {
	case cfgKeyUsageClaude:
		return claudeDialog(), nil
	case cfgKeyUsageCodex:
		return codexDialog(version)
	default:
		return usageDialog{}, fmt.Errorf("%w: %s", errUsageProtocol, protocol)
	}
}

// claudeDialog drives the CLI's stream-json mode. stdin closes right after the
// two requests: the CLI answers first and exits on EOF, so holding the pipe
// open would only keep a finished process alive.
func claudeDialog() usageDialog {
	return usageDialog{
		args: []string{
			"-p",
			"--input-format", claudeStreamFormat,
			"--output-format", claudeStreamFormat,
			"--verbose",
		},
		stdinLines: []string{claudeInitRequest, claudeUsageRequest},
		closeStdin: true,
		decode:     decodeClaudeUsage,
	}
}

// codexDialog drives the app-server's JSON-RPC mode. stdin stays open until the
// reply lands, because app-server treats an early EOF as a shutdown and exits
// without answering.
func codexDialog(version string) (usageDialog, error) {
	initialize, err := codexRequestLine(codexInitID, codexInitMethod, map[string]any{
		"clientInfo": map[string]any{keyName: clientName, "version": version},
	})
	if err != nil {
		return usageDialog{}, err
	}

	usage, err := codexRequestLine(codexUsageID, codexUsageMethod, map[string]any{})
	if err != nil {
		return usageDialog{}, err
	}

	return usageDialog{
		args:       []string{"app-server"},
		stdinLines: []string{initialize, codexInitializedNotice, usage},
		closeStdin: false,
		decode:     decodeCodexUsage,
	}, nil
}

// codexRequestLine renders one JSON-RPC request. Marshaling rather than pasting
// the line together keeps a params value carrying a quote from breaking it into
// two messages, and keeps each id spelled once, where the decoder reads it.
func codexRequestLine(id int64, method string, params map[string]any) (string, error) {
	encoded, err := json.Marshal(map[string]any{
		"jsonrpc": jsonRPCVersion,
		keyID:     id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return "", fmt.Errorf("encode codex %s request: %w", method, err)
	}

	return string(encoded), nil
}

// claudeControlLine is the envelope every stream-json control response shares.
// The payload stays raw so a line for another request id costs one small
// allocation instead of a full decode.
type claudeControlLine struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     string          `json:"error"`
		Response  json.RawMessage `json:"response"`
	} `json:"response"`
}

// claudeUsagePayload is the get_usage answer. The windows stay raw because the
// table holds nulls for the windows a plan lacks, and may hold keys this build
// has never heard of.
type claudeUsagePayload struct {
	RateLimits          map[string]json.RawMessage `json:"rate_limits"`
	SubscriptionType    string                     `json:"subscription_type"`
	RateLimitsAvailable bool                       `json:"rate_limits_available"`
}

// The rate_limits keys the Claude protocol documents. A plan carries a null
// for the ones it lacks, and may carry keys this build has never heard of.
const (
	claudeFiveHourKey       = "five_hour"
	claudeSevenDayKey       = "seven_day"
	claudeSevenDayOpusKey   = "seven_day_opus"
	claudeSevenDaySonnetKey = "seven_day_sonnet"
)

// claudeWindowOrder fixes the order the documented windows are listed in, so
// two runs of one login read the same way.
func claudeWindowOrder() []string {
	return []string{claudeFiveHourKey, claudeSevenDayKey, claudeSevenDayOpusKey, claudeSevenDaySonnetKey}
}

// decodeClaudeUsage matches the get_usage control response. A line that is not
// JSON is noise on a stream that also carries system events, so it is skipped
// rather than failing the probe.
func decodeClaudeUsage(line []byte) (usageDetail, bool, error) {
	var envelope claudeControlLine

	if !decodeLine(line, &envelope) {
		return usageDetail{}, false, nil
	}

	if envelope.Type != claudeResponseType || envelope.Response.RequestID != claudeUsageID {
		return usageDetail{}, false, nil
	}

	if envelope.Response.Subtype != claudeSuccess {
		return usageDetail{}, true, fmt.Errorf("%w: %s", errUsageRefused,
			usageTail(envelope.Response.Subtype+" "+envelope.Response.Error))
	}

	var payload claudeUsagePayload

	if err := json.Unmarshal(envelope.Response.Response, &payload); err != nil {
		return usageDetail{}, true, fmt.Errorf("decode get_usage payload: %w", err)
	}

	if !payload.RateLimitsAvailable {
		return usageDetail{}, true, errUsageNoLimits
	}

	windows, err := claudeWindows(payload.RateLimits)
	if err != nil {
		return usageDetail{}, true, err
	}

	if len(windows) == 0 {
		return usageDetail{}, true, errUsageNoLimits
	}

	return usageDetail{plan: payload.SubscriptionType, windows: windows}, true, nil
}

// claudeWindows puts the documented windows first and skips the nulls a plan
// without that window carries. A key that will not decode is remembered: among
// good keys it is a field this build has yet to learn, but alone it would make
// the report claim a plan with no limits.
func claudeWindows(table map[string]json.RawMessage) ([]UsageWindow, error) {
	documented := claudeWindowOrder()
	keys := slices.Clone(documented)

	for _, key := range slices.Sorted(maps.Keys(table)) {
		if !slices.Contains(documented, key) {
			keys = append(keys, key)
		}
	}

	windows := make([]UsageWindow, 0, len(keys))

	var unreadable string

	for _, key := range keys {
		window, ok, err := claudeWindow(table[key])
		if err != nil && unreadable == "" {
			unreadable = key
		}

		if !ok {
			continue
		}

		window.Name = claudeWindowName(key)
		windows = append(windows, window)
	}

	if len(windows) == 0 && unreadable != "" {
		return nil, fmt.Errorf("%w: rate_limits.%s", errUsageBadWindow, unreadable)
	}

	return windows, nil
}

// claudeWindow reads one entry. An absent key and the null a plan carries for a
// window it lacks are both "no window here"; a value the window shape cannot
// hold is reported, so the caller can decide whether anything else decoded.
func claudeWindow(raw json.RawMessage) (UsageWindow, bool, error) {
	if len(raw) == 0 {
		return UsageWindow{}, false, nil
	}

	var parsed struct {
		ResetsAt    *time.Time `json:"resets_at"`
		Utilization *float64   `json:"utilization"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return UsageWindow{}, false, fmt.Errorf("decode window: %w", err)
	}

	if parsed.ResetsAt == nil || parsed.Utilization == nil {
		return UsageWindow{}, false, nil
	}

	return UsageWindow{ResetsAt: parsed.ResetsAt.UTC(), UsedPercent: *parsed.Utilization}, true, nil
}

// claudeWindowName gives a documented key its short label and leaves any other
// key as it arrived, so a window Anthropic adds still reaches the report.
func claudeWindowName(key string) string {
	switch key {
	case claudeFiveHourKey:
		return windowFiveHour
	case claudeSevenDayKey:
		return windowSevenDay
	case claudeSevenDayOpusKey:
		return windowSevenDay + " opus"
	case claudeSevenDaySonnetKey:
		return windowSevenDay + " sonnet"
	default:
		return key
	}
}

// codexResponseLine is one JSON-RPC line. A notification carries no id, which
// is what tells it apart from the reply this runner waits for. The result stays
// raw because its members are camelCase, which this project's tag convention
// does not carry; those keys are named as constants below instead.
type codexResponseLine struct {
	ID     *int64          `json:"id"`
	Error  *codexError     `json:"error"`
	Result json.RawMessage `json:"result"`
}

// Members of the app-server's reply, spelled the way it spells them.
const (
	codexRateLimitsKey  = "rateLimits"
	codexPrimaryKey     = "primary"
	codexSecondaryKey   = "secondary"
	codexPlanTypeKey    = "planType"
	codexUsedPercentKey = "usedPercent"
	codexResetsAtKey    = "resetsAt"
	codexWindowMinsKey  = "windowDurationMins"
)

// codexError is the JSON-RPC failure member, whose keys need no translation.
type codexError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// decodeCodexUsage matches the rate-limit reply by request id, skipping the
// configWarning and status notifications the app-server interleaves.
func decodeCodexUsage(line []byte) (usageDetail, bool, error) {
	var envelope codexResponseLine

	if !decodeLine(line, &envelope) {
		return usageDetail{}, false, nil
	}

	if envelope.ID == nil || *envelope.ID != codexUsageID {
		return usageDetail{}, false, nil
	}

	if envelope.Error != nil {
		return usageDetail{}, true, fmt.Errorf("%w: %s", errUsageRefused, usageTail(envelope.Error.Message))
	}

	limits, hasLimits := codexMember(envelope.Result, codexRateLimitsKey)
	if !hasLimits {
		return usageDetail{}, true, errUsageNoLimits
	}

	windows := codexWindows(limits)
	if len(windows) == 0 {
		return usageDetail{}, true, errUsageNoLimits
	}

	return usageDetail{plan: codexText(limits, codexPlanTypeKey), windows: windows}, true, nil
}

// codexFields decodes one JSON object into its raw members, answering false for
// a null, a missing value, or anything that is not an object.
func codexFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage

	if !decodeLine(raw, &fields) || fields == nil {
		return nil, false
	}

	return fields, true
}

// codexMember reads one nested object out of another.
func codexMember(raw json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	fields, ok := codexFields(raw)
	if !ok {
		return nil, false
	}

	return codexFields(fields[key])
}

// codexNumber reads one numeric member. Both the percentage and the epoch
// arrive as JSON numbers, so one reader covers them.
func codexNumber(fields map[string]json.RawMessage, key string) (float64, bool) {
	var value float64

	if !decodeLine(fields[key], &value) {
		return 0, false
	}

	return value, true
}

// codexText reads one string member, answering empty when it is absent.
func codexText(fields map[string]json.RawMessage, key string) string {
	var value string

	if !decodeLine(fields[key], &value) {
		return ""
	}

	return value
}

// codexWindows renders the primary window and the secondary when the plan has
// one. A plan with a single weekly window reports only the primary.
func codexWindows(limits map[string]json.RawMessage) []UsageWindow {
	keys := []string{codexPrimaryKey, codexSecondaryKey}
	windows := make([]UsageWindow, 0, len(keys))

	for _, key := range keys {
		window, ok := codexWindow(limits[key])
		if !ok {
			continue
		}

		windows = append(windows, window)
	}

	return windows
}

// codexWindow reads one window, treating anything without both a percentage and
// a reset time as a member that is not a window.
func codexWindow(raw json.RawMessage) (UsageWindow, bool) {
	fields, ok := codexFields(raw)
	if !ok {
		return UsageWindow{}, false
	}

	used, hasUsed := codexNumber(fields, codexUsedPercentKey)
	resets, hasResets := codexNumber(fields, codexResetsAtKey)

	if !hasUsed || !hasResets {
		return UsageWindow{}, false
	}

	minutes, _ := codexNumber(fields, codexWindowMinsKey)

	return UsageWindow{
		ResetsAt:    time.Unix(int64(resets), 0).UTC(),
		Name:        codexWindowName(int(minutes)),
		UsedPercent: used,
	}, true
}

// codexWindowName labels a window by its length, since the app-server reports
// minutes where the Claude protocol reports a name.
func codexWindowName(minutes int) string {
	switch minutes {
	case fiveHourMinutes:
		return windowFiveHour
	case sevenDayMinutes:
		return windowSevenDay
	default:
		return strconv.Itoa(minutes) + "m"
	}
}
