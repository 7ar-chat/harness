package product

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/7ar-chat/harness/adapters/store/githoststore"
	"github.com/7ar-chat/harness/adapters/store/reqstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
)

const userToken = "glpat-aaaaaaaaaaaaaaaaaaaa"

type ghEnv struct {
	*rig
	tm   *teamstore.Mem
	gh   *GitHosts
	host string // the registered host's name
	api  *httptest.Server
}

// newGhEnv is a team server where alice administers, bob and carol use, and a fake GitLab accepts userToken.
func newGhEnv(t *testing.T) *ghEnv {
	t.Helper()
	e := &ghEnv{tm: teamstore.NewMem()}
	cnUsers(t, e.tm, "alice", "bob", "carol")
	e.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" && r.Header.Get("PRIVATE-TOKEN") == userToken {
			_, _ = w.Write([]byte(`{"username":"bob"}`))
			return
		}
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(e.api.Close)
	var err error
	e.gh, err = NewGitHosts(githoststore.NewMem(nil), e.tm, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	e.rig = newRig(t, rigOpts{team: true, prod: Options{Team: e.tm, Admins: []string{"alice"}, GitHosts: e.gh}})
	e.host = strings.TrimPrefix(e.api.URL, "http://")
	return e
}

func (e *ghEnv) register(extra string) {
	e.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"kind":"gitlab","baseUrl":%q%s}`, e.host, e.api.URL, extra)
	e.json("POST", "/api/admin/git-hosts", "alice", body, 200)
}

func TestOnlyAdminsRegisterGitHosts(t *testing.T) {
	e := newGhEnv(t)
	body := fmt.Sprintf(`{"name":%q,"kind":"gitlab","baseUrl":%q}`, e.host, e.api.URL)
	e.json("POST", "/api/admin/git-hosts", "bob", body, 403)
	e.json("DELETE", "/api/admin/git-hosts/"+e.host, "bob", "", 403)
	e.json("POST", "/api/admin/git-hosts", "alice", `{"name":"github.com","kind":"gitlab"}`, 400)
	e.json("POST", "/api/admin/git-hosts", "alice", `{"name":"x.example.com","kind":"nope"}`, 400)
	e.register("")
	hosts := e.json("GET", "/api/git-hosts", "bob", "", 200)
	list := hosts["hosts"].([]any)
	if len(list) != 1 || hosts["admin"] != false || list[0].(map[string]any)["name"] != e.host {
		t.Fatalf("hosts = %v", hosts)
	}
	if e.json("GET", "/api/git-hosts", "alice", "", 200)["admin"] != true {
		t.Fatal("the admin is not told so")
	}
	// An unregistered host takes no credentials.
	e.json("PUT", "/api/me/git-hosts/other.example.com/token", "bob", `{"token":"x"}`, 404)
	e.json("DELETE", "/api/admin/git-hosts/"+e.host, "alice", "", 204)
	if _, err := e.gh.Host(context.Background(), e.host); err == nil {
		t.Fatal("the host is still registered")
	}
}

func TestATokenIsCheckedSealedAndNeverReturned(t *testing.T) {
	e := newGhEnv(t)
	e.register("")
	path := "/api/me/git-hosts/" + e.host + "/token"
	e.json("PUT", path, "bob", `{"token":"glpat-wrongwrongwrongwrong"}`, 400)
	e.json("PUT", path, "bob", `{"token":""}`, 400)
	resp := e.do("PUT", path, "bob", `{"token":"`+userToken+`","username":"bobby"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := rawBody(e.do("GET", "/api/git-hosts", "bob", "").Body)
	if strings.Contains(body, userToken) || !strings.Contains(body, `"hasToken":true`) || !strings.Contains(body, "bobby") {
		t.Fatalf("view = %s", body)
	}
	other := rawBody(e.do("GET", "/api/git-hosts", "carol", "").Body)
	if !strings.Contains(other, `"hasToken":false`) {
		t.Fatalf("another user sees a token: %s", other)
	}
	cn, err := e.tm.GetConnector(context.Background(), "bob", connGitToken+e.host)
	if err != nil || strings.Contains(string(cn.Data), userToken) {
		t.Fatalf("the token is stored in clear: %v", err)
	}
	cred, err := e.gh.Credential(context.Background(), "bob", e.host)
	if err != nil || cred.Token != userToken || cred.Username != "bobby" {
		t.Fatalf("cred = %+v, %v", cred, err)
	}
	if _, err := e.gh.Credential(context.Background(), "carol", e.host); err == nil {
		t.Fatal("carol borrowed bob's token")
	}
	e.json("DELETE", path, "bob", "", 204)
	if _, err := e.gh.Credential(context.Background(), "bob", e.host); err == nil {
		t.Fatal("the token survived its deletion")
	}
}

func TestTheHostsServiceTokenServesUsersWithoutOne(t *testing.T) {
	e := newGhEnv(t)
	e.register(`,"serviceToken":"svc-token-12345678","serviceUsername":"ci-bot"`)
	cred, err := e.gh.Credential(context.Background(), "carol", e.host)
	if err != nil || cred.Token != "svc-token-12345678" || cred.Username != "ci-bot" {
		t.Fatalf("cred = %+v, %v", cred, err)
	}
	body := rawBody(e.do("GET", "/api/git-hosts", "carol", "").Body)
	if strings.Contains(body, "svc-token") || !strings.Contains(body, `"hasServiceToken":true`) {
		t.Fatalf("view = %s", body)
	}
	// Updating the host keeps the token unless asked to clear it.
	e.register("")
	if _, err := e.gh.Credential(context.Background(), "carol", e.host); err != nil {
		t.Fatal("the service token was dropped by an update")
	}
	e.register(`,"clearServiceToken":true`)
	if _, err := e.gh.Credential(context.Background(), "carol", e.host); err == nil {
		t.Fatal("the service token was not cleared")
	}
}

