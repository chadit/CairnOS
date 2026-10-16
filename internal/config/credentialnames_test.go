package config_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/config"
)

// credRunnerName is the runner block most cases here name, and zaiKeyEnv the
// one key variable both z-ai and z_ai render to.
const (
	credRunnerName = "zai"
	zaiKeyEnv      = "CAIRNOS_RUNNER_Z_AI_API_KEY"

	// secondBotTokenEnv is what an identity names when it must post as another
	// bot on the same server.
	secondBotTokenEnv = "CAIRNOS_SECOND_BOT_TOKEN" // #nosec G101 -- a variable name, not a credential
)

// TestMattermostTokenEnvsResolutionOrder covers every branch of the order an
// identity's token name is chosen by. One Mattermost server is the ordinary
// case, so the shared default is the last word; the per-identity name stays
// ahead of it for hosts that already export one.
func TestMattermostTokenEnvsResolutionOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		tokenEnv string
		want     []string
	}{
		{
			name: "no token_env tries the identity name then the shared default",
			want: []string{opsIdentityEnv, config.DefaultMattermostTokenEnv},
		},
		{
			// An operator who named the variable meant that one: falling back
			// from it would hand this identity another bot's token.
			name:     "an explicit token_env is the whole list",
			tokenEnv: secondBotTokenEnv,
			want:     []string{secondBotTokenEnv},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			identity := config.MattermostIdentity{TokenEnv: testCase.tokenEnv}

			if got := config.MattermostTokenEnvs(&identity, identityOps); !slices.Equal(got, testCase.want) {
				t.Fatalf("token envs = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestResolveMattermostToken covers which name actually supplies the token when
// more than one is set, and the answer when none is.
func TestResolveMattermostToken(t *testing.T) {
	t.Parallel()

	const (
		perIdentityToken = "token-from-the-identity-name"
		sharedToken      = "token-from-the-shared-name"
		namedToken       = "token-from-token-env"
	)

	perIdentity := config.MattermostTokenEnv(identityOps)

	cases := []struct {
		env      map[string]string
		name     string
		tokenEnv string
		wantEnv  string
		want     string
	}{
		{
			name:    "the shared default alone",
			env:     map[string]string{config.DefaultMattermostTokenEnv: sharedToken},
			want:    sharedToken,
			wantEnv: config.DefaultMattermostTokenEnv,
		},
		{
			// The workstation that already exports the per-identity name keeps
			// working, which is the whole reason this name is still tried.
			name:    "the identity name wins over the shared default",
			env:     map[string]string{perIdentity: perIdentityToken, config.DefaultMattermostTokenEnv: sharedToken},
			want:    perIdentityToken,
			wantEnv: perIdentity,
		},
		{
			name:     "token_env wins over both",
			tokenEnv: secondBotTokenEnv,
			env: map[string]string{
				secondBotTokenEnv:                namedToken,
				perIdentity:                      perIdentityToken,
				config.DefaultMattermostTokenEnv: sharedToken,
			},
			want:    namedToken,
			wantEnv: secondBotTokenEnv,
		},
		{
			// A token_env naming an unset variable does not fall back, so the
			// identity stays off rather than posting as the wrong bot.
			name:     "token_env unset does not fall back",
			tokenEnv: secondBotTokenEnv,
			env:      map[string]string{config.DefaultMattermostTokenEnv: sharedToken},
		},
		{name: "nothing set"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			identity := config.MattermostIdentity{TokenEnv: testCase.tokenEnv}

			token, env := config.ResolveMattermostToken(&identity, identityOps,
				func(name string) string { return testCase.env[name] })

			if token != testCase.want || env != testCase.wantEnv {
				t.Fatalf("token/env = %q/%q, want %q/%q", token, env, testCase.want, testCase.wantEnv)
			}
		})
	}
}

// TestRunnerKeyEnvs covers the runner half. A block carries its own key so two
// openai-type blocks pointed at different vendors do not share one credential,
// and the per-type name stays behind it for hosts that set only that.
func TestRunnerKeyEnvs(t *testing.T) {
	t.Parallel()

	const openaiKey = "CAIRNOS_RUNNER_OPENAI_API_KEY"

	cases := []struct {
		name  string
		block string
		want  []string
	}{
		{
			name:  "a named block reads its own key first",
			block: credRunnerName,
			want:  []string{"CAIRNOS_RUNNER_ZAI_API_KEY", openaiKey},
		},
		{
			// Both rules produce the same string here, and naming it twice in
			// an error reads like a bug.
			name:  "a block named after its type collapses to one name",
			block: "openai",
			want:  []string{openaiKey},
		},
		{
			// The same mapping host names go through, so a dash and an
			// underscore cannot land on two different variables.
			name:  "a punctuated name goes through the host token rule",
			block: "z-ai",
			want:  []string{zaiKeyEnv, openaiKey},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := config.RunnerKeyEnvs(testCase.block, openaiKey); !slices.Equal(got, testCase.want) {
				t.Fatalf("key envs = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestLoadRefusesRunnerNamesThatShareOneKey covers the mapping's lossy half. Two
// spellings of one name render as one variable, and picking a winner would hand
// one vendor the other's credential with nothing at run time to say so.
func TestLoadRefusesRunnerNamesThatShareOneKey(t *testing.T) {
	t.Parallel()

	toml := "[runner.z-ai]\ntype = \"openai\"\nmodel = \"glm\"\n\n" +
		"[runner.z_ai]\ntype = \"openai\"\nmodel = \"glm\"\n"

	_, err := config.Load(writeConfig(t, toml))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("load error = %v, want one wrapping ErrInvalidConfig", err)
	}

	message := err.Error()
	for _, want := range []string{"runner.z-ai", "runner.z_ai", zaiKeyEnv} {
		if !strings.Contains(message, want) {
			t.Fatalf("load error %q does not mention %q", message, want)
		}
	}
}

// TestLoadKeepsRunnerNamesThatDiffer is the other side: two names that render
// as two variables are two runners, which is the whole point of the per-block
// key.
func TestLoadKeepsRunnerNamesThatDiffer(t *testing.T) {
	t.Parallel()

	toml := "[runner.zai]\ntype = \"openai\"\nmodel = \"glm\"\n\n" +
		"[runner.openai]\nmodel = \"gpt\"\n"

	cfg, err := config.Load(writeConfig(t, toml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Runners) != 2 {
		t.Fatalf("runners = %v, want both blocks kept", cfg.Runners)
	}
}
