// Package telemetry recognises machine output in an APRS message body.
//
// Callsign validation cannot see this traffic: a tracker or gateway that sends
// its own status as an APRS message uses a perfectly valid callsign, so the
// message looks like a station talking. What gives it away is the body — an
// uptime string, a bracket-delimited health block, a series of bare numbers.
//
// The patterns are rows in the archive, edited by an operator, because they
// describe this network's traffic rather than a specification. This package
// compiles them once and answers a single question per message.
package telemetry

import (
	"fmt"
	"regexp"
	"strings"
)

// Rule is one compiled pattern.
type Rule struct {
	ID      int64
	Name    string
	Pattern string
	Kind    string
	re      *regexp.Regexp
}

// matches applies this rule to a body.
func (r Rule) matches(text string) bool {
	if r.Kind == encodedKind {
		return isEncodedBlob(text)
	}
	return r.re.MatchString(text)
}

// Source is what the collector needs from the store: the enabled patterns and a
// generation that changes when they do.
type Source struct {
	ID      int64
	Name    string
	Pattern string
	// Kind is "regex" or "encoded-blob". Empty means "regex", so a caller that
	// predates the column behaves as before.
	Kind       string
	Generation int64
}

// Matcher applies a compiled set of rules.
//
// It is immutable once built, and a new one replaces it wholesale, so a message
// never sees a half-updated rule set. The collector holds it behind a pointer
// swap.
type Matcher struct {
	rules      []Rule
	Generation int64
	// broken records patterns that failed to compile, so the admin page can
	// report them rather than silently not filtering.
	broken []string
}

// Compile builds a Matcher from stored patterns.
//
// A pattern that does not compile is skipped, not fatal: one bad row written by
// hand must not stop the collector. It is reported through Broken so the admin
// page can show it.
func Compile(items []Source, generation int64) *Matcher {
	m := &Matcher{Generation: generation}
	for _, it := range items {
		kind := it.Kind
		if kind == "" {
			kind = "regex"
		}
		switch kind {
		case "regex":
			re, err := regexp.Compile(it.Pattern)
			if err != nil {
				m.broken = append(m.broken, fmt.Sprintf("%s: %v", it.Name, err))
				continue
			}
			m.rules = append(m.rules, Rule{ID: it.ID, Name: it.Name, Pattern: it.Pattern, Kind: kind, re: re})
		case encodedKind:
			// A code-based detector: the pattern column is a label, not a
			// regular expression.
			m.rules = append(m.rules, Rule{ID: it.ID, Name: it.Name, Pattern: it.Pattern, Kind: kind})
		default:
			m.broken = append(m.broken, fmt.Sprintf("%s: unknown kind %q", it.Name, kind))
		}
	}
	return m
}

// Broken returns the patterns that could not be compiled.
func (m *Matcher) Broken() []string {
	if m == nil {
		return nil
	}
	return m.broken
}

// Count returns how many rules are active.
func (m *Matcher) Count() int {
	if m == nil {
		return 0
	}
	return len(m.rules)
}

// Match reports whether a body is machine output, and which rule said so.
//
// An empty matcher matches nothing: with no rules configured the archive keeps
// everything, which is the honest reading of "no filters". That is the opposite
// of the callsign validator, which fails closed — because a missing *filter* is
// a deliberate configuration, while a missing *callsign list* is a failure.
func (m *Matcher) Match(text string) (Rule, bool) {
	if m == nil || len(m.rules) == 0 {
		return Rule{}, false
	}
	// Patterns are anchored where they need to be, so trimming leading space
	// here does not change what they mean and makes them robust to padding.
	text = strings.TrimSpace(text)
	for _, r := range m.rules {
		if r.matches(text) {
			return r, true
		}
	}
	return Rule{}, false
}

// Validate compiles a single pattern, for the admin page's form check.
//
// It knows nothing about kinds: a rule of kind encoded-blob has a label in its
// pattern column, so its validation happens in ValidateKind.
func Validate(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("a pattern is required")
	}
	_, err := regexp.Compile(pattern)
	return err
}

// ValidateKind checks a rule as a whole: a regex must compile, and a detector
// kind must be one this build knows.
func ValidateKind(kind, pattern string) error {
	switch kind {
	case "", "regex":
		return Validate(pattern)
	case encodedKind:
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("a label is required")
		}
		return nil
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
}

// Kinds lists the rule kinds this build supports, for the admin form.
var Kinds = []struct {
	ID   string
	Desc string
}{
	{"regex", "A Go regular expression matched against the message body"},
	{encodedKind, "An opaque encoded run (base64-like), detected by shape and entropy"},
}
