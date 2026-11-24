package product

import "testing"

func TestFactoryStatusTellsMembersWhetherTheReviewIsIndependentAndNobodyElse(t *testing.T) {
	// Arrange: the three review modes (and none set), asked by an owner, a member, a viewer and an outsider.
	for mode, want := range map[string]bool{ReviewIndependent: true, ReviewSameModel: false, ReviewOff: false, "": false} {
		mode := mode
		e := newFxEnv(t, fxOpts{prod: func(o *Options) { o.Factory.Review = mode }})
		path := "/api/projects/" + e.project + "/factory/status"
		// Act
		owner := e.json("GET", path, "alice", "", 200)
		member := e.json("GET", path, "bob", "", 200)
		// Assert
		if owner["independentReview"] != want || member["independentReview"] != want {
			t.Errorf("mode %q: independentReview owner %v member %v, want %v", mode, owner["independentReview"], member["independentReview"], want)
		}
		wantMode := mode
		if mode == "" {
			wantMode = ReviewOff
		}
		if owner["review"] != wantMode {
			t.Errorf("mode %q: review = %v", mode, owner["review"])
		}
		if owner["canAutoMerge"] != true || member["canAutoMerge"] != false {
			t.Errorf("canAutoMerge owner %v member %v", owner["canAutoMerge"], member["canAutoMerge"])
		}
		if _, leaks := owner["reviewModel"]; leaks {
			t.Errorf("the model name must not be exposed: %v", owner)
		}
		if got := e.do("GET", path, "carol", "").StatusCode; got != 403 {
			t.Errorf("viewer: %d, want 403", got)
		}
		if got := e.do("GET", path, "dave", "").StatusCode; got != 404 {
			t.Errorf("outsider: %d, want 404", got)
		}
	}
}

func TestReviewModeNamesWhoReviews(t *testing.T) {
	cases := map[string]struct {
		model           string
		noReview, forge bool
		want            string
	}{
		"second model":        {"m2", false, true, ReviewIndependent},
		"same model":          {"", false, true, ReviewSameModel},
		"opt out":             {"m2", true, true, ReviewOff},
		"no forge, no review": {"", false, false, ReviewOff},
	}
	for name, c := range cases {
		if got := ReviewMode(c.model, c.noReview, c.forge); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
