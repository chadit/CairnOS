package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/config"
)

// configTemplate is the commented config init writes. The verbs are filled
// from config.Default() so this file and the binary never disagree about
// defaults.
const configTemplate = `# CairnOS configuration.
#
# Every key can be overridden by an environment variable named
# CAIRNOS_<SECTION>_<KEY>: engine.max_concurrent_actions becomes
# CAIRNOS_ENGINE_MAX_CONCURRENT_ACTIONS, store.dsn becomes CAIRNOS_STORE_DSN.
# The environment wins over this file.
#
# Secret values never live here. A ${{ secrets.name }} reference reads the
# CAIRNOS_SECRET_NAME environment variable by default; set [secrets] backend =
# "store" to also resolve from the encrypted store (env still wins).

[engine]
max_concurrent_actions = %d     # in-flight runner calls across the engine
default_timeout        = "%s"   # ceiling for actions without their own

[engine.retry]
# Engine-wide retry defaults; actions override them per file.
max_attempts = %d
backoff      = "%s"             # exponential | constant
base_delay   = "%s"
max_delay    = "%s"
jitter       = %t

[store]
backend = "%s"                  # sqlite (postgres arrives in a later phase)
dsn     = "%s"                  # relative paths resolve against this directory

[secrets]
backend = "%s"                  # env (default; CAIRNOS_SECRET_<NAME>) | store (encrypted store, env first)

[memory]
backend = "%s"                  # sqlite keyword search; vector backends come later
vector  = %t

[retention]
runs    = "%s"                  # delete runs and action results older than this
outputs = "%s"                  # truncate large action outputs sooner
sweep   = "%s"                  # how often the background prune runs

[update]
auto     = %t                   # serve installs a newer build and restarts on it
interval = "%s"                 # how often serve checks for one

[env]
# Environment variable names workflows may read; empty means no env access.
allowlist = []

[telemetry]
enabled    = %t
prometheus = "%s"               # /metrics bind, empty to disable

[api]
enabled = %t                    # cairnos serve turns this on (Phase 1)
grpc    = "%s"
http    = "%s"

[auth]
enabled = %t

[hive]
enabled = %t
`

// helloWorkflow is the out-of-the-box example: a two-action DAG showing
// depends_on ordering and output interpolation, runnable with no external
// services because both actions use the mock runner.
const helloWorkflow = `# hello.toml: the smallest useful CairnOS workflow.
#
# Run it:
#   cairnos run examples/hello.toml
# Override the input:
#   cairnos run examples/hello.toml --input name=cairn

[workflow]
name        = "hello"
description = "Greet someone, then echo the greeting through a second action."

[workflow.inputs.name]
type        = "string"
default     = "world"
description = "Who to greet."

[action.greet]
runner = "mock"
params = { output = "hello, ${{ inputs.name }}" }

[action.echo]
runner     = "mock"
depends_on = ["greet"]
params     = { output = "echo: ${{ greet.output }}" }
`

// exampleSkill is the out-of-the-box skill: drop-in Markdown a chat turn can
// invoke with /summarize, or the driver can load by its description. The
// frontmatter carries the name and description; everything after it is the
// body injected into the driver prompt.
const exampleSkill = `---
name: summarize
description: Summarize the conversation into five bullets.
---
Summarize the conversation so far into at most five concise bullet points,
each a single sentence. Lead with the most important decision or open
question, and skip pleasantries and restated context.
`

// exampleSkillFileName is the example skill init writes under skills/.
const exampleSkillFileName = "summarize.md"

func (a *App) cmdInit(args []string) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	dirFlag := dirFlagOn(flags)

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	dir, err := resolveDir(*dirFlag, a.envLookup())
	if err != nil {
		a.printErrf("init: %v\n", err)

		return ExitFailure
	}

	if err := scaffold(dir); err != nil {
		a.printErrf("init: %v\n", err)

		return ExitFailure
	}

	a.printOutf("initialized %s\n", dir)
	a.printOutf("try: cairnos run %s --dir %s\n",
		filepath.Join(dir, examplesDirName, helloFileName), dir)

	return ExitOK
}

// scaffold creates the init layout: config.toml, data/, workflows/, examples/,
// skills/, and context/ (per-workspace chat context docs). Existing files are
// kept so re-running init never clobbers an edited config or skill.
func scaffold(dir string) error {
	for _, sub := range []string{dataDirName, workflowsDirName, examplesDirName, skillsDirName, contextDirName} {
		if err := os.MkdirAll(filepath.Join(dir, sub), dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	if err := writeIfAbsent(filepath.Join(dir, configFileName), renderConfig()); err != nil {
		return err
	}

	if err := writeIfAbsent(filepath.Join(dir, skillsDirName, exampleSkillFileName), exampleSkill); err != nil {
		return err
	}

	return writeIfAbsent(filepath.Join(dir, examplesDirName, helloFileName), helloWorkflow)
}

// writeIfAbsent writes content unless the path already exists, keeping init
// safe to re-run.
func writeIfAbsent(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// renderConfig fills the template from the compiled-in defaults.
func renderConfig() string {
	def := config.Default()

	return fmt.Sprintf(
		configTemplate,
		def.Engine.MaxConcurrentActions,
		formatDuration(def.Engine.DefaultTimeout),
		def.Engine.Retry.MaxAttempts,
		def.Engine.Retry.Backoff,
		formatDuration(def.Engine.Retry.BaseDelay),
		formatDuration(def.Engine.Retry.MaxDelay),
		def.Engine.Retry.Jitter,
		def.Store.Backend,
		def.Store.DSN,
		def.Secrets.Backend,
		def.Memory.Backend,
		def.Memory.Vector,
		formatDuration(def.Retention.Runs),
		formatDuration(def.Retention.Outputs),
		formatDuration(def.Retention.Sweep),
		def.Update.Auto,
		formatDuration(def.Update.Interval),
		def.Telemetry.Enabled,
		def.Telemetry.Prometheus,
		def.API.Enabled,
		def.API.GRPC,
		def.API.HTTP,
		def.Auth.Enabled,
		def.Hive.Enabled,
	)
}

// formatDuration renders a duration the way the spec's config examples do:
// whole days as "90d", which time.Duration.String cannot produce, otherwise
// the standard form with trailing zero units trimmed ("10m", not "10m0s").
func formatDuration(dur time.Duration) string {
	const day = 24 * time.Hour

	if dur >= day && dur%day == 0 {
		return strconv.FormatInt(int64(dur/day), 10) + "d"
	}

	text := dur.String()

	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}

	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}

	return text
}
