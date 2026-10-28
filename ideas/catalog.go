// Package ideas is the catalog of Work Chat ideas: ready-made tasks with demo
// files, copied from Work's request ideas (astraforge/requests/ideas.py).
// Like Work, the catalog is English-only and server-side.
package ideas

import (
	"strings"
	"sync"
)

// Content types of the demo files.
const (
	Text     = "text/plain"
	Markdown = "text/markdown"
	CSV      = "text/csv"
	XLSX     = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	PDF      = "application/pdf"
	DOCX     = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// DemoFile is a demo file of an idea, uploaded into the conversation when the
// idea is used.
type DemoFile struct {
	Name        string
	ContentType string
	// Content is Work's inline text of the file.
	Content string
	// Data is the uploadable file: Content itself for text files, a real
	// document built from Content for PDF and Office files (Work Chat's
	// uploads reject a .pdf/.docx/.xlsx whose bytes are plain text).
	Data []byte
}

// Binary reports whether Data is not Content (a generated PDF/Office document).
func (f DemoFile) Binary() bool {
	return f.ContentType == PDF || f.ContentType == DOCX || f.ContentType == XLSX
}

// Idea is one entry of the catalog.
type Idea struct {
	ID                    string
	Title                 string
	Summary               string
	Prompt                string
	FakeFiles             []string
	Category              string
	FeaturedOnHome        bool
	RecommendedSkillSlugs []string
}

// DemoFiles are the demo files of FakeFiles, in order.
func (i Idea) DemoFiles() []DemoFile {
	out := make([]DemoFile, 0, len(i.FakeFiles))
	for _, name := range i.FakeFiles {
		out = append(out, demoFile(name))
	}
	return out
}

func lines(s ...string) string { return strings.Join(s, "\n") }

type demoContent struct{ contentType, content string }

var demoFileContents = map[string]demoContent{
	"product_launch_brief.md": {Markdown, lines(
		"# Product launch brief",
		"Product: Astra Assist Teams",
		"Goal: help support teams prepare customer responses in under 15 minutes.",
		"Audience: operations leads, customer success managers, level 2 support.",
		"Key messages: time saved, traceability, response quality, collaboration in Cowork.",
		"Constraints: pilot launch in 6 weeks, legal approval before external communication.",
	)},
	"market_demo.csv": {CSV, lines(
		"Segment,Market_size,Growth,Priority",
		"B2B support,12000000,14%,High",
		"Internal operations,8500000,9%,Medium",
		"Customer success,6200000,12%,High",
	)},
	"quarterly_sales.xlsx": {XLSX, lines(
		"Region,Customer,Product,Quarter,Revenue,Volume,Satisfaction",
		"France,Alto Retail,Pro Suite,Q1,42000,84,8.9",
		"France,Alto Retail,Pro Suite,Q2,51000,101,9.1",
		"Benelux,Northwind,Pro Suite,Q1,38000,76,8.4",
		"Benelux,Northwind,Pro Suite,Q2,29000,58,7.2",
		"DACH,Contoso,Team Suite,Q1,61000,122,8.7",
		"DACH,Contoso,Team Suite,Q2,68000,136,8.8",
		"Iberia,Fabrikam,Team Suite,Q1,24000,48,7.9",
		"Iberia,Fabrikam,Team Suite,Q2,33000,66,8.2",
	)},
	"committee_meeting_notes.txt": {Text, lines(
		"Operations committee meeting - raw notes",
		"Decision: approve the Cowork pilot for internal requests starting July 15.",
		"Risk item: clarify the validation process for sensitive attachments.",
		"Action Camille: prepare training material by June 28.",
		"Action Malik: consolidate beta feedback and propose 3 tracking KPIs.",
	)},
	"action_items.csv": {CSV, lines(
		"Action,Owner,Deadline,Status",
		"Training material,Camille,2026-06-28,In progress",
		"Pilot KPIs,Malik,2026-06-25,Not started",
		"Legal validation,Nora,2026-07-02,To schedule",
	)},
	"supplier_contract_demo.pdf": {PDF, lines(
		"Demo supplier contract",
		"Initial term: 24 months, automatic 12-month renewal.",
		"SLA: 99.5% availability, penalties capped at 5% of the monthly invoice.",
		"Data: subcontracting allowed after written notification.",
		"Termination: 90-day notice, exit fees based on exported volume.",
		"Negotiation point: annual security audit and liability cap.",
	)},
	"support_tickets.csv": {CSV, lines(
		"Ticket,Priority,Category,SLA_hours,Resolution_hours,Status",
		"SUP-101,P1,Authentication,4,3,Closed",
		"SUP-102,P2,Performance,12,18,Closed",
		"SUP-103,P3,Product question,48,22,Closed",
		"SUP-104,P2,Integration,12,9,Closed",
		"SUP-105,P1,Data incident,4,7,Open",
	)},
	"client_onboarding.md": {Markdown, lines(
		"# Client onboarding context",
		"Client: Maison Verde",
		"Topic: launch a pilot workspace for 25 users.",
		"Progress: SSO configured, two workshops completed, test data loaded.",
		"Blocker: waiting for security approval on log exports.",
		"Desired tone: reassuring, clear, focused on next steps.",
	)},
	"clients_export.csv": {CSV, lines(
		"id,name,email,country,created_at",
		"1,Alto Retail,contact@alto.example,France,2026-01-04",
		"2,Northwind,,Belgium,04/02/2026",
		"3,alto retail,contact@alto.example,France,2026-01-04",
		"4,Fabrikam,ops@fabrikam.example,Spain,2026-03-12",
		"5,Contoso,contact@contoso.example,,2026/03/18",
	)},
	"partner_portal_project.md": {Markdown, lines(
		"# Partner portal project",
		"Goal: centralize requests, documents, and incident tracking for partners.",
		"V1 scope: authentication, document upload, status tracking, email notifications.",
		"Constraints: pilot before the end of Q3, existing CRM integration, basic mobile support.",
		"Risks: CRM data quality, SSO dependency, business expert availability.",
	)},
	"policy_v1.docx": {DOCX, lines(
		"Remote work policy V1",
		"Remote work allowed 2 days per week after manager approval.",
		"Equipment provided on request. Tuesday presence is mandatory.",
		"Sensitive data must remain in approved tools.",
	)},
	"policy_v2.docx": {DOCX, lines(
		"Remote work policy V2",
		"Remote work allowed 3 days per week after manager and HR approval.",
		"Equipment provided automatically for eligible employees.",
		"Tuesday presence is mandatory and quarterly workshops require attendance.",
		"Sensitive data must remain in approved tools with MFA enabled.",
	)},
	"support_ticket_faq.csv": {CSV, lines(
		"Question,Category,Frequency,Resolution",
		"How do I reset my SSO access?,Authentication,18,Use the identity portal and restart the session.",
		"Why is my export blocked?,Data,11,Check the workspace role and file size.",
		"How do I invite a partner?,Administration,7,Go to Workspaces > Members and use the viewer role.",
		"When should I escalate a P1 incident?,Support,5,Escalate if production impact or data loss is involved.",
	)},
}

// builtDemoFiles are the demo files, generated once.
var builtDemoFiles = sync.OnceValue(func() map[string]DemoFile {
	out := make(map[string]DemoFile, len(demoFileContents))
	for name, c := range demoFileContents {
		out[name] = buildDemoFile(name, c)
	}
	return out
})

// demoFile mirrors Work's _demo_file_payload (unknown names get a text placeholder).
func demoFile(name string) DemoFile {
	if f, ok := builtDemoFiles()[name]; ok {
		return f
	}
	return buildDemoFile(name, demoContent{Text, "Demo file: " + name + "\n"})
}

func buildDemoFile(name string, c demoContent) DemoFile {
	f := DemoFile{Name: name, ContentType: c.contentType, Content: c.content}
	switch c.contentType {
	case PDF:
		f.Data = pdfDocument(c.content)
	case DOCX:
		f.Data = docxDocument(c.content)
	case XLSX:
		f.Data = xlsxDocument(c.content)
	default:
		f.Data = []byte(c.content)
	}
	return f
}

var catalog = []Idea{
	{
		ID:      "generate-presentation",
		Title:   "Generate a presentation",
		Summary: "Turn a demo brief into a clear deck with storyline, messages, and risks.",
		Prompt: "Create an 8-slide PowerPoint presentation as a .pptx file from the " +
			"demo file product_launch_brief.md. Include context, key messages, " +
			"launch plan, risks, and next steps.",
		FakeFiles:             []string{"product_launch_brief.md", "market_demo.csv"},
		Category:              "Office",
		FeaturedOnHome:        true,
		RecommendedSkillSlugs: []string{"anthropic-pptx"},
	},
	{
		ID:      "analyze-excel",
		Title:   "Analyze a spreadsheet",
		Summary: "Read demo sales data and extract trends, anomalies, and actions.",
		Prompt: "Analyze the demo spreadsheet quarterly_sales.xlsx. Identify trends, " +
			"anomalies, top customers, weak regions, and produce a " +
			"management-ready summary.",
		FakeFiles:             []string{"quarterly_sales.xlsx"},
		Category:              "Data",
		FeaturedOnHome:        true,
		RecommendedSkillSlugs: []string{"anthropic-xlsx"},
	},
	{
		ID:      "generate-word-document",
		Title:   "Generate a Word document",
		Summary: "Structure raw meeting notes into a share-ready document.",
		Prompt: "Generate a Word document from the demo notes " +
			"committee_meeting_notes.txt. Produce a structured meeting report with " +
			"decisions, action items, owners, and deadlines.",
		FakeFiles:             []string{"committee_meeting_notes.txt", "action_items.csv"},
		Category:              "Office",
		FeaturedOnHome:        true,
		RecommendedSkillSlugs: []string{"anthropic-docx"},
	},
	{
		ID:      "summarize-pdf",
		Title:   "Summarize a PDF",
		Summary: "Extract key clauses, deadlines, risks, and negotiation points.",
		Prompt: "Read the demo PDF supplier_contract_demo.pdf and produce a summary " +
			"of important clauses, deadlines, contractual risks, and negotiation " +
			"points.",
		FakeFiles:             []string{"supplier_contract_demo.pdf"},
		Category:              "Legal",
		FeaturedOnHome:        true,
		RecommendedSkillSlugs: []string{"anthropic-pdf"},
	},
	{
		ID:      "create-kpi-report",
		Title:   "Create a KPI report",
		Summary: "Turn demo support tickets into a weekly performance report.",
		Prompt: "Using support_tickets.csv, create a weekly KPI report with volumes, " +
			"SLA performance, root causes, priorities, and recommendations.",
		FakeFiles:             []string{"support_tickets.csv"},
		Category:              "Reporting",
		RecommendedSkillSlugs: []string{"anthropic-xlsx"},
	},
	{
		ID:      "draft-client-email",
		Title:   "Draft a professional email",
		Summary: "Write a client follow-up with short and detailed versions.",
		Prompt: "Draft a professional follow-up email from the demo context " +
			"client_onboarding.md. Provide a short version, a detailed version, " +
			"and a clear subject line.",
		FakeFiles:             []string{"client_onboarding.md"},
		Category:              "Communication",
		RecommendedSkillSlugs: []string{"anthropic-internal-comms"},
	},
	{
		ID:      "clean-csv-data",
		Title:   "Clean and restructure data",
		Summary: "Detect duplicates, missing fields, and inconsistent formats.",
		Prompt: "Analyze clients_export.csv, detect duplicates, missing fields, and " +
			"inconsistent formats, then propose a clean version and the cleanup " +
			"rules applied.",
		FakeFiles:             []string{"clients_export.csv"},
		Category:              "Data",
		RecommendedSkillSlugs: []string{"anthropic-xlsx"},
	},
	{
		ID:      "create-project-plan",
		Title:   "Create a project plan",
		Summary: "Turn a business brief into milestones, risks, and deliverables.",
		Prompt: "Turn the demo brief partner_portal_project.md into a project plan " +
			"with objectives, milestones, dependencies, risks, deliverables, and " +
			"timeline.",
		FakeFiles:             []string{"partner_portal_project.md"},
		Category:              "Planning",
		RecommendedSkillSlugs: []string{"anthropic-doc-coauthoring"},
	},
	{
		ID:      "compare-documents",
		Title:   "Compare documents",
		Summary: "Compare two versions and summarize important changes.",
		Prompt: "Compare policy_v1.docx and policy_v2.docx. List important changes, " +
			"risks, areas to validate, and produce an executive summary.",
		FakeFiles:             []string{"policy_v1.docx", "policy_v2.docx"},
		Category:              "Review",
		RecommendedSkillSlugs: []string{"anthropic-docx"},
	},
	{
		ID:      "generate-faq",
		Title:   "Generate an FAQ",
		Summary: "Turn demo support tickets into an internal knowledge base.",
		Prompt: "Using support_ticket_faq.csv, generate an internal FAQ with frequent " +
			"questions, short answers, categories, and escalation points.",
		FakeFiles:             []string{"support_ticket_faq.csv"},
		Category:              "Knowledge",
		RecommendedSkillSlugs: []string{"anthropic-doc-coauthoring"},
	},
}

// All returns the catalog in display order (featuredOnly keeps the ideas featured on the home page).
func All(featuredOnly bool) []Idea {
	out := make([]Idea, 0, len(catalog))
	for _, idea := range catalog {
		if !featuredOnly || idea.FeaturedOnHome {
			out = append(out, idea)
		}
	}
	return out
}

// Get returns the idea of a (trimmed) id.
func Get(id string) (Idea, bool) {
	id = strings.TrimSpace(id)
	for _, idea := range catalog {
		if idea.ID == id {
			return idea, true
		}
	}
	return Idea{}, false
}
