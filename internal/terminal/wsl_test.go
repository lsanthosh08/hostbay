package terminal

import (
	"encoding/binary"
	"slices"
	"testing"
	"unicode/utf16"
)

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func TestParseWSLListDecodesUTF16(t *testing.T) {
	out := utf16le("\ufeffUbuntu-22.04\r\nDebian\r\n\r\nkali-linux\r\n")
	got := parseWSLList(out)
	if want := []string{"Ubuntu-22.04", "Debian", "kali-linux"}; !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}

func TestParseWSLListEmptyWhenNothingInstalled(t *testing.T) {
	if got := parseWSLList(nil); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	if got := parseWSLList(utf16le("\r\n")); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
