package product

import "testing"

func TestConnectTapdStoresTheStoryLinkPatternAndStatusAliases(t *testing.T) {
	f := newFakeTapd(t)
	e := newTapdEnv(t, true)
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"storyUrl":"http://x.example.com/{id}"`), 400)
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"storyUrl":"https://x.example.com/nothing"`), 400)
	e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"statusAliases":"planning=maybe"`), 400)
	st := e.json("PUT", e.path("/tapd"), "alice", tapdConnectBody(f, `,"storyUrl":"https://tapd.example.com/{workspace}/s/{id}","statusAliases":"planning=closed, developing=open"`), 200)
	if st["storyUrl"] != "https://tapd.example.com/{workspace}/s/{id}" || st["statusAliases"] != "planning=closed, developing=open" {
		t.Fatalf("status = %v", st)
	}
	out := e.json("POST", e.path("/import/tapd"), "bob", "", 200)
	// planning is closed by alias; resolved is an end status; only developing ("Export") imports.
	if out["imported"].(float64) != 1 {
		t.Fatalf("import = %v", out)
	}
	src := out["requirements"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["url"] != "https://tapd.example.com/1010104801/s/1010104801003" {
		t.Fatalf("source = %v", src)
	}
}
