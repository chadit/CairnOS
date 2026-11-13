// Package skills is the operator's drop-in skill library: Markdown documents
// under <home>/skills/ that a chat turn can inject by name. A Library scans one
// directory freshly on every call, the same no-cache freshness contract
// api.WorkflowResolver.Resolve gives workflows, so dropping or editing a file
// applies on the next chat turn with no restart and no re-registration.
//
// # Trust posture
//
// Skill files are operator-authored local configuration under the CairnOS
// home, the same trust class as workflow TOML and chat personalities. They are
// never remote content; nothing here fetches, syncs, or accepts a skill over
// the network. A skill body influences prompt text only: it cannot add tools,
// raise budgets, widen memory scopes, or move the confirmation gate. A
// hostile skill file means a compromised operator machine, which sits outside
// the threat model, exactly like a hostile workflow file.
package skills

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Byte caps applied at load, so a runaway file cannot dominate a prompt. The
// shift on the body cap is byte-size arithmetic (16 * 1024).
const (
	maxSkillBodyBytes        = 16 << 10 // 16 KiB
	maxSkillDescriptionBytes = 256
)

// Frontmatter grammar tokens and the file extension the scan recognizes.
const (
	frontmatterFence = "---"
	skillFileSuffix  = ".md"
	keyName          = "name"
	keyDescription   = "description"
)

// truncationMarker trails a value clipped to its cap, the same shape chat's
// truncateResult uses, so a truncated skill reads honestly.
const truncationMarker = "... (truncated)"

// Skill is one loaded skill document.
type Skill struct {
	// Name is the frontmatter name, or the file stem when frontmatter omits it.
	Name string

	// Description is the frontmatter description, capped at 256 bytes; empty when
	// frontmatter omits it.
	Description string

	// Body is everything after the frontmatter, capped at 16 KiB.
	Body string
}

// Library lists and loads skills out of one directory, freshly on every call.
type Library struct {
	logger *slog.Logger
	dir    string
}

// New builds a Library over one skills directory. A nil logger falls back to
// the slog default.
func New(dir string, logger *slog.Logger) *Library {
	if logger == nil {
		logger = slog.Default()
	}

	return &Library{dir: dir, logger: logger}
}

// List returns every loadable skill in lexical filename order, rescanning the
// directory on each call. A file that fails to load is skipped with a warn
// naming the path and the error, so one broken file never masks the others. On
// a duplicate effective name the lexically-first file wins and the shadowed
// file is skipped with a warn.
func (lib *Library) List() []Skill {
	entries, err := os.ReadDir(lib.dir)
	if err != nil {
		lib.logger.Warn("skills directory unreadable", "dir", lib.dir, "error", err)

		return nil
	}

	seen := make(map[string]struct{}, len(entries))

	var loaded []Skill

	for _, entry := range entries {
		skill, ok := lib.loadEntry(entry)
		if !ok {
			continue
		}

		if _, shadowed := seen[skill.Name]; shadowed {
			lib.logger.Warn("skill name shadowed by earlier file", "name", skill.Name, "file", entry.Name())

			continue
		}

		seen[skill.Name] = struct{}{}
		loaded = append(loaded, *skill)
	}

	return loaded
}

