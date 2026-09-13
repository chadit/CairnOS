package bridgetest

import "sync"

// Label is the transport and identity pair every bridge counter carries (FR42).
type Label struct {
	Transport string
	Identity  string
}

// Metrics is a bridge.Metrics recorder that keeps the labels as well as the
// counts, so a test proves the transport and identity actually reach telemetry
// rather than only that a counter moved.
type Metrics struct {
	updates      map[Label]map[string]int
	confirms     map[Label]map[string]int
	sendFailures []Label
	turnsOK      int
	turnsError   int
	mu           sync.Mutex
}

// NewMetrics builds a zeroed recorder.
func NewMetrics() *Metrics {
	return &Metrics{
		updates:  map[Label]map[string]int{},
		confirms: map[Label]map[string]int{},
	}
}

// Update counts one inbound outcome under its labels.
func (m *Metrics) Update(transport, identity, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	countIn(m.updates, Label{Transport: transport, Identity: identity}, outcome)
}

// Turn counts one delivered turn. The labels are covered by the other three
// counters, so this one keeps just the tallies the turn assertions read.
func (m *Metrics) Turn(_, _ string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ok {
		m.turnsOK++

		return
	}

	m.turnsError++
}

// Confirm counts one resolved confirmation under its labels.
func (m *Metrics) Confirm(transport, identity, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	countIn(m.confirms, Label{Transport: transport, Identity: identity}, outcome)
}

// SendFailure records one failed outbound send with its labels.
func (m *Metrics) SendFailure(transport, identity string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sendFailures = append(m.sendFailures, Label{Transport: transport, Identity: identity})
}

// countIn bumps one outcome under one label set, creating the inner map on first
// contact so the callers stay two lines each.
func countIn(counts map[Label]map[string]int, label Label, outcome string) {
	byOutcome, ok := counts[label]
	if !ok {
		byOutcome = map[string]int{}
		counts[label] = byOutcome
	}

	byOutcome[outcome]++
}

// UpdateCount reads one update outcome count for one transport and identity.
func (m *Metrics) UpdateCount(transport, identity, outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.updates[Label{Transport: transport, Identity: identity}][outcome]
}

// ConfirmCount reads one confirm outcome count for one transport and identity.
func (m *Metrics) ConfirmCount(transport, identity, outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.confirms[Label{Transport: transport, Identity: identity}][outcome]
}

// TurnCounts reads the ok and error turn tallies.
func (m *Metrics) TurnCounts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.turnsOK, m.turnsError
}

// SendFailures returns the labels every recorded send failure carried, in order.
func (m *Metrics) SendFailures() []Label {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]Label(nil), m.sendFailures...)
}
