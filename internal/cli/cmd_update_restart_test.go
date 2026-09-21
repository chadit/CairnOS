package cli_test

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chadit/CairnOS/internal/cli"
)

// restartRecorder stands in for the two outside-world halves of the restart:
// the drain request to the running service and the supervisor restart behind
// it. drainErr is what an older build's missing endpoint looks like.
type restartRecorder struct {
	drainErr  error
	inFlight  int
	drained   bool
	restarted bool
}

func (r *restartRecorder) hooks() cli.UpdateRestartHooks {
	return cli.UpdateRestartHooks{
		Drain: func(context.Context) (int, error) {
			r.drained = true

			if r.drainErr != nil {
				return 0, r.drainErr
			}

			return r.inFlight, nil
		},
		Restart: func(context.Context) error {
			r.restarted = true

			return nil
		},
	}
}

// TestUpdateRestartPrefersTheDrain covers the three ways `cairnos update` can
// turn a service over: the drain it wants, the supervisor restart it falls back
// to when the running build has no such endpoint, and the one --restart-now
// asks for outright.
func TestUpdateRestartPrefersTheDrain(t *testing.T) {
	t.Parallel()

	unimplemented := status.Error(codes.Unimplemented,
		"unknown method RequestRestart for service cairnos.v1.AdminService")

	cases := []struct {
		drainErr    error
		name        string
		restartNow  bool
		wantDrained bool
		wantRestart bool
	}{
		{
			name:        "a build that answers drains",
			wantDrained: true, wantRestart: false,
		},
		{
			name:        "a build that predates the endpoint restarts",
			drainErr:    unimplemented,
			wantDrained: false, wantRestart: true,
		},
		{
			name:        "restart-now skips the drain entirely",
			restartNow:  true,
			wantDrained: false, wantRestart: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			recorder := &restartRecorder{drainErr: testCase.drainErr, inFlight: 2}
			app := cli.New(&bytes.Buffer{}, &bytes.Buffer{})

			drained, err := app.UpdateDriveRestart(t.Context(), recorder.hooks(), testCase.restartNow)
			if err != nil {
				t.Fatalf("UpdateDriveRestart: %v", err)
			}

			if drained != testCase.wantDrained {
				t.Errorf("drained = %t, want %t", drained, testCase.wantDrained)
			}

			if recorder.restarted != testCase.wantRestart {
				t.Errorf("supervisor restart ran = %t, want %t", recorder.restarted, testCase.wantRestart)
			}

			// --restart-now must not even ask, since asking would drain a
			// service the operator wanted replaced now.
			if wantAsked := !testCase.restartNow; recorder.drained != wantAsked {
				t.Errorf("drain requested = %t, want %t", recorder.drained, wantAsked)
			}
		})
	}
}
