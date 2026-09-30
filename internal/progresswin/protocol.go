// Package progresswin draws the "please wait" window shown while an update of
// EMLy or of AryxD Agent itself is downloaded and installed: a fixed-size,
// non-closable dialog with a heading, a detail line and a progress bar, whose
// title-bar icon is taken from an executable (EMLy.exe for EMLy's updates).
//
// Like internal/toast it runs in the session of the user at the machine (console or RDP), in a helper
// process the SYSTEM service starts there with the "show-progress"
// subcommand (internal/notify.OpenProgressWindow). The service drives it
// through the helper's stdin, one JSON Message per line, and the window never
// outlives the pipe: whatever happens to the service, the window goes away.
package progresswin

import (
	"bufio"
	"encoding/json"
	"io"
)

// Indeterminate is the Percent of a phase whose length is unknown - a setup
// run, a download without Content-Length. The bar then shows a marquee.
const Indeterminate = -1

// Message is one line of the service -> window protocol. Each one replaces the
// whole visible state, so a lost or coalesced line costs nothing.
type Message struct {
	Heading string `json:"heading,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Percent is 0-100, or Indeterminate.
	Percent int `json:"percent"`
	// Close ends the window at once. Without it, the end of stdin means the
	// service went away while still busy - see Options.WaitService.
	Close bool `json:"close,omitempty"`
}

// Encode returns m as one protocol line.
func Encode(m Message) []byte {
	b, _ := json.Marshal(m) // a struct of strings and ints cannot fail
	return append(b, '\n')
}

// readMessages decodes r line by line into apply until r ends or a Close
// message arrives, and reports which of the two ended it. Malformed lines are
// skipped: the next one restates the whole state anyway. A read error counts
// as the end of r - a broken pipe is how the service going away looks.
func readMessages(r io.Reader, apply func(Message)) (closed bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Close {
			return true
		}
		apply(m)
	}
	return false
}
