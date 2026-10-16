package messaging_test

import (
	"testing"

	"github.com/chadit/CairnOS/internal/messaging"
)

// Each of these identities uses its name as its username too.
const (
	alertsName = "alerts"
	zedName    = "zed"
)

func TestRegistryAddAndGet(t *testing.T) {
	t.Parallel()

	registry := messaging.NewRegistry()

	if _, ok := registry.Get(identityName); ok {
		t.Fatal("Get on an empty registry reported a sender")
	}

	first := &fakeSender{identity: messaging.Identity{Name: identityName, Transport: transportMattermost}}
	registry.Add(identityName, first)

	got, ok := registry.Get(identityName)
	if !ok {
		t.Fatal("Get after Add reported no sender")
	}

	if got != first {
		t.Fatalf("Get returned %p, want the registered sender %p", got, first)
	}

	// A transport that rebuilds its client after a reconnect re-registers under
	// the same name; the stale sender must not survive.
	second := &fakeSender{identity: messaging.Identity{Name: identityName, Transport: transportMattermost}}
	registry.Add(identityName, second)

	if got, _ = registry.Get(identityName); got != second {
		t.Fatalf("Get after re-Add returned the stale sender %p, want %p", got, second)
	}
}

func TestRegistryListIsSortedByName(t *testing.T) {
	t.Parallel()

	registry := messaging.NewRegistry()

	want := []messaging.Identity{
		{Name: alertsName, Transport: transportMattermost, Username: alertsName, DisplayName: "Alerts", URL: serverURL},
		{Name: identityName, Transport: transportMattermost, Username: botUsername, DisplayName: botDisplayName, URL: serverURL},
		{Name: zedName, Transport: "telegram", Username: zedName, DisplayName: "Zed", URL: "https://t.example"},
	}

	// Insert out of order: a map iteration would otherwise reshuffle the
	// listing between calls, and a CLI listing has to be stable.
	for _, index := range []int{2, 0, 1} {
		registry.Add(want[index].Name, &fakeSender{identity: want[index]})
	}

	got := registry.List()
	if len(got) != len(want) {
		t.Fatalf("List() returned %d identities, want %d", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRegistryListOnEmptyRegistry(t *testing.T) {
	t.Parallel()

	// Serve creates the registry before any bridge connects, so an empty
	// listing is a normal state rather than a startup ordering bug.
	if got := messaging.NewRegistry().List(); len(got) != 0 {
		t.Fatalf("List() on an empty registry = %+v, want no identities", got)
	}
}
