package capabilities

import (
	"reflect"
	"strings"
	"testing"
)

func entry(id string, mod func(*Entry)) Entry {
	e := Entry{ID: id, Name: id, URL: "https://" + id + ".example.com/mcp", Transport: TransportHTTP}
	if mod != nil {
		mod(&e)
	}
	return e
}

func TestValidNameIsASlugThatCannotBreakTheToolPrefix(t *testing.T) {
	cases := map[string]bool{
		"exa": true, "my-docs": true, "a_b": true, "a1": true,
		"": false, "Exa": false, "1abc": false, "a b": false, "a__b": false, "a.b": false,
		strings.Repeat("a", 32): true, strings.Repeat("a", 33): false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := ValidName(name)

			// Assert
			if got != want {
				t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestValidateRejectsAnEntryThatCouldReachAnAddressOrLeakAField(t *testing.T) {
	cases := map[string]struct {
		mod  func(*Entry)
		want string
	}{
		"fine":                 {func(*Entry) {}, ""},
		"plain http":           {func(e *Entry) { e.URL = "http://x.example.com/mcp" }, "https"},
		"bad transport":        {func(e *Entry) { e.Transport = "stdio" }, "transport"},
		"no name":              {func(e *Entry) { e.Name = " " }, "no name"},
		"bad id":               {func(e *Entry) { e.ID = "Bad Id" }, "valid name"},
		"unknown placeholder":  {func(e *Entry) { e.Headers = map[string]string{"x-api-key": "${nope}"} }, "not a field"},
		"bad field key":        {func(e *Entry) { e.Fields = []Field{{Key: "Api-Key"}} }, "bad field key"},
		"required but default": {func(e *Entry) { e.DefaultEnabled = true; e.Fields = []Field{{Key: "k", Required: true}} }, "on by default"},
		"optional and default": {func(e *Entry) { e.DefaultEnabled = true; e.Fields = []Field{{Key: "k"}} }, ""},
		"declared placeholder": {func(e *Entry) { e.Fields = []Field{{Key: "k"}}; e.Headers = map[string]string{"a": "Bearer ${k}"} }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			e := entry("srv", tc.mod)

			// Act
			err := e.Validate()

			// Assert
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Validate() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func TestExpandLeavesOutAHeaderWhoseValueIsNotGiven(t *testing.T) {
	// Arrange
	e := entry("srv", func(e *Entry) {
		e.Fields = []Field{{Key: "token"}, {Key: "org"}}
		e.Headers = map[string]string{
			"Authorization": "Bearer ${token}",
			"x-org":         "${org}",
			"x-static":      "1",
			"x-both":        "${org}:${token}",
		}
	})

	// Act
	got := e.Expand(map[string]string{"token": "s3cret", "org": ""})

	// Assert
	want := map[string]string{"Authorization": "Bearer s3cret", "x-static": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand = %v, want %v (an empty or absent field drops every header that uses it)", got, want)
	}
}

func TestEffectiveAppliesTheCatalogDefaultUnlessTheScopeDecided(t *testing.T) {
	catalog := []Entry{
		entry("on", func(e *Entry) { e.DefaultEnabled = true }),
		entry("off", nil),
	}
	cases := map[string]struct {
		installs []Install
		want     map[string]bool
	}{
		"as shipped":        {nil, map[string]bool{"on": true, "off": false}},
		"turn one off":      {[]Install{{CatalogID: "on", Name: "on", Enabled: false}}, map[string]bool{"on": false, "off": false}},
		"turn one on":       {[]Install{{CatalogID: "off", Name: "off", Enabled: true}}, map[string]bool{"on": true, "off": true}},
		"install of a stub": {[]Install{{CatalogID: "off", Name: "off", Enabled: false}}, map[string]bool{"on": true, "off": false}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			items := Effective(catalog, tc.installs)

			// Assert
			got := map[string]bool{}
			for _, it := range items {
				got[it.Name] = it.Enabled
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnItemWithAMissingRequiredFieldIsNeverEnabled(t *testing.T) {
	// Arrange
	catalog := []Entry{entry("gh", func(e *Entry) {
		e.Fields = []Field{{Key: "token", Required: true}, {Key: "org"}}
	})}
	on := Install{CatalogID: "gh", Name: "gh", Enabled: true}

	// Act
	without := Effective(catalog, []Install{on})[0]
	on.SecretKeys = []string{"token"}
	with := Effective(catalog, []Install{on})[0]

	// Assert
	if without.Enabled || !reflect.DeepEqual(without.Missing, []string{"token"}) {
		t.Errorf("without the token: enabled=%v missing=%v, want disabled and missing [token]", without.Enabled, without.Missing)
	}
	if !with.Enabled || len(with.Missing) != 0 {
		t.Errorf("with the token: enabled=%v missing=%v, want enabled", with.Enabled, with.Missing)
	}
}

func TestEffectiveListsCustomServersAndFlagsOrphans(t *testing.T) {
	// Arrange
	catalog := []Entry{entry("exa", func(e *Entry) { e.DefaultEnabled = true })}
	installs := []Install{
		{ID: "1", CatalogID: "", Name: "zeta", URL: "https://z.example.com/mcp", Enabled: true},
		{ID: "2", CatalogID: "gone", Name: "gone", Enabled: true},
		{ID: "3", CatalogID: "", Name: "alpha", URL: "https://a.example.com/mcp", Enabled: false},
	}

	// Act
	items := Effective(catalog, installs)

	// Assert
	var names []string
	byName := map[string]Item{}
	for _, it := range items {
		names = append(names, it.Name)
		byName[it.Name] = it
	}
	if want := []string{"alpha", "exa", "gone", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v (sorted)", names, want)
	}
	if o := byName["gone"]; !o.Orphan || o.Enabled || o.Entry != nil {
		t.Errorf("orphan = %+v, want an orphan that is never enabled", o)
	}
	if c := byName["zeta"]; c.Entry != nil || !c.Enabled || c.Orphan {
		t.Errorf("custom = %+v, want enabled with no entry", c)
	}
	if byName["alpha"].Enabled {
		t.Error("a custom server that is switched off must stay off")
	}
}

func TestAllowedToolsPrefersTheInstallThenTheEntryThenNone(t *testing.T) {
	e := entry("srv", func(e *Entry) { e.Tools = []string{"search"} })
	cases := map[string]struct {
		it   Item
		want []string
	}{
		"install wins":      {Item{Entry: &e, Install: &Install{Tools: []string{"fetch"}}}, []string{"fetch"}},
		"entry default":     {Item{Entry: &e, Install: &Install{}}, []string{"search"}},
		"entry, no install": {Item{Entry: &e}, []string{"search"}},
		"custom, nothing":   {Item{Install: &Install{}}, nil},
		"entry, no tools":   {Item{Entry: &Entry{}}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.it.AllowedTools(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AllowedTools = %v, want %v", got, tc.want)
			}
		})
	}
}
