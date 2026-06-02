package auth

import "testing"

func TestSessionLocationResolverFallbacks(t *testing.T) {
	resolver := &SessionLocationResolver{}
	privateIP := "192.168.1.12"
	publicIP := "8.8.8.8"
	localhost := "localhost"
	loopback := "127.0.0.1"
	empty := ""

	tests := []struct {
		name string
		ip   *string
		want string
	}{
		{name: "nil", ip: nil, want: unknownSessionLocation},
		{name: "empty", ip: &empty, want: unknownSessionLocation},
		{name: "localhost", ip: &localhost, want: localSessionLocation},
		{name: "loopback", ip: &loopback, want: localSessionLocation},
		{name: "private", ip: &privateIP, want: privateSessionLocation},
		{name: "public without database", ip: &publicIP, want: unknownSessionLocation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolver.Location(tt.ip); got != tt.want {
				t.Fatalf("Location() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLocalizedGeoNamePrefersChinese(t *testing.T) {
	got := localizedGeoName(
		map[string]string{"en": "United States", "zh-CN": "\u7f8e\u56fd"},
		"US",
	)
	if got != "\u7f8e\u56fd" {
		t.Fatalf("localizedGeoName() = %q, want %q", got, "\u7f8e\u56fd")
	}
}

func TestJoinLocationPartsDeduplicates(t *testing.T) {
	got := joinLocationParts("\u4e2d\u56fd", "\u5e7f\u4e1c", "\u5e7f\u4e1c", "\u6df1\u5733")
	want := "\u4e2d\u56fd, \u5e7f\u4e1c, \u6df1\u5733"
	if got != want {
		t.Fatalf("joinLocationParts() = %q, want %q", got, want)
	}
}
