package audit

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: " XHIGH ", want: "xhigh"},
		{value: "fixed", want: "fixed"},
		// max/minimal are real grok-build wire tiers; adapters only forward them when the model menu lists them.
		{value: "max", want: "max"},
		{value: "MINIMAL", want: "minimal"},
		{value: "ultra", want: ""},
		{value: "thinking:32000", want: ""},
		{value: "customer@example.com", want: ""},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			if got := NormalizeReasoningEffort(test.value); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
