package wsclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateArgsMirrorsTheAPI(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{CmdMachineInfo, ``, ""},
		{CmdMachineInfo, `{"sections":["network"]}`, ""},
		{CmdMachineInfo, `{"sections":["gpu"]}`, ErrInvalidArgs},
		{CmdAppsListUpgradable, `{"x":1}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"delay_seconds":3600,"when_user_active":"skip"}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3601}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"when_user_active":"force"}`, ErrInvalidArgs},
		{"machine.format_disk", `{}`, ErrUnsupportedCommand},
	}
	for _, c := range cases {
		got := ValidateArgs(c.name, json.RawMessage(c.args))
		if (c.want == "") != (got == nil) || (got != nil && got.Code != c.want) {
			t.Errorf("ValidateArgs(%s, %s) = %+v, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestRebootArgsDefaults(t *testing.T) {
	var a RebootArgs
	if a.Delay() != 300 || a.Mode() != WhenUserActiveWarn {
		t.Fatalf("defaults = %d %s", a.Delay(), a.Mode())
	}
}

func TestDestructive(t *testing.T) {
	if !Destructive(CmdServiceRestart) || !Destructive(CmdMachineReboot) || Destructive(CmdMachineInfo) {
		t.Fatal("destructive set wrong")
	}
}

func TestManifestCheckOmitsUnknowns(t *testing.T) {
	b, _ := json.Marshal(ManifestCheck{Target: "emly", Decision: "up_to_date", CheckedAt: "t"})
	for _, key := range []string{"installed_version", "available_version", "pending", "source", "error", "channel"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("%s present in %s", key, b)
		}
	}
}
