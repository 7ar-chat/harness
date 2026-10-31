package product

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestCleanNameNormalizesAndBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"collapses whitespace":      {"  Ada \t  Lovelace\n", "Ada Lovelace", true},
		"empty resets":              {"   ", "", true},
		"exactly 80 runes":          {strings.Repeat("é", 80), strings.Repeat("é", 80), true},
		"81 runes":                  {strings.Repeat("é", 81), "", false},
		"control character":         {"Ada\x07Lovelace", "", false},
		"NUL":                       {"Ada\x00", "", false},
		"unicode name is kept":      {"李 雷", "李 雷", true},
		"80 runes after collapsing": {strings.Repeat("a", 40) + "     " + strings.Repeat("b", 39), strings.Repeat("a", 40) + " " + strings.Repeat("b", 39), true},
	} {
		got, ok := cleanName(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: cleanName(%q) = %q, %v; want %q, %v", name, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestProfilePatchValidatesAndKeepsWhatItIsNotAskedToChange(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)
	r.json("PATCH", "/api/me/profile", "alice", `{"displayName":"Ada"}`, 200)

	// Act / Assert: a control character is refused and the name is unchanged.
	if got := cnErrCode(r.json("PATCH", "/api/me/profile", "alice", `{"displayName":"A\u0007da"}`, 400)); got != "invalid_request" {
		t.Fatalf("code %q", got)
	}
	r.json("PATCH", "/api/me/profile", "alice", `{"displayName":`, 400)
	// A patch without displayName leaves it alone (null and absent are the same: nothing to change).
	for _, body := range []string{`{}`, `{"displayName":null}`} {
		if u := r.json("PATCH", "/api/me/profile", "alice", body, 200); u["displayName"] != "Ada" || u["name"] != "Ada" {
			t.Fatalf("%s changed the profile: %v", body, u)
		}
	}
	// The name of one user does not leak to another.
	if u := r.json("PATCH", "/api/me/profile", "bob", `{}`, 200); u["displayName"] != "" || u["login"] != "bob" {
		t.Fatalf("bob = %v", u)
	}
	// A failing store is an internal error, not a silent success.
	tm.failOn("SetDisplayName", errCnBoom)
	r.json("PATCH", "/api/me/profile", "alice", `{"displayName":"Zed"}`, 500)
	tm.failOn("SetDisplayName", nil)
	tm.failOn("GetUser", errCnBoom)
	r.json("PATCH", "/api/me/profile", "alice", `{}`, 500)
	r.json("DELETE", "/api/me/avatar", "alice", "", 500)
}

func TestProfileOfAnUnregisteredUser(t *testing.T) {
	// Arrange: carol authenticates but never logged in through OIDC, so the store does not know her.
	r, _, _ := cnGitHubRig(t, nil)

	// Act / Assert: reading falls back to the login; writing says the account is not registered yet.
	if u := r.json("PATCH", "/api/me/profile", "carol", `{}`, 200); u["name"] != "carol" || u["defaultName"] != "carol" || u["hasCustomAvatar"] != false {
		t.Fatalf("fallback profile = %v", u)
	}
	if got := cnErrCode(r.json("PATCH", "/api/me/profile", "carol", `{"displayName":"Carol"}`, 404)); got != "not_found" {
		t.Fatalf("rename code %q", got)
	}
	if got := cnErrCode(r.json("DELETE", "/api/me/avatar", "carol", "", 404)); got != "not_found" {
		t.Fatalf("avatar delete code %q", got)
	}
	if got := r.raw("PUT", "/api/me/avatar", "carol", newPNG(t), "image/png").StatusCode; got != 404 {
		t.Fatalf("avatar upload = %d", got)
	}
}

func TestAvatarUploadBoundariesAndServing(t *testing.T) {
	// Arrange
	r, _, tm := cnGitHubRig(t, nil)
	img := newPNG(t)
	sized := func(n int) []byte { return append(append([]byte(nil), img...), make([]byte, n-len(img))...) }

	// Act / Assert: exactly at the limit is accepted, one byte over is not, an empty body is not an image.
	if got := r.raw("PUT", "/api/me/avatar", "alice", sized(maxAvatarBytes), "image/png").StatusCode; got != 200 {
		t.Fatalf("avatar at the limit = %d", got)
	}
	if got := r.raw("PUT", "/api/me/avatar", "alice", sized(maxAvatarBytes+1), "image/png").StatusCode; got != 413 {
		t.Fatalf("avatar one byte over = %d", got)
	}
	if got := r.raw("PUT", "/api/me/avatar", "alice", nil, "image/png").StatusCode; got != 415 {
		t.Fatalf("empty avatar = %d", got)
	}
	if got := r.raw("PUT", "/api/me/avatar", "alice", []byte("GIF89a"+strings.Repeat("\x00", 20)), "text/html").StatusCode; got != 200 {
		t.Fatalf("a GIF with a lying content type = %d", got)
	}
	// The image is served with headers that keep it inert and cacheable per version.
	resp := r.do("GET", "/api/users/alice/avatar", "bob", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/gif" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "private") {
		t.Fatalf("avatar response %d %v", resp.StatusCode, resp.Header)
	}
	// Deleting twice is fine; afterwards there is no avatar.
	r.json("DELETE", "/api/me/avatar", "alice", "", 200)
	r.json("DELETE", "/api/me/avatar", "alice", "", 200)
	if got := cnErrCode(r.json("GET", "/api/users/alice/avatar", "bob", "", 404)); got != "not_found" {
		t.Fatalf("code %q", got)
	}
	// A store failure while serving is not reported as "no avatar".
	tm.failOn("GetAvatar", errCnBoom)
	r.json("GET", "/api/users/alice/avatar", "bob", "", 500)
}

func TestViewOfFallsBackToTheLogin(t *testing.T) {
	// Arrange / Act
	v := viewOf(store.User{Login: "alice"})
	named := viewOf(store.User{Login: "alice", Name: "Alice A", DisplayName: "Ali", AvatarURL: "https://idp/pic", Email: "a@x.io"})

	// Assert
	if v.Name != "alice" || v.DefaultName != "alice" || v.HasCustomAvatar {
		t.Fatalf("view = %+v", v)
	}
	if named.Name != "Ali" || named.DefaultName != "Alice A" || named.AvatarURL != "https://idp/pic" || named.Email != "a@x.io" {
		t.Fatalf("view = %+v", named)
	}
	if up := viewOf(store.User{Login: "a b", AvatarVersion: "v1"}); !up.HasCustomAvatar || up.AvatarURL != "/api/users/a%20b/avatar?v=v1" {
		t.Fatalf("uploaded avatar view = %+v", up)
	}
}
