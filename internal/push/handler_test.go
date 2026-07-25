package push

import (
	"strings"
	"testing"
)

func TestValidRegistration(t *testing.T) {
	t.Parallel()
	valid := deviceRegistrationRequest{
		Provider:       "fcm",
		InstallationID: "installation-1",
		Token:          "token-1",
		Platform:       "android",
	}
	if !validRegistration(valid) {
		t.Fatal("valid Android FCM registration was rejected")
	}

	for name, mutate := range map[string]func(*deviceRegistrationRequest){
		"provider": func(req *deviceRegistrationRequest) { req.Provider = "unknown" },
		"platform": func(req *deviceRegistrationRequest) { req.Platform = "windows" },
		"installation": func(req *deviceRegistrationRequest) {
			req.InstallationID = strings.Repeat("x", 129)
		},
		"token": func(req *deviceRegistrationRequest) { req.Token = "" },
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := valid
			mutate(&req)
			if validRegistration(req) {
				t.Fatalf("invalid %s registration was accepted", name)
			}
		})
	}
}

func TestValidProviderIncludesReplaceableAndroidProviders(t *testing.T) {
	t.Parallel()
	if !validProvider("fcm") || !validProvider("oppo") {
		t.Fatal("configured Android providers should be accepted")
	}
	if validProvider("unknown") {
		t.Fatal("unknown provider should be rejected")
	}
}
