package policy

import "testing"

func TestPermission_String(t *testing.T) {
	cases := []struct {
		p    Permission
		want string
	}{
		{Pass, "pass"},
		{Ask, "ask"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.p, got, c.want)
		}
	}
}
