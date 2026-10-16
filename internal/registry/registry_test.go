package registry_test

import (
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/registry"
)

const (
	nameAlpha = "alpha"
	nameBravo = "bravo"
)

func TestTableZeroValueIsEmpty(t *testing.T) {
	t.Parallel()

	var table registry.Table[int]

	if _, ok := table.Get(nameAlpha); ok {
		t.Fatal("Get on a zero table reported a value")
	}

	if names := table.List(); len(names) != 0 {
		t.Fatalf("List on a zero table = %v, want none", names)
	}

	if snapshot := table.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("Snapshot on a zero table = %v, want none", snapshot)
	}
}

func TestTableAddReplacesEarlierValue(t *testing.T) {
	t.Parallel()

	var table registry.Table[int]

	table.Add(nameAlpha, 1)
	table.Add(nameAlpha, 2)

	got, ok := table.Get(nameAlpha)
	if !ok {
		t.Fatal("Get after Add reported no value")
	}

	if got != 2 {
		t.Fatalf("Get = %d, want the later value 2", got)
	}

	if names := table.List(); !slices.Equal(names, []string{nameAlpha}) {
		t.Fatalf("List = %v, want one entry after a replace", names)
	}
}

func TestTableListIsSorted(t *testing.T) {
	t.Parallel()

	var table registry.Table[string]

	table.Add(nameBravo, "b")
	table.Add(nameAlpha, "a")

	if names := table.List(); !slices.Equal(names, []string{nameAlpha, nameBravo}) {
		t.Fatalf("List = %v, want [%s %s]", names, nameAlpha, nameBravo)
	}
}

func TestTableSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	var table registry.Table[int]

	table.Add(nameAlpha, 1)

	snapshot := table.Snapshot()
	snapshot[nameBravo] = 2
	snapshot[nameAlpha] = 3

	if _, ok := table.Get(nameBravo); ok {
		t.Fatal("a key added to the snapshot reached the table")
	}

	if got, _ := table.Get(nameAlpha); got != 1 {
		t.Fatalf("Get = %d after editing the snapshot, want the stored 1", got)
	}
}

// The race detector is the assertion here: concurrent Add, Get, List, and
// Snapshot calls must not race on the map.
func TestTableConcurrentUse(t *testing.T) {
	t.Parallel()

	const workers = 8

	var (
		table registry.Table[int]
		group sync.WaitGroup
	)

	for worker := range workers {
		group.Go(func() {
			name := strconv.Itoa(worker)
			table.Add(name, worker)
			table.Get(name)
			table.List()
			table.Snapshot()
		})
	}

	group.Wait()

	if names := table.List(); len(names) != workers {
		t.Fatalf("List has %d names after %d concurrent adds, want %d", len(names), workers, workers)
	}
}
