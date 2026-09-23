package desktop

import (
	"strings"
)

// Persona defines a departmental AI employee persona (inspired by Cura Labs & IrisGo).
type Persona struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Department     string   `json:"department"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Specialization string   `json:"specialization"`
	DefaultSkills  []string `json:"default_skills"`
	Channels       []string `json:"channels"`
	Icon           string   `json:"icon"`
}

// TeamTemplate defines a bundled multi-agent departmental team.
type TeamTemplate struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	PersonaIDs  []string `json:"persona_ids"`
}

// Catalog of departmental AI employee personas.
var defaultPersonas = []Persona{
	{
		ID:             "finance",
		Name:           "Finance Specialist",
		Department:     "Finance",
		Title:          "Senior Financial Operations Specialist",
		Description:    "Extracts and reconciles invoices, reviews expenses against policy limits, audits billing, and manages AP/AR records.",
		Specialization: "Finance and accounting operations: invoice processing, expense approvals, payment matching, and billing reconciliation",
		DefaultSkills:  []string{"invoice-processing", "expense-approval", "billing-reconciliation"},
		Channels:       []string{"email", "slack", "webhook"},
		Icon:           "receipt",
	},
	{
		ID:             "support",
		Name:           "Support Specialist",
		Department:     "Customer Support",
		Title:          "Customer Success & Support Specialist",
		Description:    "Triages inbound support inboxes, routes tickets to product queues, and drafts verified FAQ and documentation responses.",
		Specialization: "Customer support: email triage, ticket routing, FAQ replies, and customer inquiry resolution",
		DefaultSkills:  []string{"email-triage", "ticket-router", "customer-faq-reply"},
		Channels:       []string{"email", "slack", "telegram", "webhook"},
		Icon:           "headset",
	},
	{
		ID:             "sales",
		Name:           "Sales Development Specialist",
		Department:     "Sales",
		Title:          "Inbound Sales & Pipeline Specialist",
		Description:    "Scores inbound leads, researches target accounts before calls, drafts meeting follow-ups, and updates CRM deal stages.",
		Specialization: "Inbound sales operations: lead scoring, account research, follow-up drafting, and CRM pipeline synchronization",
		DefaultSkills:  []string{"lead-scoring", "customer-research", "follow-up-sender", "pipeline-update"},
		Channels:       []string{"email", "slack", "webhook"},
		Icon:           "trending-up",
	},
	{
		ID:             "executive_assistant",
		Name:           "Executive Assistant",
		Department:     "Operations",
		Title:          "Executive Operations & Productivity Assistant",
		Description:    "Produces executive morning briefs, formats meeting minutes, schedules calendars, and coordinates multi-step workflows.",
		Specialization: "Executive assistance: morning briefs, meeting minutes synthesis, calendar tracking, and recurring workflow automation",
		DefaultSkills:  []string{"morning-brief", "meeting-notes", "workflow-trigger"},
		Channels:       []string{"email", "slack", "telegram", "webhook"},
		Icon:           "calendar",
	},
	{
		ID:             "data",
		Name:           "Data Analyst",
		Department:     "Data",
		Title:          "Data & Business Intelligence Analyst",
		Description:    "Builds weekly KPI summary reports, synchronizes datastores, and extracts structured records from web sources.",
		Specialization: "Data analytics: KPI reporting, database delta synchronization, CSV manipulation, and web scraping",
		DefaultSkills:  []string{"report-builder", "data-sync", "web-scraper"},
		Channels:       []string{"slack", "webhook"},
		Icon:           "bar-chart-2",
	},
	{
		ID:             "devops",
		Name:           "DevOps Engineer",
		Department:     "Engineering",
		Title:          "Platform & Cloud Operations Engineer",
		Description:    "Monitors application canary rollouts, reviews code pull requests, correlates alarm clusters, and digests infrastructure alerts.",
		Specialization: "DevOps & cloud engineering: deployment monitoring, pull request review, error telemetry, and alert correlation",
		DefaultSkills:  []string{"deploy-monitor", "pr-review", "alert-digest"},
		Channels:       []string{"slack", "webhook"},
		Icon:           "terminal",
	},
	{
		ID:             "recruiting",
		Name:           "Recruiting Specialist",
		Department:     "Human Resources",
		Title:          "Talent & Onboarding Coordinator",
		Description:    "Coordinates new hire IT provisioning checklists, candidate screening summaries, and orientation materials.",
		Specialization: "Talent and HR: employee onboarding workflows, role provisioning, and candidate tracking",
		DefaultSkills:  []string{"onboarding-flow"},
		Channels:       []string{"email", "slack", "webhook"},
		Icon:           "user-check",
	},
	{
		ID:             "legal",
		Name:           "Compliance Officer",
		Department:     "Legal & Compliance",
		Title:          "Security & Compliance Analyst",
		Description:    "Audits access permissions, detects stale credentials, checks SOC2/GDPR controls, and drafts regulatory summaries.",
		Specialization: "Legal & compliance: SOC2/GDPR compliance checks, dormant account audits, and contract summaries",
		DefaultSkills:  []string{"compliance-check"},
		Channels:       []string{"email", "slack", "webhook"},
		Icon:           "shield",
	},
	{
		ID:             "marketing",
		Name:           "Marketing Specialist",
		Department:     "Marketing",
		Title:          "Product Marketing Coordinator",
		Description:    "Drafts product changelogs, release updates, and social copy across communication channels.",
		Specialization: "Product marketing: release changelog authoring, feature announcements, and social content drafts",
		DefaultSkills:  []string{"content-drafts"},
		Channels:       []string{"slack", "webhook"},
		Icon:           "send",
	},
}

// Catalog of multi-agent team templates.
var defaultTeams = []TeamTemplate{
	{
		ID:          "business-core",
		Name:        "Business Core Team",
		Description: "Comprehensive operational coverage for growing businesses: Finance, Support, Sales, and Executive Operations.",
		PersonaIDs:  []string{"finance", "support", "sales", "executive_assistant"},
	},
	{
		ID:          "revenue-ops",
		Name:        "Revenue Operations Team",
		Description: "Drives customer acquisition and revenue accounting: Inbound Sales, Data Analytics, and Financial Operations.",
		PersonaIDs:  []string{"sales", "data", "finance"},
	},
	{
		ID:          "engineering-ops",
		Name:        "Engineering & Platform Team",
		Description: "Keeps cloud services healthy and code high-quality: DevOps Platform Engineer and Data Analyst.",
		PersonaIDs:  []string{"devops", "data"},
	},
	{
		ID:          "executive-suite",
		Name:        "Executive Suite",
		Description: "Supports leadership with morning briefings, financial oversight, and compliance assurance.",
		PersonaIDs:  []string{"executive_assistant", "finance", "legal"},
	},
}

// AllPersonas returns all available departmental personas.
func AllPersonas() []Persona {
	out := make([]Persona, len(defaultPersonas))
	copy(out, defaultPersonas)
	return out
}

// GetPersona returns a persona by its identifier.
func GetPersona(id string) (Persona, bool) {
	norm := strings.ToLower(strings.TrimSpace(id))
	for _, p := range defaultPersonas {
		if strings.ToLower(p.ID) == norm {
			return p, true
		}
	}
	return Persona{}, false
}

// AllTeams returns all available multi-agent team templates.
func AllTeams() []TeamTemplate {
	out := make([]TeamTemplate, len(defaultTeams))
	copy(out, defaultTeams)
	return out
}

// GetTeam returns a team template by its identifier.
func GetTeam(id string) (TeamTemplate, bool) {
	norm := strings.ToLower(strings.TrimSpace(id))
	for _, t := range defaultTeams {
		if strings.ToLower(t.ID) == norm {
			return t, true
		}
	}
	return TeamTemplate{}, false
}

// MatchPersonaForInbound maps an incoming message (from email, Slack, webhook, etc.)
// to the most relevant department persona based on content intent.
func MatchPersonaForInbound(channel, sender, text string) (Persona, bool) {
	lower := strings.ToLower(text + " " + sender + " " + channel)

	type matchScore struct {
		persona  Persona
		keywords []string
	}

	scores := []matchScore{
		{
			persona:  defaultPersonas[0], // finance
			keywords: []string{"invoice", "receipt", "billing", "expense", "payment", "stripe", "bank", "payout", "refund"},
		},
		{
			persona:  defaultPersonas[1], // support
			keywords: []string{"help", "support", "issue", "ticket", "bug", "error", "broken", "cannot login", "faq", "customer"},
		},
		{
			persona:  defaultPersonas[2], // sales
			keywords: []string{"demo", "pricing", "lead", "quote", "proposal", "contract value", "call", "acv", "prospect"},
		},
		{
			persona:  defaultPersonas[5], // devops
			keywords: []string{"deploy", "canary", "pr", "pull request", "build failed", "5xx", "outage", "cluster", "alert"},
		},
		{
			persona:  defaultPersonas[6], // recruiting
			keywords: []string{"onboarding", "new hire", "candidate", "resume", "interview", "offer letter"},
		},
		{
			persona:  defaultPersonas[7], // legal
			keywords: []string{"compliance", "soc2", "gdpr", "audit", "dormant", "nda", "terms"},
		},
		{
			persona:  defaultPersonas[4], // data
			keywords: []string{"report", "kpi", "scrape", "sync", "metrics", "analytics", "sql"},
		},
		{
			persona:  defaultPersonas[8], // marketing
			keywords: []string{"changelog", "announcement", "social", "newsletter", "blog"},
		},
		{
			persona:  defaultPersonas[3], // executive assistant
			keywords: []string{"morning brief", "agenda", "meeting notes", "schedule", "calendar"},
		},
	}

	var bestPersona Persona
	bestCount := 0

	for _, sc := range scores {
		count := 0
		for _, kw := range sc.keywords {
			if strings.Contains(lower, kw) {
				count++
			}
		}
		if count > bestCount {
			bestCount = count
			bestPersona = sc.persona
		}
	}

	if bestCount > 0 {
		return bestPersona, true
	}

	// Default fallback to Executive Assistant or Support depending on channel
	if strings.Contains(channel, "slack") || strings.Contains(channel, "email") {
		return defaultPersonas[1], true // Support
	}

	return defaultPersonas[3], true // Executive Assistant
}
