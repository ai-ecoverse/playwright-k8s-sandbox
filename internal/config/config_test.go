package config

import "testing"

func TestSubstrateHTTPHosts(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "test")
	t.Setenv("BACKEND", "substrate")
	t.Setenv("SUBSTRATE_ACTOR_TEMPLATE", "test/browser")
	t.Setenv("SUBSTRATE_ALLOWED_HTTP_HOSTS", "fixture.test, second.test")
	c, err := FromEnv()
	if err != nil || len(c.SubstrateAllowedHTTPHosts) != 2 || c.SubstrateAllowedHTTPHosts[1] != "second.test" {
		t.Fatalf("config=%v error=%v", c, err)
	}
	for _, invalid := range []string{"http://fixture.test", "fixture.test:80", "fixture.test,", "bad host"} {
		t.Setenv("SUBSTRATE_ALLOWED_HTTP_HOSTS", invalid)
		if _, err := FromEnv(); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestParsePort(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		allowZero bool
		want      int
		wantErr   bool
	}{
		{name: "valid port", raw: "9222", want: 9222},
		{name: "min", raw: "1", want: 1},
		{name: "max", raw: "65535", want: 65535},
		{name: "zero rejected without allowZero", raw: "0", wantErr: true},
		{name: "zero allowed as sentinel", raw: "0", allowZero: true, want: 0},
		{name: "negative rejected", raw: "-1", wantErr: true},
		{name: "above range rejected", raw: "65536", wantErr: true},
		{name: "non-numeric rejected", raw: "abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePort("PORT", tt.raw, tt.allowZero)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePort(%q) = %d, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePort(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("parsePort(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}