func TestGeneratedSSHKeysStaySealedAndShowOnlyThePublicHalf(t *testing.T) {
	e := newGhEnv(t)
	e.register("")
	path := "/api/me/git-hosts/" + e.host + "/ssh-key"
	out := e.json("POST", path, "bob", "", 200)
	pub, _ := out["publicKey"].(string)
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.HasPrefix(out["fingerprint"].(string), "SHA256:") {
		t.Fatalf("out = %v", out)
	}
	cred, err := e.gh.Credential(context.Background(), "bob", e.host)
	if err != nil || !strings.Contains(string(cred.SSHKey), "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("cred: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(cred.SSHKey)
	if err != nil || !strings.HasPrefix(pub, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))) {
		t.Fatalf("the private key does not match the public one: %v", err)
	}
	body := rawBody(e.do("GET", "/api/git-hosts", "bob", "").Body)
	if strings.Contains(body, "PRIVATE KEY") || !strings.Contains(body, `"hasSshKey":true`) || !strings.Contains(body, pub) {
		t.Fatalf("view = %s", body)
	}
	cn, _ := e.tm.GetConnector(context.Background(), "bob", connGitSSH+e.host)
	if strings.Contains(string(cn.Data), "PRIVATE KEY") {
		t.Fatal("the private key is stored in clear")
	}
	e.json("DELETE", path, "bob", "", 204)
	if _, err := e.gh.Credential(context.Background(), "bob", e.host); err == nil {
		t.Fatal("the key survived its deletion")
	}
}

func TestScanKeysReadsTheHostKeysTheServerPresents(t *testing.T) {
	e := newGhEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if sc, _, _, err := ssh.NewServerConn(c, cfg); err == nil {
					sc.Close()
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	e.register(fmt.Sprintf(`,"sshHost":"127.0.0.1","sshPort":%d`, port))
	e.json("POST", "/api/admin/git-hosts/"+e.host+"/scan-keys", "bob", "", 403)
	out := e.json("POST", "/api/admin/git-hosts/"+e.host+"/scan-keys", "alice", "", 200)
	keys, _ := out["knownHosts"].([]any)
	want := fmt.Sprintf("[127.0.0.1]:%d %s", port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("keys = %v, want %q", keys, want)
	}
}

func TestEgressListsTheRegisteredHosts(t *testing.T) {
	e := newGhEnv(t)
	e.register(`,"sshHost":"ssh.example.com"`)
	got := e.gh.Egress(context.Background())
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "ssh.example.com") || !strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("egress = %s", b)
	}
}

func TestIssuesOfAProjectOnARegisteredHostBecomeRequirements(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{}`))
		case "/api/v4/projects/grp%2Fproj/issues":
			if r.Header.Get("PRIVATE-TOKEN") != userToken {
				http.Error(w, `{"message":"401"}`, 401)
				return
			}
			_, _ = w.Write([]byte(`[{"iid":3,"title":"Crash on save","description":"steps","web_url":"https://x/3","labels":["bug"],"author":{"username":"zoe"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gl.Close()
	tm := teamstore.NewMem()
	cnUsers(t, tm, "alice", "bob")
	gh, err := NewGitHosts(githoststore.NewMem(nil), tm, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, rigOpts{team: true, prod: Options{Team: tm, Admins: []string{"alice"}, GitHosts: gh, Requirements: reqstore.NewMem(nil)}})
	host := strings.TrimPrefix(gl.URL, "http://")
	r.json("POST", "/api/admin/git-hosts", "alice", fmt.Sprintf(`{"name":%q,"kind":"gitlab","baseUrl":%q}`, host, gl.URL), 200)
	pid := r.json("POST", "/api/projects", "alice", fmt.Sprintf(`{"name":"p","repoUrl":"https://%s/grp/proj.git"}`, host), 201)["id"].(string)
	r.json("PUT", "/api/projects/"+pid+"/members/bob", "alice", `{"role":"member"}`, 204)
	path := "/api/projects/" + pid + "/requirements/import/issues"

	// Without a token the host answers 401 and the user is told where to add one.
	resp := r.do("POST", path, "bob", `{"label":"bug"}`)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(rawBody(resp.Body), "Git access") {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	r.json("PUT", "/api/me/git-hosts/"+host+"/token", "bob", `{"token":"`+userToken+`"}`, 204)
	out := r.json("POST", path, "bob", `{"label":"bug"}`, 200)
	if out["imported"] != float64(1) || out["skipped"] != float64(0) {
		t.Fatalf("out = %v", out)
	}
	req := out["requirements"].([]any)[0].(map[string]any)
	src := req["source"].(map[string]any)
	if req["title"] != "Crash on save" || src["kind"] != "gitlab_issue" || src["repo"] != host+"/grp/proj" || src["number"] != float64(3) {
		t.Fatalf("requirement = %v", req)
	}
	again := r.json("POST", path, "bob", `{}`, 200)
	if again["imported"] != float64(0) || again["skipped"] != float64(1) {
		t.Fatalf("a second import must dedupe: %v", again)
	}
}

func TestAServerWithoutGitHostsAnswersAnEmptyDisabledList(t *testing.T) {
	r := newRig(t, rigOpts{team: true})
	out := r.json("GET", "/api/git-hosts", "alice", "", 200)
	if out["enabled"] != false || len(out["hosts"].([]any)) != 0 {
		t.Fatalf("out = %v", out)
	}
	r.json("POST", "/api/admin/git-hosts", "alice", `{"name":"x.example.com","kind":"gitea"}`, 404)
}
