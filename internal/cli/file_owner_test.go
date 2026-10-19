package cli_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/cli"
)

// The chown and refusal branches need a file another user owns, which a test
// run without root cannot create, so the decision is checked on its own.
func TestKeepOwnerPlan(t *testing.T) {
	t.Parallel()

	const (
		serveUID = 990
		otherUID = 1000
	)

	cases := []struct {
		wantErr   error
		name      string
		ownerUID  int
		euid      int
		wantChown bool
	}{
		{name: "owner edits its own file", ownerUID: serveUID, euid: serveUID},
		{name: "root edits its own file", ownerUID: 0, euid: 0},
		{name: "root edits the serve user's file", ownerUID: serveUID, euid: 0, wantChown: true},
		{name: "another user edits the serve user's file", ownerUID: serveUID, euid: otherUID, wantErr: cli.ErrForeignOwner},
		{name: "a user edits a root-owned file", ownerUID: 0, euid: otherUID, wantErr: cli.ErrForeignOwner},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			chown, err := cli.KeepOwnerPlan("/home/cairnos/.cairnos/config.toml", testCase.ownerUID, testCase.euid)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error = %v, want %v", err, testCase.wantErr)
			}

			if chown != testCase.wantChown {
				t.Fatalf("chown = %t, want %t", chown, testCase.wantChown)
			}
		})
	}
}
