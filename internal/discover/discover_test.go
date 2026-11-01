package discover_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/discover"
	"github.com/chadit/CairnOS/internal/scheduler"
	"github.com/chadit/CairnOS/internal/source"
)

// Fixture values: the source every case reconciles, its glob, and the cron
// expressions a file starts with and is edited to.
const (
	treeName   = "automation"
	treeRef    = "main"
	glob       = "engines/*/cairnos/*.toml"
	hourly     = "@hourly"
	daily      = "@daily"
	fileMode   = 0o600
	dirMode    = 0o750
	syncRel    = "engines/garmin/cairnos/sync.toml"
	digestRel  = "engines/mail/cairnos/digest.toml"
	syncName   = "garmin_sync"
	digestName = "mail_digest"
)

// errNoCommit is what the fake answers for any git call other than the one
// discovery makes, so a case that drifts from that call fails loudly.
var errNoCommit = errors.New("unexpected git call")

// fixture is one checkout on disk, the files the fake git reports as committed
// at HEAD, and a real schedule store.
type fixture struct {
	store     *scheduler.SQLite
	logs      *bytes.Buffer
	dest      string
	committed []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store, err := scheduler.OpenSQLite(filepath.Join(t.TempDir(), "schedules.db"))
	if err != nil {
		t.Fatalf("open schedule store: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close schedule store: %v", closeErr)
		}
	})

	return &fixture{store: store, logs: &bytes.Buffer{}, dest: t.TempDir()}
}

// git answers the calls CommittedFiles makes: the ref lookup finds treeRef as
// an origin branch and nothing else, and the listing returns the committed
// set, which is how a case says what origin carries independent of the disk.
func (f *fixture) git(_ context.Context, _ string, argv ...string) (string, error) {
	switch {
	case len(argv) > 0 && argv[0] == "rev-parse" && argv[len(argv)-1] == "refs/remotes/origin/"+treeRef:
		return "", nil
	case len(argv) > 0 && argv[0] == "ls-tree":
		return strings.Join(f.committed, "\x00"), nil
	default:
		return "", errNoCommit
	}
}

// write puts a workflow file on disk without committing it.
func (f *fixture) write(t *testing.T, rel, body string) string {
	t.Helper()

	path := filepath.Join(f.dest, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(body), fileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// commit writes a workflow file and lists it as committed at HEAD.
func (f *fixture) commit(t *testing.T, rel, body string) string {
	t.Helper()

	f.committed = append(f.committed, rel)

	return f.write(t, rel, body)
}

// reconcile runs one pass and fails the case on error.
func (f *fixture) reconcile(t *testing.T) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(f.logs, nil))
	tree := discover.Tree{Name: treeName, Dest: f.dest, Ref: treeRef, Globs: []string{glob}}

	if err := discover.Reconcile(t.Context(), f.store, source.Git(f.git), tree, logger); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// schedules lists the store, failing the case on error.
func (f *fixture) schedules(t *testing.T) []scheduler.Schedule {
	t.Helper()

	scheds, err := f.store.List(t.Context())
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}

	return scheds
}

// add stores an entry the way `schedule add` would, outside discovery.
func (f *fixture) add(t *testing.T, path, name, cron string) {
	t.Helper()

	if _, err := f.store.Add(t.Context(), &scheduler.Schedule{WorkflowPath: path, WorkflowName: name, Cron: cron}); err != nil {
		t.Fatalf("add schedule: %v", err)
	}
}

// scheduled builds a minimal workflow file with an optional schedule key.
func scheduled(name, cron string) string {
	body := "[workflow]\nname = \"" + name + "\"\n"
	if cron != "" {
		body += "schedule = \"" + cron + "\"\n"
	}

	return body + "\n[action.tick]\nrunner = \"mock\"\nparams = { output = \"tick\" }\n"
}

// wantOnly asserts the store holds exactly one entry per given path, with the
// cron each maps to.
func wantOnly(t *testing.T, scheds []scheduler.Schedule, want map[string]string) {
	t.Helper()

	if len(scheds) != len(want) {
		t.Fatalf("store holds %d schedules, want %d: %+v", len(scheds), len(want), scheds)
	}

	for idx := range scheds {
		sched := &scheds[idx]

		cron, ok := want[sched.WorkflowPath]
		if !ok {
			t.Fatalf("unexpected schedule for %s", sched.WorkflowPath)
		}

		if sched.Cron != cron {
			t.Fatalf("schedule for %s has cron %q, want %q", sched.WorkflowPath, sched.Cron, cron)
		}
	}
}

