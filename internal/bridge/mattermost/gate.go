package mattermost

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// propTrue is the value Mattermost writes for a boolean prop; props are a
// string map on the wire, so "true" is a string, not a bool.
const propTrue = "true"

// PropActivateAI is the opt-in an automated poster sets to address the bot
// deliberately. It admits a post the provenance check would otherwise drop.
const PropActivateAI = "activate_ai"

// The props the server stamps on a post no human typed. from_bot covers bot
// accounts, and the server writes it on every post by a user with is_bot set;
// the other three cover integrations that post with no bot user behind them,
// which is the only case a user lookup has to cover.
const (
	propFromBot      = "from_bot"
	propFromWebhook  = "from_webhook"
	propFromPlugin   = "from_plugin"
	propFromOAuthApp = "from_oauth_app"
)

// Loop-limiter defaults (FR15).
const (
	defaultLoopMaxEvents = 20
	defaultLoopWindow    = 5 * time.Minute
	defaultLoopCooldown  = 10 * time.Minute
)

// The three code forms Mattermost markdown renders: a backtick fence, a tilde
// fence, and a four-space or tab indent. backtick alone opens an inline span.
const (
	backtick    = '`'
	fenceMarker = "```"
	tildeMarker = "~~~"
	indentSpace = "    "
	indentTab   = "\t"
)

// MentionsInText reports whether message addresses username outside of code.
// The server's own mention list says the bot was mentioned but not where, so
// this is the check that keeps a username quoted in a code sample from waking
// the bot (FR10).
func MentionsInText(message, username string) bool {
	if username == "" {
		return false
	}

	return indexMention(blankCode(message), username) >= 0
}

// StripMention removes a leading @username and one following space, so the
// agent reads the request the way the user wrote it rather than a line that
// opens with its own name.
func StripMention(message, username string) string {
	if username == "" {
		return message
	}

	rest := strings.TrimLeft(message, " \t")

	mention := "@" + username
	if len(rest) < len(mention) || !strings.EqualFold(rest[:len(mention)], mention) {
		return message
	}

	rest = rest[len(mention):]

	// "@botfoo" and "@bot-team" both name a different user, so only a boundary
	// after the username makes this the mention to strip.
	if continues(rest) {
		return message
	}

	return strings.TrimPrefix(rest, " ")
}

// IsAutomated reports whether a post came from something other than a person
// typing. user may be nil when the caller has not looked the author up; the
// props alone answer the bot-account case, because the server stamps from_bot
// on every post a bot user makes.
func IsAutomated(post *Post, user *User) bool {
	if user != nil && user.IsBot {
		return true
	}

	if post == nil {
		return false
	}

	for _, prop := range [...]string{propFromBot, propFromWebhook, propFromPlugin, propFromOAuthApp} {
		if post.Props[prop] == propTrue {
			return true
		}
	}

	return false
}

// ActivateAI reports the prop an automated poster sets to address the bot on
// purpose, which admits it outside the channels allow_bots_in names (FR14).
func ActivateAI(post *Post) bool {
	return post != nil && post.Props[PropActivateAI] == propTrue
}

// blankCode overwrites every code form with spaces: indented blocks first, then
// both fences and inline spans. It replaces rather than deletes so every byte
// offset in the result still points at the same place in the original message.
func blankCode(message string) string {
	buf := []byte(message)

	blankIndented(buf)

	var cursor int

	for cursor < len(buf) {
		cursor = blankAt(buf, cursor)
	}

	return string(buf)
}

// blankAt blanks whatever code form opens at cursor and returns the index past
// it, or the next byte when nothing opens there.
func blankAt(buf []byte, cursor int) int {
	switch {
	case bytes.HasPrefix(buf[cursor:], []byte(fenceMarker)):
		return blankRegion(buf, cursor, []byte(fenceMarker))
	case bytes.HasPrefix(buf[cursor:], []byte(tildeMarker)):
		return blankRegion(buf, cursor, []byte(tildeMarker))
	case buf[cursor] == backtick && closesOnLine(buf, cursor):
		return blankRegion(buf, cursor, []byte{backtick})
	default:
		return cursor + 1
	}
}

// closesOnLine reports whether the inline span opening at start closes before
// the line ends. Mattermost renders an unmatched backtick as a literal, so
// blanking to end-of-message there erases a real mention and drops the post.
// An unterminated fence is the opposite case and does run to the end.
func closesOnLine(buf []byte, start int) bool {
	line := buf[start+1:]
	if end := bytes.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}

	return bytes.IndexByte(line, backtick) >= 0
}

// blankIndented blanks the four-space and tab indented code lines, which
// Mattermost renders as code just as a fence does (FR10). A line opens a block
// only when the line before it is blank or is itself indented code, which is
// what keeps a wrapped list item from reading as code.
func blankIndented(buf []byte) {
	var start int

	opens := true

	for start < len(buf) {
		stop := lineEnd(buf, start)
		line := buf[start:stop]
		empty := len(bytes.TrimSpace(line)) == 0
		code := opens && !empty && indented(line)

		if code {
			blankBytes(line)
		}

		opens = code || empty
		start = stop + 1
	}
}

// lineEnd returns the index of the newline that ends the line at start, or the
// end of buf for the last line.
func lineEnd(buf []byte, start int) int {
	if at := bytes.IndexByte(buf[start:], '\n'); at >= 0 {
		return start + at
	}

	return len(buf)
}

