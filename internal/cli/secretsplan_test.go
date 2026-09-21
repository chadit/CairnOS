package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// The identity a host declares, the bot account behind it, and the variable the
// identity reads first. The three values differ in length so a case can tell
// which source a token came from without printing one.
const (
	botIdentity   = "helper"
	botUsername   = "helper-agent"
	botTokenVar   = "CAIRNOS_BRIDGE_MATTERMOST_HELPER_TOKEN"
	botValue      = "bot-token-value"
	explicitValue = "explicit-token-value"
)

// botConfig stages a home with one Mattermost identity whose bot account is
// botUsername, plus whatever extra tables the case needs.
func botConfig(t *testing.T, extra string) string {
	t.Helper()

	return writeBitwardenConfig(t, "[secrets.bitwarden]\nitem = \""+pullItem+"\"\n\n"+
		"[bridge.mattermost]\nurl = \"https://mm.example.com\"\n\n"+
		"[bridge.mattermost.identity."+botIdentity+"]\n"+
		"username = \""+botUsername+"\"\nworkspace = \"default\"\n"+extra)
}

// renderedLength returns how long the value one variable was written with is.
// Cases compare lengths rather than values so a failure message never carries a
// token.
func renderedLength(t *testing.T, rendered, name string) int {
	t.Helper()

	for line := range strings.SplitSeq(rendered, "\n") {
		value, found := strings.CutPrefix(line, name+"='")
		if !found {
			continue
		}

		return len(strings.TrimSuffix(value, "'"))
	}

	t.Fatalf("the rendered file carries no %s", name)

	return 0
}

// TestSecretsPullRendersAFieldNamedAfterABot is the case the fleet is in: the
// operator keeps one token per bot on the item under the bot's own Mattermost
// username, and no mapping says which variable that fills.
func TestSecretsPullRendersAFieldNamedAfterABot(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{botUsername: botValue})
	dir := botConfig(t, "")

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
	if got := renderedLength(t, rendered, botTokenVar); got != len(botValue) {
		t.Fatalf("%s holds a %d-character value, want %d", botTokenVar, got, len(botValue))
	}

	if strings.Contains(rendered, botUsername+"=") {
		t.Fatalf("the username was rendered as a variable name of its own")
	}
}

// TestSecretsPullListsAnInferredBotTokenOnADryRun keeps the reporting path
// honest: a variable the pull would write has to appear in the report an
// operator reads before letting it write.
func TestSecretsPullListsAnInferredBotTokenOnADryRun(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{botUsername: botValue})
	dir := botConfig(t, "")

	code, stdout, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, botTokenVar) {
		t.Fatalf("the dry run does not list the inferred token variable:\n%s", stdout)
	}

	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); err == nil {
		t.Fatalf("the dry run wrote the file")
	}
}

// TestSecretsPullResolvesABotTokenMostSpecificFirst is the plan's whole
// ordering rule in one case. A name that says which variable it fills beats a
// name that says which bot it belongs to, and a mapping the operator wrote
// beats both.
func TestSecretsPullResolvesABotTokenMostSpecificFirst(t *testing.T) {
	t.Parallel()

	mapping := "\n[secrets.bitwarden.fields]\n" + botTokenVar + " = \"" + humanField + "\"\n"

	cases := []struct {
		name   string
		fields map[string]string
		extra  string
		want   string
	}{
		{
			name:   "only the username field",
			fields: map[string]string{botUsername: botValue},
			want:   botValue,
		},
		{
			name: "an explicit variable name beats the username field",
			fields: map[string]string{
				botUsername: botValue,
				botTokenVar: explicitValue,
			},
			want: explicitValue,
		},
		{
			name: "a mapping beats both",
			fields: map[string]string{
				botUsername: botValue,
				botTokenVar: explicitValue,
				humanField:  humanValue,
			},
			extra: mapping,
			want:  humanValue,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			binDir := stageFakeBW(t, testCase.fields)
			dir := botConfig(t, testCase.extra)

			code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
			if code != cli.ExitOK {
				t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
			}

			rendered := runPullRead(t, filepath.Join(dir, "secrets.env"))
			if got := renderedLength(t, rendered, botTokenVar); got != len(testCase.want) {
				t.Fatalf("%s holds a %d-character value, want %d",
					botTokenVar, got, len(testCase.want))
			}
		})
	}
}

// TestSecretsPullIsQuietWhenOnlyTheUsernameFieldCarriesTheToken covers the FR20
// half: an identity the item covers under the bot's name needs no change, so
// warning would send the operator to add a second copy of a token they have.
func TestSecretsPullIsQuietWhenOnlyTheUsernameFieldCarriesTheToken(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{botUsername: botValue})
	dir := botConfig(t, "")

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST") {
		t.Fatalf("the pull warned about a token the item already carries:\n%s", stderr)
	}
}

// TestSecretsPullWarnsWhenTheBotFieldIsEmpty is the other side: a field left
// blank would look configured in the generated file and behave as if it were
// not, so it counts as absent and the warning still names what to add.
func TestSecretsPullWarnsWhenTheBotFieldIsEmpty(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{
		botUsername: "",
		inferredVar: inferredValue,
	})

	dir := botConfig(t, "")

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_TOKEN") {
		t.Fatalf("an empty bot field was taken as a token:\n%s", stderr)
	}
}

// TestSecretsPullWarnsForAnIdentityWithNoFieldOfItsOwn proves the inference
// covers only the bots the item names. A second identity nothing on the item
// matches is still reported.
func TestSecretsPullWarnsForAnIdentityWithNoFieldOfItsOwn(t *testing.T) {
	t.Parallel()

	binDir := stageFakeBW(t, map[string]string{botUsername: botValue})

	dir := botConfig(t, "\n[bridge.mattermost.identity.ops]\n"+
		"username = \"ops-agent\"\nworkspace = \"default\"\n")

	code, _, stderr := runEnvCLI(t, pathEnv(binDir), secretsCmd, "pull", flagDirArg, dir)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok\nstderr: %s", code, stderr)
	}

	if !strings.Contains(stderr, "CAIRNOS_BRIDGE_MATTERMOST_TOKEN") {
		t.Fatalf("the identity with no field of its own went unreported:\n%s", stderr)
	}
}
