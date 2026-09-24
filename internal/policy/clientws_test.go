package policy

import (
	"testing"
)

func TestClientWSCommandsDefaultWhenAbsent(t *testing.T) {
	p, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"generatedAt":"2026-09-23T00:00:00Z","servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	c := p.Global.ClientWS
	if !c.Enabled || !c.Allows("machine.info") || c.Allows("machine.reboot") {
		t.Fatalf("ClientWS = %+v", c)
	}
}

func TestClientWSCommandsReplaceTheDefault(t *testing.T) {
	p, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"generatedAt":"2026-09-23T00:00:00Z","servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true,"commands":["machine.reboot"]}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	c := p.Global.ClientWS
	if !c.Allows("machine.reboot") || c.Allows("machine.info") {
		t.Fatalf("ClientWS = %+v (arrays replace, they do not merge)", c)
	}
}

func TestLegacyPolicyAllowsOnlyReadOnlyCommands(t *testing.T) {
	d := fixtureDefaults(t)
	if d.ClientWS.Enabled || d.ClientWS.Allows("service.restart") || !d.ClientWS.Allows("apps.list_upgradable") {
		t.Fatalf("defaults ClientWS = %+v", d.ClientWS)
	}
}

// An explicit empty list means "no commands allowed" - distinct from the
// section being absent entirely, which keeps DefaultClientWSCommands. Merge
// patch replaces arrays whole rather than merging them, so this falls out of
// complete()/mergePatch() naturally; this test is the regression guard for
// that behaviour, since "commands: []" and "commands absent" produce the
// same zero-length-looking JSON value if handled carelessly.
func TestClientWSCommandsEmptyMeansNoneAllowed(t *testing.T) {
	p, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"generatedAt":"2026-09-23T00:00:00Z","servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true,"commands":[]}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if p.Global.ClientWS.Allows("machine.info") {
		t.Fatalf("ClientWS = %+v, want commands:[] to allow nothing", p.Global.ClientWS)
	}

	absent, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"generatedAt":"2026-09-23T00:00:00Z","servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if !absent.Global.ClientWS.Allows("machine.info") {
		t.Fatalf("ClientWS = %+v, want an absent commands key to keep the default allowlist", absent.Global.ClientWS)
	}
}
