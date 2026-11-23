package product

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
)

const gwSecret = "gateway-shared-secret-0123456789"

func trustedRig(t *testing.T, tm *teamstore.Mem, admins ...string) *rig {
	t.Helper()
	return newRig(t, rigOpts{team: true, rawAuth: true, prod: Options{Team: tm, Admins: admins,
		TrustIdentityHeaders: &TrustedHeaders{Secret: []byte(gwSecret)}}})
}

func gwGet(t *testing.T, r *rig, path string, hdr ...string) *http.Response {
	t.Helper()
	return r.do("GET", path, "", "", hdr...)
}

func TestTrustedHeadersSignInTheUserTheGatewayVouchesFor(t *testing.T) {
	tm := teamstore.NewMem()
	r := trustedRig(t, tm, "root@corp.example")
	resp := gwGet(t, r, "/api/me", "X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "Ann.Lee", "X-Forwarded-Email", "ann@corp.example", "X-Forwarded-Name", "Ann Lee")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	u, err := tm.GetUser(context.Background(), "ann.lee")
	if err != nil || u.Email != "ann@corp.example" || u.Name != "Ann Lee" || u.Status != "approved" {
		t.Fatalf("user = %+v, %v", u, err)
	}
	// An admin named by email in HARNESS_ADMINS is an admin through the gateway too.
	resp = gwGet(t, r, "/api/me", "X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "root", "X-Forwarded-Email", "root@corp.example")
	if resp.StatusCode != 200 {
		t.Fatalf("admin status = %d", resp.StatusCode)
	}
	if resp := gwGet(t, r, "/api/admin/users", "X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "root", "X-Forwarded-Email", "root@corp.example"); resp.StatusCode != 200 {
		t.Fatalf("admin users = %d", resp.StatusCode)
	}
	if resp := gwGet(t, r, "/api/admin/users", "X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "ann.lee"); resp.StatusCode != 403 {
		t.Fatalf("a plain user reached the admin panel: %d", resp.StatusCode)
	}
}

func TestTrustedHeadersRefuseAMissingOrWrongSecret(t *testing.T) {
	r := trustedRig(t, teamstore.NewMem())
	for name, hdr := range map[string][]string{
		"no secret":    {"X-Forwarded-User", "mallory"},
		"wrong secret": {"X-Harness-Gateway-Secret", "not-the-secret-not-the-secret", "X-Forwarded-User", "mallory"},
		"empty login":  {"X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "  "},
		"spaced login": {"X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "a b"},
		"secret only":  {"X-Harness-Gateway-Secret", gwSecret},
		"nothing":      nil,
	} {
		if resp := gwGet(t, r, "/api/me", hdr...); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
}

func TestTrustedHeaderNamesAreConfigurable(t *testing.T) {
	cfg := TrustedHeaders{Secret: []byte(gwSecret), SecretHeader: "X-Gw-Proof", LoginHeader: "X-Auth-User", EmailHeader: "X-Auth-Mail", NameHeader: "X-Auth-Display"}
	r := newRig(t, rigOpts{team: true, rawAuth: true, prod: Options{TrustIdentityHeaders: &cfg}})
	if resp := gwGet(t, r, "/api/me", "X-Gw-Proof", gwSecret, "X-Auth-User", "zed"); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp := gwGet(t, r, "/api/me", "X-Harness-Gateway-Secret", gwSecret, "X-Forwarded-User", "zed"); resp.StatusCode != 401 {
		t.Fatalf("the default header names must not work once renamed: %d", resp.StatusCode)
	}
}

func TestTrustedHeadersFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if th, err := TrustedHeadersFromEnv(env(nil)); th != nil || err != nil {
		t.Fatalf("off: %v %v", th, err)
	}
	if _, err := TrustedHeadersFromEnv(env(map[string]string{"HARNESS_TRUST_IDENTITY_HEADERS": "1"})); err == nil || !strings.Contains(err.Error(), "HARNESS_IDENTITY_HEADER_SECRET") {
		t.Fatalf("a missing secret must be refused: %v", err)
	}
	if _, err := TrustedHeadersFromEnv(env(map[string]string{"HARNESS_TRUST_IDENTITY_HEADERS": "1", "HARNESS_IDENTITY_HEADER_SECRET": "short"})); err == nil {
		t.Fatal("a short secret must be refused")
	}
	th, err := TrustedHeadersFromEnv(env(map[string]string{"HARNESS_TRUST_IDENTITY_HEADERS": "1", "HARNESS_IDENTITY_HEADER_SECRET": gwSecret, "HARNESS_IDENTITY_LOGIN_HEADER": "x-user"}))
	if err != nil || th.LoginHeader != "X-User" || th.SecretHeader != "X-Harness-Gateway-Secret" || th.EmailHeader != "X-Forwarded-Email" {
		t.Fatalf("%+v, %v", th, err)
	}
	if _, err := TrustedHeadersFromEnv(env(map[string]string{"HARNESS_TRUST_IDENTITY_HEADERS": "1", "HARNESS_IDENTITY_HEADER_SECRET": gwSecret, "HARNESS_IDENTITY_LOGIN_HEADER": "X-Harness-Gateway-Secret"})); err == nil {
		t.Fatal("two roles on one header must be refused")
	}
}
