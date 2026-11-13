package sanitize_test

import (
	"strings"
	"testing"

	"aprs-archive/internal/sanitize"
)

func TestTextRemovesControlCharacters(t *testing.T) {
	in := "hello\x1b[31mred\x00world\x7f"
	if got, want := sanitize.SingleLine(in), "hello[31mredworld"; got != want {
		t.Fatalf("SingleLine(%q) = %q, want %q", in, got, want)
	}
}

func TestTextRemovesBidiOverrides(t *testing.T) {
	in := "abc\u202edef\u2066ghi\ufeff"
	if got, want := sanitize.SingleLine(in), "abcdefghi"; got != want {
		t.Fatalf("SingleLine(%q) = %q, want %q", in, got, want)
	}
}

func TestTextCollapsesWhitespaceOnOneLine(t *testing.T) {
	in := "  a \t b\r\nc  "
	if got, want := sanitize.SingleLine(in), "a b c"; got != want {
		t.Fatalf("SingleLine(%q) = %q, want %q", in, got, want)
	}
}

func TestTextKeepsLineBreaksInMultiLine(t *testing.T) {
	in := "line one\nline two\r\nline three"
	if got, want := sanitize.MultiLine(in), "line one\nline two\nline three"; got != want {
		t.Fatalf("MultiLine(%q) = %q, want %q", in, got, want)
	}
}

func TestTextCapsBlankLineRuns(t *testing.T) {
	in := "a\n\n\n\n\nb"
	if got, want := sanitize.MultiLine(in), "a\n\nb"; got != want {
		t.Fatalf("MultiLine(%q) = %q, want %q", in, got, want)
	}
}

func TestTextIsIdempotent(t *testing.T) {
	in := "x\x00\u202e y\n\n\nz\t"
	once := sanitize.MultiLine(in)
	if twice := sanitize.MultiLine(once); twice != once {
		t.Fatalf("not idempotent: %q -> %q", once, twice)
	}
}

func TestTextHandlesInvalidUTF8(t *testing.T) {
	got := sanitize.SingleLine("bad\xffbytes")
	if !strings.Contains(got, "bad") || !strings.Contains(got, "bytes") {
		t.Fatalf("SingleLine dropped content: %q", got)
	}
}
