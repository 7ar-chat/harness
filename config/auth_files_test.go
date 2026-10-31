package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestReadAuth_NoDirOrNoFileIsAnEmptyMapNotAnError(t *testing.T) {
	for name, dir := range map[string]string{"empty dir name": "", "missing file": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			keys, err := config.ReadAuth(dir)
			if err != nil || keys == nil || len(keys) != 0 {
				t.Fatalf("keys %#v err %v", keys, err)
			}
		})
	}
}

func TestReadAuth_EmptyFileHasNoKeys(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	writeAuthFile(t, dir, " \n\t", 0o600)

	// Act
	keys, err := config.ReadAuth(dir)

	// Assert
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys %v err %v", keys, err)
	}
}

func TestReadAuth_InvalidContentNamesTheFileAndNeverEchoesTheKey(t *testing.T) {
	cases := map[string]struct {
		body    string
		mention string
	}{
		"syntax error":   {`{"providers": {"a": {"api_key": "sk-secret-aaaa"`, "syntax error at byte"},
		"trailing junk":  {`{"providers": {"a": {"api_key": "sk-secret-bbbb"}}} sk-secret-bbbb`, "syntax error at byte"},
		"wrong key type": {`{"providers": {"a": {"api_key": 12345678}}}`, "wrong type for"},
		"wrong shape":    {`{"providers": "sk-secret-cccc"}`, "wrong type for providers"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			dir := t.TempDir()
			file := writeAuthFile(t, dir, tc.body, 0o600)

			// Act
			_, err := config.ReadAuth(dir)

			// Assert
			if err == nil {
				t.Fatal("invalid file accepted")
			}
			msg := err.Error()
			if !strings.Contains(msg, file) || !strings.Contains(msg, tc.mention) {
				t.Errorf("error should name the file and the kind of failure: %v", err)
			}
			if strings.Contains(msg, "sk-secret") || strings.Contains(msg, "12345678") {
				t.Errorf("error leaks file content: %v", err)
			}
		})
	}
}

func TestReadAuth_UnreadableEntryReportsTheFile(t *testing.T) {
	// Arrange: auth.json is a directory, so reading it fails after the mode check.
	dir := t.TempDir()
	file := filepath.Join(dir, config.AuthFile)
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := config.ReadAuth(dir)

	// Assert
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("err %v", err)
	}
}

func TestReadAuth_StatFailureOtherThanNotExistIsReported(t *testing.T) {
	// Arrange: the "directory" is a regular file, so statting inside it gives ENOTDIR.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Act
	keys, err := config.ReadAuth(blocker)

	// Assert
	if err == nil || keys != nil || !strings.Contains(err.Error(), config.AuthFile) {
		t.Fatalf("keys %v err %v", keys, err)
	}
}

func TestSetAuthKey_RejectsBadInputBeforeTouchingDisk(t *testing.T) {
	cases := []struct {
		name, dir, provider, key, want string
	}{
		{"blank key", "", "p", "   \t", "empty API key"},
		{"name starting with dash", "", "-p", "k", "invalid provider name"},
		{"empty name", "", "", "k", "invalid provider name"},
		{"no user directory", "", "p", "key-123456789012", "no user directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.SetAuthKey(tc.dir, tc.provider, tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSetAuthKey_TrimsTheKeyAndReplacesAnExistingOne(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	if err := config.SetAuthKey(dir, "p", "old-key-000000000"); err != nil {
		t.Fatal(err)
	}

	// Act
	err := config.SetAuthKey(dir, "p", "  new-key-111111111\n")
	keys, rerr := config.ReadAuth(dir)

	// Assert
	if err != nil || rerr != nil {
		t.Fatalf("%v %v", err, rerr)
	}
	if len(keys) != 1 || keys["p"] != "new-key-111111111" {
		t.Fatalf("keys %v", keys)
	}
}

func TestSetAuthKey_FailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	// Arrange: a regular file stands where the directory should be.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Act
	err := config.SetAuthKey(filepath.Join(blocker, "sub"), "p", "key-123456789012")

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestSetAuthKey_FailsAndLeavesNothingWhenTheDirectoryIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions and a non-root user")
	}
	// Arrange
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Act
	err := config.SetAuthKey(dir, "p", "key-123456789012")

	// Assert
	if err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
	_ = os.Chmod(dir, 0o700)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestRemoveAuthKey_UnknownProviderChangesNothing(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	if err := config.SetAuthKey(dir, "a", "key-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, config.AuthFile))

	// Act
	ok, err := config.RemoveAuthKey(dir, "zzz")

	// Assert
	if ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, config.AuthFile))
	if string(before) != string(after) {
		t.Error("the file was rewritten for nothing")
	}
}

func TestRemoveAuthKey_NoFileAndNoDirCreateNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")

	ok, err := config.RemoveAuthKey(dir, "a")

	if ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("removing from nothing must not create the directory")
	}
}

func TestRemoveAuthKey_SurfacesAReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := t.TempDir()
	writeAuthFile(t, dir, authBody, 0o644)

	ok, err := config.RemoveAuthKey(dir, "openai")

	if ok || err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

func TestRemoveAuthKey_LastKeyLeavesAValidEmptyFile(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	if err := config.SetAuthKey(dir, "a", "key-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}

	// Act
	ok, err := config.RemoveAuthKey(dir, "a")
	keys, rerr := config.ReadAuth(dir)

	// Assert
	if !ok || err != nil || rerr != nil || len(keys) != 0 {
		t.Fatalf("ok %v err %v read %v keys %v", ok, err, rerr, keys)
	}
}

func TestAuthProviders_AreSorted(t *testing.T) {
	got := config.AuthProviders(map[string]string{"zeta": "1", "alpha": "2", "Mid": "3"})

	if want := []string{"Mid", "alpha", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := config.AuthProviders(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestValidProviderName(t *testing.T) {
	cases := map[string]bool{
		"opencode": true, "a": true, "9lives": true, "my.prov_1-b": true,
		"": false, "-x": false, ".x": false, "_x": false, "a b": false, "a/b": false, "é": false, "a\n": false,
	}
	for name, want := range cases {
		if got := config.ValidProviderName(name); got != want {
			t.Errorf("ValidProviderName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAuthPath_IsInsideTheUserDir(t *testing.T) {
	if got, want := config.AuthPath("/x/y"), filepath.Join("/x/y", config.AuthFile); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
