package builtins_test

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
)

// Config keys of the reset-due rule, and a second codex wrapper for the cases
// that need two codex logins in one report.
const (
	resetDuePercentKey = "reset_due_percent"
	resetDueLeadKey    = "reset_due_lead"
	codexAltLauncher   = "codex-alt"
	weeklyWindow       = "weekly"
)

// resetClockEpoch is the instant every reset-due case reads on, so each expiry
// and reset time below is an offset from it.
const resetClockEpoch = 1790000000

// days spells an offset from resetClock the way the reset-due lines report it.
func days(count float64) time.Duration {
	return time.Duration(count * float64(24*time.Hour))
}

// resetClock is resetClockEpoch as a time.
func resetClock() time.Time { return time.Unix(resetClockEpoch, 0).UTC() }

// codexCreditsReply is an app-server reply whose weekly window is 90% spent
// with four days to go, followed by whatever credits member the case supplies.
func codexCreditsReply(creditsMember string) string {
	resetsAt := strconv.FormatInt(resetClock().Add(days(4)).Unix(), 10)

	return `{"id":2,"result":{"rateLimits":{"limitId":"codex",` +
		`"primary":{"usedPercent":90,"windowDurationMins":10080,"resetsAt":` + resetsAt + `},` +
		`"secondary":null,"planType":"prolite"}` + creditsMember + `},"jsonrpc":"2.0"}`
}

// epochAt renders resetClock plus offset as the epoch seconds both formats use.
func epochAt(offset time.Duration) string {
	return strconv.FormatInt(resetClock().Add(offset).Unix(), 10)
}