// indented reports whether line carries the indent that opens a code block.
func indented(line []byte) bool {
	return bytes.HasPrefix(line, []byte(indentSpace)) || bytes.HasPrefix(line, []byte(indentTab))
}

// blankRegion spaces out the code region opening at start and returns the index
// past it. An unterminated region runs to the end of the message, which is what
// a reader sees too.
func blankRegion(buf []byte, start int, marker []byte) int {
	stop := len(buf)

	body := start + len(marker)
	if closing := bytes.Index(buf[body:], marker); closing >= 0 {
		stop = body + closing + len(marker)
	}

	blankBytes(buf[start:stop])

	return stop
}

// blankBytes overwrites region with spaces, keeping every later byte offset
// where it was.
func blankBytes(region []byte) {
	for i := range region {
		region[i] = ' '
	}
}

// indexMention finds "@username" at a word boundary and returns its offset, or
// -1. Mattermost usernames are lowercase ASCII, so folding bytes is enough and
// saves compiling a pattern per post.
func indexMention(text, username string) int {
	needle := "@" + strings.ToLower(username)
	folded := strings.ToLower(text)

	var start int

	for start+len(needle) <= len(folded) {
		offset := strings.Index(folded[start:], needle)
		if offset < 0 {
			return -1
		}

		offset += start
		if bounded(folded, offset, len(needle)) {
			return offset
		}

		start = offset + 1
	}

	return -1
}

// bounded reports whether the match at offset stands alone: an address such as
// "mail@cairn" is not a mention, and "@cairnbot" names someone else.
func bounded(text string, at, width int) bool {
	if at > 0 && isWordByte(text[at-1]) {
		return false
	}

	return !continues(text[at+width:])
}

// continues reports whether rest extends the username a mention just matched.
// Mattermost drops a trailing "." or "-" run when it resolves a mention, so
// "Hey @cairn." still addresses cairn while "@cairn-bot" names someone else.
func continues(rest string) bool {
	var offset int

	for offset < len(rest) && (rest[offset] == '.' || rest[offset] == '-') {
		offset++
	}

	return offset < len(rest) && isWordByte(rest[offset])
}

// isWordByte reports whether b continues a username. Mattermost usernames match
// ^[a-z0-9.\-_]+$, so "." and "-" belong: without them "@ops" matches inside
// "@ops-team", which names a different user (FR10).
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	default:
		return b == '_' || b == '.' || b == '-'
	}
}

// LoopLimitConfig tunes the loop guard; zero fields take the FR15 defaults.
type LoopLimitConfig struct {
	// Window is how far back an admission counts.
	Window time.Duration

	// Cooldown is how long a tripped conversation stays silent.
	Cooldown time.Duration

	// MaxEvents is how many automated posts one conversation may spend inside
	// Window before it trips.
	MaxEvents int
}

// LoopLimiter caps how many automated posts one conversation may turn into chat
// turns. Provenance keeps most loops from starting, but the channels
// allow_bots_in names are exactly the ones where two bots can answer each other
// forever, so the window is the second half of that guard (design D6).
// Safe for concurrent use.
type LoopLimiter struct {
	windows map[string]*loopWindow

	mu sync.Mutex

	window   time.Duration
	cooldown time.Duration
	max      int
}

// loopWindow is one conversation's recent admissions plus the cooldown it is
// serving, if any.
type loopWindow struct {
	until  time.Time
	admits []time.Time
}

// NewLoopLimiter builds a limiter, applying the FR15 defaults to unset fields.
func NewLoopLimiter(cfg LoopLimitConfig) *LoopLimiter {
	maxEvents := cfg.MaxEvents
	if maxEvents <= 0 {
		maxEvents = defaultLoopMaxEvents
	}

	window := cfg.Window
	if window <= 0 {
		window = defaultLoopWindow
	}

	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultLoopCooldown
	}

	return &LoopLimiter{
		windows:  make(map[string]*loopWindow),
		window:   window,
		cooldown: cooldown,
		max:      maxEvents,
	}
}

// Admit records one automated post on key and reports whether the bridge should
// answer it. The post that would exceed the window trips the cooldown, so the
// conversation goes quiet at max admissions rather than after them.
func (l *LoopLimiter) Admit(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(now)

	entry, ok := l.windows[key]
	if !ok {
		entry = &loopWindow{}
		l.windows[key] = entry
	}

	if now.Before(entry.until) {
		return false
	}

	entry.admits = keepSince(entry.admits, now.Add(-l.window))

	if len(entry.admits) >= l.max {
		entry.until = now.Add(l.cooldown)
		entry.admits = nil

		return false
	}

	entry.admits = append(entry.admits, now)

	return true
}

// keepSince drops the admissions that fell out of the window. The slice is
// ordered by arrival, so the first one still inside marks the cut.
func keepSince(admits []time.Time, cutoff time.Time) []time.Time {
	for i, at := range admits {
		if at.After(cutoff) {
			return admits[i:]
		}
	}

	return nil
}

// pruneLocked drops conversations with nothing left to remember, so the map
// stays bounded by the conversations currently talking. The caller holds mu.
func (l *LoopLimiter) pruneLocked(now time.Time) {
	for key, entry := range l.windows {
		if len(entry.admits) == 0 && now.After(entry.until) {
			delete(l.windows, key)
		}
	}
}
