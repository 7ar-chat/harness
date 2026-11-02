package product

import (
	"net/url"
	"testing"
)

func TestTheShippedCatalogIsValid(t *testing.T) {
	// Arrange
	catalog := Catalog()
	ids := map[string]bool{}

	// Act / Assert
	if len(catalog) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, e := range catalog {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.ID, err)
		}
		if ids[e.ID] {
			t.Errorf("duplicate id %s", e.ID)
		}
		ids[e.ID] = true
		if u, err := url.Parse(e.URL); err != nil || u.User != nil || u.Scheme != "https" {
			t.Errorf("%s: bad URL %q", e.ID, e.URL)
		}
		if e.Description == "" || e.Homepage == "" {
			t.Errorf("%s: a catalog entry needs a description and a homepage", e.ID)
		}
		if err := checkServerURL(e.URL); err != nil {
			t.Errorf("%s: the product would refuse its own URL: %v", e.ID, err)
		}
	}
}

func TestOnlyKeylessEntriesAreOnByDefault(t *testing.T) {
	on := map[string]bool{}
	for _, e := range Catalog() {
		if e.DefaultEnabled {
			on[e.ID] = true
		}
	}
	if !on["exa"] || on["github"] || len(on) != 1 {
		t.Fatalf("on by default: %v (want only exa)", on)
	}
}

func TestExaSendsItsKeyOnlyWhenOneIsGivenAndGitHubRequiresItsToken(t *testing.T) {
	for _, e := range Catalog() {
		switch e.ID {
		case "exa":
			if len(e.Expand(nil)) != 0 || e.Expand(map[string]string{"api_key": "K"})["x-api-key"] != "K" {
				t.Errorf("exa headers: keyless %v", e.Expand(nil))
			}
		case "github":
			if len(e.Fields) != 1 || !e.Fields[0].Required || e.Expand(map[string]string{"token": "T"})["Authorization"] != "Bearer T" {
				t.Errorf("github: %+v", e)
			}
		}
	}
}
