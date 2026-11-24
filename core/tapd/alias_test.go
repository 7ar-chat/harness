package tapd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/tapd"
)

func TestStoryURLTemplateAndStatusAliases(t *testing.T) {
	f := newFake(t)
	f.stories = []string{story(1, "planning"), story(2, "resolved"), story(3, "todo_x"), story(4, "done")}
	c := client(f)
	c.Cfg.StoryURL = "https://tapd.example.com/{workspace}/s/{id}?from=7ar"
	c.Cfg.StatusAliases = "done=closed, todo_x=open, planning=closed"
	res, err := c.OpenStories(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range res.Stories {
		ids = append(ids, s.ID)
	}
	// planning closed by alias, resolved closed by the workflow, todo_x open, done closed by alias.
	if strings.Join(ids, ",") != "3" {
		t.Fatalf("open ids = %v", ids)
	}
	if got := res.Stories[0].URL; got != "https://tapd.example.com/1010104801/s/3?from=7ar" {
		t.Fatalf("url = %q", got)
	}
}

func TestValidStoryURLAndParseAliases(t *testing.T) {
	for _, ok := range []string{"", tapd.DefaultStoryURL, "https://h.example.com/x/{id}"} {
		if err := tapd.ValidStoryURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://h.example.com/{id}", "https://h.example.com/nothing", "https://u:p@h.example.com/{id}", "https://h.example.com/{id}/{other}", "javascript:alert({id})", "https:///{id}", "https://h.example.com/ {id}"} {
		if tapd.ValidStoryURL(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	m, err := tapd.ParseAliases("Done=closed; 待办 = open\nx=OPEN")
	if err != nil || len(m) != 3 || m["done"] || !m["待办"] || !m["x"] {
		t.Fatalf("aliases = %v, %v", m, err)
	}
	for _, bad := range []string{"done", "done=maybe", "=open"} {
		if _, err := tapd.ParseAliases(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
