package main

import (
	"bytes"
	stdlog "log"
	"strings"
	"testing"
)

// The webm parser logs a line through the standard logger for every
// discardable block. On some files that is tens of thousands of lines, so it
// has to be muted.
func TestLibraryLoggingIsSilenced(t *testing.T) {
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	stdlog.Println("Discardable packet")
	if !strings.Contains(buf.String(), "Discardable packet") {
		t.Fatal("standard logger is not the sink the parser writes to")
	}

	silenceLibraryLogging()
	buf.Reset()
	for i := 0; i < 1000; i++ {
		stdlog.Println("Discardable packet")
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes still written after silencing", buf.Len())
	}
}
