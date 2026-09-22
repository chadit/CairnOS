package cli_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/messaging"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store/sqlite"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Fixture values the adapter cases share.
const (
	adapterIdentity = "ops"
	adapterTarget   = "#usage"
	adapterKey      = "usage:build_box"
	adapterOldRoot  = "post-deleted"
	adapterNewRoot  = "post-fresh"
)

// errReplyReset is an unrelated reply failure, which must not open a new thread.
var errReplyReset = errors.New("connection reset")

// replyFailingSender is a messaging.Sender whose replies fail with replyErr and
// whose root posts succeed, recording the thread each send named.
type replyFailingSender struct {
	replyErr error
	threads  []string
}

// Send fails a reply with the configured error and answers a root post with a fresh id.
func (s *replyFailingSender) Send(_ context.Context, _ messaging.Target, _, threadID string) (messaging.PostRef, error) {
	s.threads = append(s.threads, threadID)

	if threadID != "" {
		return messaging.PostRef{}, s.replyErr
	}

	return messaging.PostRef{PostID: adapterNewRoot}, nil
}

// Describe names the fake identity.
func (*replyFailingSender) Describe() messaging.Identity {
	return messaging.Identity{Name: adapterIdentity}
}

// keyedAdapterPost binds the old root, then runs one thread_key post through the
// serve adapter over the given sender, returning the store and the run error.
func keyedAdapterPost(t *testing.T, sender messaging.Sender) (*sqlite.Store, error) {
	t.Helper()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if bindErr := db.BindMessageThread(t.Context(), adapterIdentity, adapterTarget, adapterKey, adapterOldRoot); bindErr != nil {
		t.Fatalf("bind old root: %v", bindErr)
	}

	registry := messaging.NewRegistry()
	registry.Add(adapterIdentity, sender)

	built, err := builtins.MessageFactory(cli.NewMessageSender(registry, slog.New(slog.DiscardHandler)), db)(nil)
	if err != nil {
		t.Fatalf("build message runner: %v", err)
	}

	_, runErr := built.Execute(t.Context(), &runner.Task{
		RunID: "run-1", ActionName: "post", Resolved: true,
		Params: map[string]any{
			"identity": adapterIdentity, "to": adapterTarget, "text": "report", "thread_key": adapterKey,
		},
	})
	if runErr != nil {
		return db, fmt.Errorf("execute keyed post: %w", runErr)
	}

	return db, nil
}

func TestServeSenderTurnsAMissingRootIntoAFreshThread(t *testing.T) {
	t.Parallel()

	sender := &replyFailingSender{replyErr: fmt.Errorf("post: %w", messaging.ErrThreadNotFound)}

	db, err := keyedAdapterPost(t, sender)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(sender.threads) != 2 || sender.threads[0] != adapterOldRoot || sender.threads[1] != "" {
		t.Fatalf("sends named threads %q, want a reply on %s and then a root post", sender.threads, adapterOldRoot)
	}

	rootID, err := db.LookupMessageThread(t.Context(), adapterIdentity, adapterTarget, adapterKey)
	if err != nil || rootID != adapterNewRoot {
		t.Fatalf("stored root = %q (%v), want %q", rootID, err, adapterNewRoot)
	}
}

func TestServeSenderKeepsTheThreadOnAnUnrelatedFailure(t *testing.T) {
	t.Parallel()

	sender := &replyFailingSender{replyErr: errReplyReset}

	db, err := keyedAdapterPost(t, sender)
	if !errors.Is(err, errReplyReset) {
		t.Fatalf("Execute error = %v, want the reply failure", err)
	}

	if len(sender.threads) != 1 {
		t.Fatalf("sends named threads %q, want only the failed reply", sender.threads)
	}

	rootID, err := db.LookupMessageThread(t.Context(), adapterIdentity, adapterTarget, adapterKey)
	if err != nil || rootID != adapterOldRoot {
		t.Fatalf("stored root = %q (%v), want the old root %q kept", rootID, err, adapterOldRoot)
	}
}
