package notify

import (
	"fmt"
	"strings"
	"testing"
)

func TestProductMessagesNameTheProduct(t *testing.T) {
	for _, m := range []Message{
		CriticalUpdateProductMessage("3g-RocketChat"),
		ProductWaitingMessage("3g-RocketChat"),
		ProductUpdatedMessage("3g-RocketChat", "1.1.0"),
	} {
		if !strings.Contains(m.Title, "3g-RocketChat") || !strings.Contains(m.Body, "3g-RocketChat") {
			t.Errorf("message does not name the product: %+v", m)
		}
		if strings.Contains(m.Title+m.Body, "EMLy") {
			t.Errorf("product message mentions EMLy: %+v", m)
		}
	}
	if !strings.Contains(ProductUpdatedMessage("X", "1.1.0").Body, "1.1.0") {
		t.Error("updated message must carry the version")
	}
}

func TestCriticalUpdateProductMessageTakesTheCountdown(t *testing.T) {
	body := fmt.Sprintf(CriticalUpdateProductMessage("X").Body, 30)
	if !strings.Contains(body, "30") || strings.Contains(body, "%!") {
		t.Errorf("body = %q", body)
	}
}

// formatBody only formats bodies that have a verb: fmt.Sprintf on a body
// without one appends "%!(EXTRA int=60)" to the text the user reads.
func TestFormatBodyWithoutVerbIsUnchanged(t *testing.T) {
	if got := formatBody("Chiudere l'applicazione.", 60); got != "Chiudere l'applicazione." {
		t.Errorf("formatBody = %q", got)
	}
	if got := formatBody("Chiude tra %d secondi.", 60); got != "Chiude tra 60 secondi." {
		t.Errorf("formatBody = %q", got)
	}
}

// The body is a format string: a '%' in the product's name must reach the
// user as a '%', not as a formatting directive.
func TestCriticalUpdateProductMessageEscapesPercentInName(t *testing.T) {
	msg := CriticalUpdateProductMessage("100% Chat")
	body := formatBody(msg.Body, 30)
	if !strings.HasPrefix(body, "100% Chat verrà chiuso tra 30 secondi") || strings.Contains(body, "%!") {
		t.Errorf("body = %q", body)
	}
	if msg.Title != "100% Chat - Aggiornamento critico" {
		t.Errorf("title = %q", msg.Title)
	}
}