func TestUsageProbeReadsCodexResetCredits(t *testing.T) {
	t.Parallel()

	firstExpiry := resetClock().Add(days(10))
	secondExpiry := resetClock().Add(days(20))

	mixed := `,"rateLimitResetCredits":{"availableCount":3,"credits":[` +
		`{"id":"a","status":"available","expiresAt":` + epochAt(days(10)) + `},` +
		`{"id":"b","status":"available","expiresAt":` + epochAt(-days(1)) + `},` +
		`{"id":"c","status":"redeemed","expiresAt":` + epochAt(days(5)) + `},` +
		`{"id":"d","status":"redeeming","expiresAt":` + epochAt(days(5)) + `},` +
		`{"id":"e","status":"available","expiresAt":null},` +
		`{"id":"f","status":"available","expiresAt":"soon"},` +
		`"junk",null,` +
		`{"id":"g","status":"available","expiresAt":` + epochAt(days(20)) + `}]}`

	tests := []struct {
		name   string
		member string
		want   []time.Time
	}{
		{name: "available unexpired credits only", member: mixed, want: []time.Time{firstExpiry, secondExpiry}},
		{name: "member missing", member: ""},
		{name: "member null", member: `,"rateLimitResetCredits":null`},
		{name: "member not an object", member: `,"rateLimitResetCredits":[1,2]`},
		{name: "credits null", member: `,"rateLimitResetCredits":{"availableCount":2,"credits":null}`},
		{name: "credits not a list", member: `,"rateLimitResetCredits":{"credits":{"status":"available"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeShim(t, dir, codexLauncher, codexHandshakeScript+`echo '`+codexCreditsReply(tt.member)+`'`)

			usage := newUsage(t, map[string]any{codexConfigKey: []any{codexLauncher}},
				builtins.UsageOptions{Lookup: pathEnv(dir), Now: resetClock})

			got, err := usage.Report(t.Context(), usageTask(map[string]any{usageLaunchers: []any{codexLauncher}}))
			if err != nil {
				t.Fatalf("report: %v", err)
			}

			login := findLogin(t, &got, codexLauncher)
			if login.Error != "" || len(login.Windows) != 1 {
				t.Fatalf("codex login = %+v, want one window and no error whatever the credits member holds", login)
			}

			if !slices.EqualFunc(login.ResetCredits, tt.want, time.Time.Equal) {
				t.Fatalf("reset credits = %v, want %v", login.ResetCredits, tt.want)
			}

			// The window is 90% spent with four days left, so any credit makes it due.
			if got.HasResetDue != (len(tt.want) > 0) {
				t.Fatalf("has_reset_due = %v with credits %v (reset_due %q)", got.HasResetDue, tt.want, got.ResetDue)
			}
		})
	}
}

// resetAccount is one report-file account, its times written as offsets from
// resetClock().
type resetAccount struct {
	tool     string
	wrapper  string
	failure  string
	window   string
	credits  []time.Duration
	used     float64
	resetsIn time.Duration
}

// entry renders the account in the report-file format.
func (a *resetAccount) entry() map[string]any {
	expiries := make([]int64, 0, len(a.credits))

	for _, offset := range a.credits {
		expiries = append(expiries, resetClock().Add(offset).Unix())
	}

	entry := map[string]any{
		toolKey:   a.tool,
		"wrapper": a.wrapper,
		"windows": []any{map[string]any{
			"name":         a.window,
			"used_percent": a.used,
			"resets_at":    resetClock().Add(a.resetsIn).Unix(),
		}},
		"reset_credit_expiries": expiries,
	}

	if a.failure != "" {
		entry["error"] = a.failure
	}

	return entry
}

// resetReportFile writes a report holding accounts, stamped at resetClock, and
// returns its path.
func resetReportFile(t *testing.T, accounts []resetAccount) string {
	t.Helper()

	entries := make([]any, 0, len(accounts))

	for index := range accounts {
		entries = append(entries, accounts[index].entry())
	}

	body, err := json.Marshal(map[string]any{
		"schema":     "cairnos.usage.v1",
		"written_at": resetClock().Unix(),
		"accounts":   entries,
	})
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}

	output := filepath.Join(t.TempDir(), reportFileName)

	if err := os.WriteFile(output, body, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}

	return output
}

func TestUsageFlagsLoginsDueAResetCredit(t *testing.T) {
	t.Parallel()

	healthy := resetAccount{tool: claudeTool, wrapper: claudeLauncher, window: windowSevenDay, used: 10, resetsIn: days(3)}
	lowCodex := resetAccount{
		tool: codexConfigKey, wrapper: codexLauncher, window: windowSevenDay,
		used: 90, resetsIn: days(4), credits: []time.Duration{days(10)},
	}
	withCodex := func(change func(*resetAccount)) []resetAccount {
		account := lowCodex
		change(&account)

		return []resetAccount{healthy, account}
	}

	tests := []struct {
		cfg      map[string]any
		name     string
		want     string
		accounts []resetAccount
	}{
		{name: "no login due", accounts: []resetAccount{healthy}},
		{
			name:     "one login due",
			accounts: []resetAccount{healthy, lowCodex},
			want: "codex login codex-agent: 10.0% of the week left, 4.0 days to the reset, " +
				"1 banked reset, the first expires in 10.0 days",
		},
		{
			name: "several logins due in file order, naming the earliest expiry",
			accounts: []resetAccount{
				{
					tool: codexConfigKey, wrapper: codexLauncher, window: windowSevenDay,
					used: 95, resetsIn: days(5), credits: []time.Duration{days(12.5), days(2.5)},
				},
				healthy,
				{
					tool: codexConfigKey, wrapper: codexAltLauncher, window: windowSevenDay,
					used: 81.5, resetsIn: 49 * time.Hour, credits: []time.Duration{days(1)},
				},
			},
			want: "codex login codex-agent: 5.0% of the week left, 5.0 days to the reset, " +
				"2 banked resets, the first expires in 2.5 days\n" +
				"codex login codex-alt: 18.5% of the week left, 2.0 days to the reset, " +
				"1 banked reset, the first expires in 1.0 days",
		},
		{name: "exactly the percent threshold", accounts: withCodex(func(a *resetAccount) { a.used = 80 })},
		{name: "exactly the lead", accounts: withCodex(func(a *resetAccount) { a.resetsIn = 48 * time.Hour })},
		{name: "errored login", accounts: withCodex(func(a *resetAccount) { a.failure = "launcher timed out" })},
		{name: "no 7d window", accounts: withCodex(func(a *resetAccount) { a.window = weeklyWindow })},
		{name: "no credits", accounts: withCodex(func(a *resetAccount) { a.credits = nil })},
		{name: "only expired credits", accounts: withCodex(func(a *resetAccount) { a.credits = []time.Duration{-days(1)} })},
		{
			name: "claude login without credits",
			accounts: []resetAccount{
				lowCodex,
				{tool: claudeTool, wrapper: altLauncher, window: windowSevenDay, used: 95, resetsIn: days(5)},
			},
			want: "codex login codex-agent: 10.0% of the week left, 4.0 days to the reset, " +
				"1 banked reset, the first expires in 10.0 days",
		},
		{
			name:     "configured thresholds",
			cfg:      map[string]any{resetDuePercentKey: int64(30), resetDueLeadKey: "72h"},
			accounts: withCodex(func(a *resetAccount) { a.used, a.resetsIn = 75, days(3.5) }),
			want: "codex login codex-agent: 25.0% of the week left, 3.5 days to the reset, " +
				"1 banked reset, the first expires in 10.0 days",
		},
		{
			name:     "configured lead outlasting the reset",
			cfg:      map[string]any{resetDueLeadKey: "120h"},
			accounts: []resetAccount{healthy, lowCodex},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			output := resetReportFile(t, tt.accounts)
			cfg := reportConfig(binTrue, output)

			maps.Copy(cfg, tt.cfg)

			usage := newUsage(t, cfg, builtins.UsageOptions{Lookup: pathEnv(filepath.Dir(output)), Now: resetClock})

			got, err := usage.Report(t.Context(), usageTask(nil))
			if err != nil {
				t.Fatalf("report: %v", err)
			}

			if got.ResetDue != tt.want || got.HasResetDue != (tt.want != "") {
				t.Fatalf("reset_due/has_reset_due = %q/%v, want %q", got.ResetDue, got.HasResetDue, tt.want)
			}
		})
	}
}

func TestUsageConfigChecksTheResetDueKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value   any
		name    string
		key     string
		refused bool
	}{
		{name: "percent zero", key: resetDuePercentKey, value: int64(0)},
		{name: "percent hundred", key: resetDuePercentKey, value: 100.0},
		{name: "percent fraction", key: resetDuePercentKey, value: 12.5},
		{name: "percent negative", key: resetDuePercentKey, value: -0.5, refused: true},
		{name: "percent over hundred", key: resetDuePercentKey, value: int64(101), refused: true},
		{name: "percent NaN", key: resetDuePercentKey, value: math.NaN(), refused: true},
		{name: "percent as text", key: resetDuePercentKey, value: "20", refused: true},
		{name: "lead in hours", key: resetDueLeadKey, value: "36h"},
		{name: "lead zero", key: resetDueLeadKey, value: "0s", refused: true},
		{name: "lead negative", key: resetDueLeadKey, value: "-1h", refused: true},
		{name: "lead unparsable", key: resetDueLeadKey, value: "eventually", refused: true},
		{name: "lead as a number", key: resetDueLeadKey, value: int64(48), refused: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewUsage(map[string]any{tt.key: tt.value}, builtins.UsageOptions{})

			if tt.refused != errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("%s = %v gave %v, want refused %v", tt.key, tt.value, err, tt.refused)
			}

			if !tt.refused && err != nil {
				t.Fatalf("%s = %v gave %v, want it accepted", tt.key, tt.value, err)
			}
		})
	}
}
