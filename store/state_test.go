package store

// The settings record itself: what a name may hold, and how it
// survives a round trip through flash.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeNameKeepsARealReplacementCharacter: ranging a string
// decodes an invalid byte as U+FFFD, which is indistinguishable from a
// U+FFFD the name really contains — so a name carrying both lost both,
// while the same name without the stray byte kept its own. The same
// visible name then saved as two different strings, and a flash write
// happened for a name nobody had changed.
func TestSanitizeNameKeepsARealReplacementCharacter(t *testing.T) {
	const real = "Bob�"
	if got := SanitizeName(real); got != real {
		t.Errorf("a name that is already text came back as %q", got)
	}
	// The same name with a stray byte in it keeps everything that is
	// text, and loses only the byte that is not.
	if got := SanitizeName("Bob\xff�"); got != real {
		t.Errorf("SanitizeName(%q) = %q, want %q", "Bob\xff�", got, real)
	}
	// Which is the property that matters: the same visible name saves
	// the same way whether or not something else in it was broken.
	if SanitizeName("Bob\xff�") != SanitizeName(real) {
		t.Error("the same name saved two different ways")
	}
}

// TestSanitizeNameIsAlwaysStorable: whatever comes off the air, what
// this returns has to be storable — one malformed frame that got through
// would stop every save on the device for good.
func TestSanitizeNameIsAlwaysStorable(t *testing.T) {
	for _, in := range []string{
		"", "Lukasz", "Bob\xff", "\xff\xfe\xfd", "��",
		strings.Repeat("n", 200), strings.Repeat("é", 100),
		"\xf0\x9f\x92\xa9", "a\xed\xa0\x80b", strings.Repeat("\xff", 64),
	} {
		got := SanitizeName(in)
		if err := checkName(got); err != nil {
			t.Errorf("SanitizeName(%q) = %q, which cannot be stored: %v", in, got, err)
		}
		if !utf8.ValidString(got) {
			t.Errorf("SanitizeName(%q) = %q, which is not text", in, got)
		}
		// And it settles: sanitizing what came back changes nothing.
		if again := SanitizeName(got); again != got {
			t.Errorf("SanitizeName(%q) = %q, and again = %q", in, got, again)
		}
	}
}