func TestReconcileRegistersNewFile(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	path := fix.commit(t, syncRel, scheduled(syncName, hourly))

	fix.reconcile(t)

	scheds := fix.schedules(t)
	wantOnly(t, scheds, map[string]string{path: hourly})

	if scheds[0].WorkflowName != syncName {
		t.Fatalf("workflow name = %q, want %q", scheds[0].WorkflowName, syncName)
	}

	if !strings.Contains(fix.logs.String(), "workflow scheduled") {
		t.Fatalf("no INFO line for the new schedule:\n%s", fix.logs)
	}
}

func TestReconcileReplacesEditedCron(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	path := fix.commit(t, syncRel, scheduled(syncName, hourly))
	fix.reconcile(t)

	fix.write(t, syncRel, scheduled(syncName, daily))
	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{path: daily})
}

func TestReconcileUnregistersRemovedFile(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	fix.commit(t, syncRel, scheduled(syncName, hourly))
	digest := fix.commit(t, digestRel, scheduled(digestName, daily))
	fix.reconcile(t)

	fix.committed = []string{digestRel}
	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{digest: daily})
}

func TestReconcileIgnoresFileOnlyOnDisk(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	fix.write(t, syncRel, scheduled(syncName, hourly))

	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{})
}

func TestReconcileLeavesHandAddedEntriesOutsideGlobs(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	elsewhere := fix.write(t, "jobs/backup.toml", scheduled("backup", daily))
	outside := filepath.Join(t.TempDir(), "other.toml")

	fix.add(t, elsewhere, "backup", daily)
	fix.add(t, outside, "other", hourly)

	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{elsewhere: daily, outside: hourly})
}

func TestReconcileAdoptsHandAddedEntryForSamePath(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	path := fix.commit(t, syncRel, scheduled(syncName, hourly))
	fix.add(t, path, syncName, hourly)

	before := fix.schedules(t)

	fix.reconcile(t)

	after := fix.schedules(t)
	wantOnly(t, after, map[string]string{path: hourly})

	if after[0].ID != before[0].ID {
		t.Fatalf("hand-added entry %s replaced by %s, want it kept", before[0].ID, after[0].ID)
	}
}

// TestReconcileAdoptsHandAddedEntryThroughSymlink covers `schedule add` given
// another spelling of the checkout, such as /tmp against /private/tmp on
// macOS: the entry is the same file, so it is adopted, not duplicated.
func TestReconcileAdoptsHandAddedEntryThroughSymlink(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	path := fix.commit(t, syncRel, scheduled(syncName, hourly))

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(fix.dest, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fix.add(t, filepath.Join(alias, filepath.FromSlash(syncRel)), syncName, hourly)
	before := fix.schedules(t)

	fix.reconcile(t)

	after := fix.schedules(t)
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("schedules after reconcile = %+v, want only the hand-added entry %s for %s", after, before[0].ID, path)
	}
}

func TestReconcileSkipsBadFilesAndRegistersTheRest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{name: "does not parse", body: "[workflow\nname = "},
		{name: "required input with no default", body: "[workflow]\nname = \"bad\"\nschedule = \"@hourly\"\n\n" +
			"[workflow.inputs.target]\ntype = \"string\"\nrequired = true\n\n" +
			"[action.tick]\nrunner = \"mock\"\nparams = { output = \"tick\" }\n"},
		{name: "fires faster than the floor", body: scheduled("bad", "* * * * * *")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fix := newFixture(t)
			bad := fix.commit(t, syncRel, testCase.body)
			digest := fix.commit(t, digestRel, scheduled(digestName, daily))

			// The bad file had a working entry before this push; skipping it keeps
			// that entry rather than unscheduling it.
			fix.add(t, bad, syncName, hourly)

			fix.reconcile(t)

			wantOnly(t, fix.schedules(t), map[string]string{bad: hourly, digest: daily})

			logs := fix.logs.String()
			if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, bad) {
				t.Fatalf("no WARN line naming %s:\n%s", bad, logs)
			}
		})
	}
}

func TestReconcileUnregistersFileThatLostItsScheduleKey(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	path := fix.commit(t, syncRel, scheduled(syncName, hourly))
	fix.reconcile(t)
	wantOnly(t, fix.schedules(t), map[string]string{path: hourly})

	fix.write(t, syncRel, scheduled(syncName, ""))
	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{})
}

func TestReconcileNeverRegistersFileWithoutScheduleKey(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	fix.commit(t, syncRel, scheduled(syncName, ""))

	fix.reconcile(t)

	wantOnly(t, fix.schedules(t), map[string]string{})
}

func TestReconcileWithoutGlobsTouchesNothing(t *testing.T) {
	t.Parallel()

	fix := newFixture(t)
	fix.commit(t, syncRel, scheduled(syncName, hourly))

	tree := discover.Tree{Name: treeName, Dest: fix.dest, Ref: treeRef}
	if err := discover.Reconcile(t.Context(), fix.store, source.Git(fix.git), tree, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	wantOnly(t, fix.schedules(t), map[string]string{})
}
