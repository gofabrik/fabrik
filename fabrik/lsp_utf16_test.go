package main

import "testing"

func TestUTF16Columns(t *testing.T) {
	// "héllo 🙂 //fabrik:http" - é is 2 bytes/1 unit, 🙂 is 4 bytes/2 units.
	line := "héllo 🙂 //fabrik:http"
	byteOff := len("héllo 🙂 ")
	utf16Off := utf16Col(line, byteOff)
	if want := len([]rune("héllo ")) + 2 + 1; utf16Off != want {
		t.Errorf("utf16Col = %d, want %d", utf16Off, want)
	}
	if got := byteCol(line, utf16Off); got != byteOff {
		t.Errorf("byteCol(utf16Col(x)) = %d, want %d", got, byteOff)
	}
	if got := byteCol("abc", 99); got != 3 {
		t.Errorf("byteCol past end = %d, want 3", got)
	}
}
