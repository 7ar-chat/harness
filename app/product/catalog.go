package product

import "github.com/7ar-chat/harness/core/capabilities"

// Catalog is the fixture of MCP servers the product ships: harness-web and harness-worker seed it into the
// capability store at startup. Every URL, header and tool name comes from the vendor's own documentation (the
// page is named at each entry); the URL is fixed here so that enabling an entry never lets anyone point it at an
// address of their choosing. An entry that lists no Tools exposes only the tools its server marks read-only.
func Catalog() []capabilities.Entry {
	return []capabilities.Entry{
		{
			// https://exa.ai/docs/get-started/exa-mcp: keyless at a free rate limit, or an API key in x-api-key.
			ID:          "exa",
			Name:        "Exa",
			Description: "Web search and page fetching for the agent. Works without a key at a free rate limit; add an Exa API key for production limits.",
			Category:    "Search",
			Homepage:    "https://exa.ai/docs/get-started/exa-mcp",
			URL:         "https://mcp.exa.ai/mcp",
			Transport:   capabilities.TransportHTTP,
			Headers:     map[string]string{"x-api-key": "${api_key}"},
			Fields: []capabilities.Field{{
				Key: "api_key", Label: "API key", Secret: true,
				Help: "Optional. Create one at dashboard.exa.ai/api-keys.",
			}},
			Tools:          []string{"web_search_exa", "web_fetch_exa"},
			DefaultEnabled: true,
		},
		{
			// https://context7.com/docs/resources/all-clients: the API key goes in "Authorization: Bearer".
			ID:          "context7",
			Name:        "Context7",
			Description: "Up-to-date documentation and code examples for libraries and frameworks.",
			Category:    "Documentation",
			Homepage:    "https://context7.com/docs",
			URL:         "https://mcp.context7.com/mcp",
			Transport:   capabilities.TransportHTTP,
			Headers:     map[string]string{"Authorization": "Bearer ${api_key}"},
			Fields: []capabilities.Field{{
				Key: "api_key", Label: "API key", Secret: true,
				Help: "Optional. Create one in the Context7 dashboard for higher rate limits.",
			}},
			Tools: []string{"resolve-library-id", "query-docs"},
		},
		{
			// https://docs.devin.ai/work-with-devin/deepwiki-mcp: free, no authentication, public repositories.
			ID:          "deepwiki",
			Name:        "DeepWiki",
			Description: "Read the generated documentation of public GitHub repositories and ask questions about them.",
			Category:    "Documentation",
			Homepage:    "https://docs.devin.ai/work-with-devin/deepwiki-mcp",
			URL:         "https://mcp.deepwiki.com/mcp",
			Transport:   capabilities.TransportHTTP,
			Tools:       []string{"read_wiki_structure", "read_wiki_contents", "ask_question"},
		},
		{
			// https://github.com/github/github-mcp-server/blob/main/docs/remote-server.md: a token in a bearer header.
			ID:          "github",
			Name:        "GitHub",
			Description: "Repositories, issues, pull requests and code search through GitHub's hosted MCP server. Only read-only tools are offered unless you choose others.",
			Category:    "Development",
			Homepage:    "https://github.com/github/github-mcp-server/blob/main/docs/remote-server.md",
			URL:         "https://api.githubcopilot.com/mcp/",
			Transport:   capabilities.TransportHTTP,
			Headers:     map[string]string{"Authorization": "Bearer ${token}"},
			Fields: []capabilities.Field{{
				Key: "token", Label: "Personal access token", Secret: true, Required: true,
				Help: "A GitHub personal access token with the scopes the tools need.",
			}},
		},
	}
}
