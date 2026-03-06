package pool

import "testing"

func TestParseCloudflareTraceIP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			name: "ipv4",
			body: "fl=29f109\nh=www.cloudflare.com\nip=203.0.113.10\nts=1700000000.000\n",
			want: "203.0.113.10",
			ok:   true,
		},
		{
			name: "ipv6",
			body: "ip=2001:db8::1\n",
			want: "2001:db8::1",
			ok:   true,
		},
		{
			name: "missing",
			body: "h=www.cloudflare.com\n",
			want: "",
			ok:   false,
		},
		{
			name: "invalid",
			body: "ip=not-an-ip\n",
			want: "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseCloudflareTraceIP(tc.body)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v (got=%q)", ok, tc.ok, got)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

