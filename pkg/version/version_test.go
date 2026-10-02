package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name            string
		version, commit string
		want            string
	}{
		{"defaults", "dev", "unknown", "dev (unknown)"},
		{"release", "v1.2.3", "abc1234", "v1.2.3 (abc1234)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldV, oldC := Version, Commit
			t.Cleanup(func() { Version, Commit = oldV, oldC })
			Version, Commit = tc.version, tc.commit

			if got := String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
