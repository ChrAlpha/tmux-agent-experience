package tmux

import "testing"

// A shot's text is what the pane shows without its escapes; its digest tells
// apart screens that differ only in attributes or in where the cursor is.
func TestAShotTellsScreensApartByAttributesAndCursor(t *testing.T) {
	plain := shot("Allow? \x1b[7mYes\x1b[0m  No\n\x1b]8;;http://x\x07link\x1b]8;;\x07\n3,0\n")
	if plain.Text != "Allow? Yes  No\nlink\n" {
		t.Fatalf("text = %q", plain.Text)
	}
	moved := shot("Allow? Yes  \x1b[7mNo\x1b[0m\n\x1b]8;;http://x\x07link\x1b]8;;\x07\n3,0\n")
	cursor := shot("Allow? \x1b[7mYes\x1b[0m  No\n\x1b]8;;http://x\x07link\x1b]8;;\x07\n4,0\n")
	again := shot("Allow? \x1b[7mYes\x1b[0m  No\n\x1b]8;;http://x\x07link\x1b]8;;\x07\n3,0\n")
	switch {
	case plain.Cursor != "3,0" || plain.Ansi == moved.Ansi:
		t.Fatalf("what a reader is shown of a selection and the cursor: %q %q", plain.Ansi, plain.Cursor)
	case moved.Text != plain.Text:
		t.Fatalf("moved text = %q", moved.Text)
	case moved.Digest == plain.Digest:
		t.Fatal("a moved selection reads as the same screen")
	case cursor.Digest == plain.Digest:
		t.Fatal("a moved cursor reads as the same screen")
	case again.Digest != plain.Digest || len(plain.Digest) != 64:
		t.Fatalf("the same screen twice: %s %s", plain.Digest, again.Digest)
	}
}
