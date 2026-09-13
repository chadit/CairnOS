package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
)

// Report answers one question: are the compiled-in pins behind upstream? The
// JSON encoding is a published contract that a CI job parses, so field names
// and nesting are fixed.
type Report struct {
	Runtime RuntimeReport `json:"runtime"`
	Models  []ModelReport `json:"models"`
}

// RuntimeReport compares the pinned llama.cpp release against the newest one.
type RuntimeReport struct {
	Name        string `json:"name"`
	Pinned      string `json:"pinned"`
	PinnedBuild string `json:"pinned_build"`
	Latest      string `json:"latest"`
	LatestBuild string `json:"latest_build"`
	Outdated    bool   `json:"outdated"`
	URL         string `json:"url"`
}

// ModelReport compares one pinned GGUF against its upstream repository. The
// digest decides outdated, not the revision: a README commit moves the
// revision every time and says nothing about the weights.
type ModelReport struct {
	Name           string `json:"name"`
	Repo           string `json:"repo"`
	File           string `json:"file"`
	PinnedRevision string `json:"pinned_revision"`
	LatestRevision string `json:"latest_revision"`
	PinnedSHA256   string `json:"pinned_sha256"`
	LatestSHA256   string `json:"latest_sha256"`
	Outdated       bool   `json:"outdated"`
	URL            string `json:"url"`
}

// Check compares every pin against upstream. A failed lookup is returned as an
// error rather than reported as up to date, because a check that cannot reach
// upstream has learned nothing and must not read as a clean bill of health.
func Check(ctx context.Context, upstream *Upstream) (Report, error) {
	runtimeReport, err := checkRuntime(ctx, upstream)
	if err != nil {
		return Report{}, err
	}

	entries := Models()
	models := make([]ModelReport, 0, len(entries))

	for idx := range entries {
		report, err := checkModel(ctx, upstream, &entries[idx])
		if err != nil {
			return Report{}, err
		}

		models = append(models, report)
	}

	return Report{Runtime: runtimeReport, Models: models}, nil
}

// checkRuntime resolves the newest llama.cpp release and compares it with the
// pin. Either half moving counts: a stable tag can be re-cut against a new
// build, and a build can appear under a tag already released.
func checkRuntime(ctx context.Context, upstream *Upstream) (RuntimeReport, error) {
	latest, err := upstream.Runtime(ctx)
	if err != nil {
		return RuntimeReport{}, err
	}

	return RuntimeReport{
		Name:        RuntimeName,
		Pinned:      PinnedVersion,
		PinnedBuild: PinnedBuild,
		Latest:      latest.Version,
		LatestBuild: latest.Build,
		Outdated:    latest.Version != PinnedVersion || latest.Build != PinnedBuild,
		URL:         latest.URL,
	}, nil
}

// checkModel resolves one repository's head revision and the pinned file's
// digest at it.
func checkModel(ctx context.Context, upstream *Upstream, model *Model) (ModelReport, error) {
	latest, err := upstream.Model(ctx, model.Repo, model.File)
	if err != nil {
		return ModelReport{}, err
	}

	return ModelReport{
		Name:           model.Name,
		Repo:           model.Repo,
		File:           model.File,
		PinnedRevision: model.Revision,
		LatestRevision: latest.Revision,
		PinnedSHA256:   model.SHA256,
		LatestSHA256:   latest.SHA256,
		Outdated:       latest.SHA256 != model.SHA256,
		URL:            model.CommitsURL(),
	}, nil
}

// JSON renders the report in the shape the CI job parses.
func (r *Report) JSON() (string, error) {
	raw, err := json.MarshalIndent(r, "", jsonIndent)
	if err != nil {
		return "", fmt.Errorf("encode report: %w", err)
	}

	return string(raw) + "\n", nil
}

// Lines renders one human-readable line per component, for the plain mode of
// the check command.
func (r *Report) Lines() []string {
	lines := make([]string, 0, 1+len(r.Models))
	lines = append(lines, fmt.Sprintf("%s: pinned %s (%s), latest %s (%s) %s",
		r.Runtime.Name, r.Runtime.Pinned, r.Runtime.PinnedBuild,
		r.Runtime.Latest, r.Runtime.LatestBuild, statusWord(r.Runtime.Outdated)))

	for idx := range r.Models {
		model := &r.Models[idx]
		lines = append(lines, fmt.Sprintf("%s: pinned %s, latest %s %s",
			model.Name, ShortDigest(model.PinnedSHA256), ShortDigest(model.LatestSHA256),
			statusWord(model.Outdated)))
	}

	return lines
}

// Outdated reports whether anything in the report is behind upstream, so a
// caller can decide whether to say more than "everything matches".
func (r *Report) Outdated() bool {
	if r.Runtime.Outdated {
		return true
	}

	for idx := range r.Models {
		if r.Models[idx].Outdated {
			return true
		}
	}

	return false
}

// digestPrefixLen is how much of a SHA-256 a human line shows: enough to tell
// two digests apart at a glance, short enough to keep the line readable.
const digestPrefixLen = 12

// ShortDigest truncates a digest for human output, leaving a short or empty
// one alone. Status lines and check lines both print digests, and a full
// SHA-256 buries the rest of the line.
func ShortDigest(digest string) string {
	if len(digest) <= digestPrefixLen {
		return digest
	}

	return digest[:digestPrefixLen]
}

// statusWord is the trailing verdict on a human line.
func statusWord(outdated bool) string {
	if outdated {
		return "(update available)"
	}

	return "(up to date)"
}
