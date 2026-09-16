package cairnerr_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/cairnerr"
)

// errCause stands in for whatever a wrapped failure happened to be.
var errCause = errors.New("a dial that timed out")

func TestEqualComparesTheWireFormOnly(t *testing.T) {
	t.Parallel()

	const (
		code    = "runner.flaky"
		message = "second attempt failed"
	)

	cases := []struct {
		first  *cairnerr.Error
		second *cairnerr.Error
		name   string
		want   bool
	}{
		{name: "both absent", want: true},
		{name: "one absent", second: cairnerr.New(code, message)},
		{
			name:   "same code and message",
			first:  cairnerr.New(code, message),
			second: cairnerr.New(code, message),
			want:   true,
		},
		{
			// The reason this function exists: a value read back out of a store
			// has no cause, and the one that went in may have had one. They
			// describe the same failure.
			name:   "same wire form, different cause",
			first:  cairnerr.Wrapf(errCause, code, "%s", message),
			second: cairnerr.New(code, message),
			want:   true,
		},
		{
			name:   "different code",
			first:  cairnerr.New("runner.timeout", message),
			second: cairnerr.New(code, message),
		},
		{
			name:   "different message",
			first:  cairnerr.New(code, "first attempt failed"),
			second: cairnerr.New(code, message),
		},
		{
			name:   "same details",
			first:  cairnerr.New(code, message).WithDetail("attempt", "2"),
			second: cairnerr.New(code, message).WithDetail("attempt", "2"),
			want:   true,
		},
		{
			// A store decodes a number back as whatever its codec chose, so the
			// details are compared as they read rather than by dynamic type.
			name:   "a detail that changed type in transit",
			first:  cairnerr.New(code, message).WithDetail("attempt", 2),
			second: cairnerr.New(code, message).WithDetail("attempt", "2"),
			want:   true,
		},
		{
			name:   "different details",
			first:  cairnerr.New(code, message).WithDetail("attempt", "2"),
			second: cairnerr.New(code, message).WithDetail("attempt", "3"),
		},
		{
			name:   "a detail the other does not carry",
			first:  cairnerr.New(code, message).WithDetail("attempt", "2"),
			second: cairnerr.New(code, message),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := cairnerr.Equal(testCase.first, testCase.second); got != testCase.want {
				t.Fatalf("Equal(%v, %v) = %t, want %t",
					testCase.first, testCase.second, got, testCase.want)
			}
		})
	}
}

// TestEqualLeavesTheChainReachable guards the other half of the contract: the
// cause is ignored for comparison, not dropped, so errors.Is still finds it.
func TestEqualLeavesTheChainReachable(t *testing.T) {
	t.Parallel()

	wrapped := cairnerr.Wrapf(errCause, "runner.flaky", "second attempt failed")

	if !errors.Is(wrapped, errCause) {
		t.Fatal("the wrapped cause is no longer reachable with errors.Is")
	}
}
