package progresswin

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadMessagesAppliesInOrderUntilClose(t *testing.T) {
	var in bytes.Buffer
	in.Write(Encode(Message{Heading: "a", Percent: 10}))
	in.WriteString("not json\n")
	in.Write(Encode(Message{Heading: "b", Percent: Indeterminate}))
	in.Write(Encode(Message{Close: true}))
	in.Write(Encode(Message{Heading: "after close"}))

	var got []Message
	closed := readMessages(&in, func(m Message) { got = append(got, m) })
	if !closed {
		t.Error("closed = false, want true: a Close message ends the stream")
	}
	if len(got) != 2 || got[0].Heading != "a" || got[1].Heading != "b" || got[1].Percent != Indeterminate {
		t.Errorf("applied %+v, want a(10) then b(indeterminate), malformed line skipped, nothing after close", got)
	}
}

// EOF without Close is the service going away mid-work, which the window has
// to tell apart from an orderly end.
func TestReadMessagesEOFIsNotClose(t *testing.T) {
	closed := readMessages(strings.NewReader(string(Encode(Message{Heading: "x"}))), func(Message) {})
	if closed {
		t.Error("closed = true on plain EOF")
	}
}

func TestEncodeIsOneLine(t *testing.T) {
	b := Encode(Message{Heading: "riga\ncon a capo", Percent: 5})
	if bytes.Count(b, []byte("\n")) != 1 || b[len(b)-1] != '\n' {
		t.Errorf("Encode = %q, want exactly one trailing newline", b)
	}
}