// Load returns the skill with the given name, rescanning the directory. The
// lexically-first file wins on a duplicate name. It returns ErrNotFound when no
// loadable file carries that name; unloadable files are skipped with a warn so
// one broken file cannot mask a valid match later in the directory.
func (lib *Library) Load(name string) (*Skill, error) {
	entries, err := os.ReadDir(lib.dir)
	if err != nil {
		return nil, fmt.Errorf("%w: skills directory unreadable: %w", ErrNotFound, err)
	}

	for _, entry := range entries {
		skill, ok := lib.loadEntry(entry)
		if !ok {
			continue
		}

		if skill.Name == name {
			return skill, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// loadEntry loads one directory entry, filtering out subdirectories and files
// that do not end in .md. A load failure warns and reports ok false so the scan
// continues past it.
func (lib *Library) loadEntry(entry os.DirEntry) (*Skill, bool) {
	if entry.IsDir() || !strings.HasSuffix(entry.Name(), skillFileSuffix) {
		return nil, false
	}

	path := filepath.Join(lib.dir, entry.Name())

	skill, err := loadFile(path)
	if err != nil {
		lib.logger.Warn("skipping unloadable skill file", "path", path, "error", err)

		return nil, false
	}

	return skill, true
}

// loadFile reads and parses one skill file.
func loadFile(path string) (*Skill, error) {
	// #nosec G304 -- the skills directory is the operator's own CairnOS home, same trust as workflow files
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}

	return parseSkill(path, data)
}

// skillDoc is one skill file parsed but not yet capped.
type skillDoc struct {
	name        string
	description string
	body        string
}

// parseSkill parses a skill file into a capped Skill. The effective name is the
// frontmatter name or, absent that, the file stem; either way it must match the
// skill-name charset or the file fails with ErrBadFrontmatter.
func parseSkill(path string, data []byte) (*Skill, error) {
	stem := strings.TrimSuffix(filepath.Base(path), skillFileSuffix)

	doc, err := parseFrontmatter(string(data), stem)
	if err != nil {
		return nil, err
	}

	if !validName(doc.name) {
		return nil, fmt.Errorf("%w: effective name %q is outside ^[a-z][a-z0-9_-]*$", ErrBadFrontmatter, doc.name)
	}

	return &Skill{
		Name:        doc.name,
		Description: truncate(doc.description, maxSkillDescriptionBytes),
		Body:        truncate(doc.body, maxSkillBodyBytes),
	}, nil
}

// parseFrontmatter reads an optional leading frontmatter block. No block at all
// is valid: the name falls back to the stem and the whole content is the body.
// A block recognizes exactly two keys, name and description; an unknown key, a
// line that is not key: value, or an unclosed block is ErrBadFrontmatter, a
// hard error rather than a guess.
func parseFrontmatter(content, stem string) (skillDoc, error) {
	doc := skillDoc{name: stem}

	lines := strings.Split(content, "\n")
	if len(lines) == 0 || trimCR(lines[0]) != frontmatterFence {
		doc.body = content

		return doc, nil
	}

	for idx := 1; idx < len(lines); idx++ {
		line := trimCR(lines[idx])
		if line == frontmatterFence {
			doc.body = strings.Join(lines[idx+1:], "\n")

			return doc, nil
		}

		key, value, found := strings.Cut(line, ":")
		if !found {
			return skillDoc{}, fmt.Errorf("%w: line %q is not key: value", ErrBadFrontmatter, line)
		}

		if err := applyFrontmatterKey(&doc, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return skillDoc{}, err
		}
	}

	return skillDoc{}, fmt.Errorf("%w: frontmatter block is never closed", ErrBadFrontmatter)
}

// applyFrontmatterKey records one recognized key onto the doc. An empty name
// value keeps the stem, matching the absent-name fallback; an unknown key is
// ErrBadFrontmatter.
func applyFrontmatterKey(doc *skillDoc, key, value string) error {
	switch key {
	case keyName:
		if value != "" {
			doc.name = value
		}
	case keyDescription:
		doc.description = value
	default:
		return fmt.Errorf("%w: unknown key %q", ErrBadFrontmatter, key)
	}

	return nil
}

// validName reports whether name matches ^[a-z][a-z0-9_-]*$, the charset that
// is also the chat slash-detection grammar, so a loadable skill is always
// invokable and nothing invokable is unloadable.
func validName(name string) bool {
	if name == "" || !isLowerAlpha(name[0]) {
		return false
	}

	for idx := 1; idx < len(name); idx++ {
		char := name[idx]
		if !isLowerAlpha(char) && !isDigit(char) && char != '_' && char != '-' {
			return false
		}
	}

	return true
}

func isLowerAlpha(char byte) bool { return char >= 'a' && char <= 'z' }

func isDigit(char byte) bool { return char >= '0' && char <= '9' }

// trimCR drops a trailing carriage return so CRLF files parse like LF files.
func trimCR(line string) string { return strings.TrimSuffix(line, "\r") }

// truncate clips text to limit bytes, appending the marker when it cut. Byte
// truncation can split a multi-byte rune, the same trade chat's truncateResult
// makes, which the caps make rare in practice.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	return text[:limit] + truncationMarker
}
