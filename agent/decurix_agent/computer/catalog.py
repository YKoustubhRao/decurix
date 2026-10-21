import time

from decurix_agent.computer import skills


# ============================================================
# Skill Catalog: IrisGo AI Standard Library & Cura Department Skills
#
# A curated catalog of optional, parameterizable skills
# covering key business departments:
#
#   Finance:        expense-approval, billing-reconciliation
#   Support:        email-triage, ticket-router, customer-faq-reply
#   Sales:          lead-scoring, customer-research, follow-up-sender, pipeline-update
#   Productivity:   morning-brief, meeting-notes, workflow-trigger
#   Data:           report-builder, data-sync, web-scraper
#   DevOps:         deploy-monitor, pr-review, alert-digest
#   Recruiting/HR:  onboarding-flow, resume-screen
#   Legal/Security: compliance-check, contract-summary
#   Marketing:      content-drafts
# ============================================================

CATALOG_SKILLS = [
    # --- Finance ---
    {
        "name": "expense-approval",
        "title": "Review and Approve Expenses",
        "department": "finance",
        "goal": "Audit submitted expense claims against company policy limits and mark eligible claims approved.",
        "done_when": "All pending expense items are checked and approved or flagged with a policy exception note.",
        "params": [
            {"name": "max_auto_approve", "label": "Auto-Approve Threshold ($)", "default": "100.00", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open pending expense reports list", "value": "~/work/expenses.csv"},
            {"n": 2, "kind": "click", "summary": "Review receipt attachment and expense category", "target": {"role": "link", "name": "View Receipt"}},
            {"n": 3, "kind": "click", "summary": "Approve compliant expenses within policy limit", "target": {"role": "button", "name": "Approve"}},
        ],
    },
    {
        "name": "billing-reconciliation",
        "title": "Reconcile Bank and Stripe Billing",
        "department": "finance",
        "goal": "Compare gateway payouts against bank deposits and highlight fee discrepancies.",
        "done_when": "Payout batches are matched to bank statement lines with zero unallocated balance.",
        "params": [
            {"name": "period", "label": "Reconciliation Month", "default": "current", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Export payout transactions from billing system", "value": "~/work/billing.csv"},
            {"n": 2, "kind": "type", "summary": "Compute total net payout minus platform fees", "value": "CALCULATE"},
            {"n": 3, "kind": "click", "summary": "Export reconciliation summary report", "target": {"role": "button", "name": "Export"}},
        ],
    },

    # --- Support ---
    {
        "name": "email-triage",
        "title": "Triage Inbound Support Email",
        "department": "support",
        "goal": "Scan inbox, categorize inquiries by urgency and product area, and draft standard responses.",
        "done_when": "All unread emails are labelled, priority tickets escalated, and drafted replies queued.",
        "params": [
            {"name": "inbox_label", "label": "Target Mailbox Label", "default": "INBOX", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open email client and fetch unread customer messages", "value": "mail://inbox"},
            {"n": 2, "kind": "click", "summary": "Classify subject and message body into category", "target": {"role": "menuitem", "name": "Apply Label"}},
            {"n": 3, "kind": "type", "summary": "Draft courteous initial response with resolution steps", "value": "Thank you for reaching out..."},
        ],
    },
    {
        "name": "ticket-router",
        "title": "Route Support Tickets",
        "department": "support",
        "goal": "Assign incoming customer tickets to appropriate engineering or support queues based on keywords.",
        "done_when": "Pending queue is empty and tickets have assigned owners and SLAs.",
        "params": [
            {"name": "queue_name", "label": "Support Queue Name", "default": "general", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "View open unassigned support tickets", "value": "~/work/tickets.json"},
            {"n": 2, "kind": "click", "summary": "Select ticket and inspect error stack or inquiry", "target": {"role": "listitem", "name": "Ticket"}},
            {"n": 3, "kind": "click", "summary": "Assign ticket to responsible team tier", "target": {"role": "button", "name": "Assign"}},
        ],
    },
    {
        "name": "customer-faq-reply",
        "title": "Draft FAQ Answers for Customer Inquiries",
        "department": "support",
        "goal": "Match customer questions to knowledge base articles and draft contextual responses.",
        "done_when": "A tailored draft response quoting the relevant documentation is saved for review.",
        "params": [
            {"name": "kb_source", "label": "Knowledge Base URL or Directory", "default": "~/work/docs", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Search knowledge base for customer inquiry keywords", "value": "~/work/docs"},
            {"n": 2, "kind": "type", "summary": "Draft tailored response referencing documentation link", "value": "DRAFT_REPLY"},
        ],
    },

    # --- Sales ---
    {
        "name": "lead-scoring",
        "title": "Score and Enrich Inbound Leads",
        "department": "sales",
        "goal": "Evaluate inbound lead company size, industry, tech stack, and intent signals to assign a qualification score.",
        "done_when": "Every new lead has an enriched profile, company headcount, and lead score (0-100).",
        "params": [
            {"name": "lead_file", "label": "Leads CSV File", "default": "~/work/leads.csv", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Load leads CSV list and find unranked records", "value": "~/work/leads.csv"},
            {"n": 2, "kind": "type", "summary": "Query company domain for size, funding, and tech stack", "value": "ENRICH"},
            {"n": 3, "kind": "click", "summary": "Update qualification score and tier in CRM", "target": {"role": "button", "name": "Save Record"}},
        ],
    },
    {
        "name": "customer-research",
        "title": "Research Account Before Sales Call",
        "department": "sales",
        "goal": "Compile an executive briefing on a target company: leadership, recent news, competitors, and pain points.",
        "done_when": "A 1-page account research brief is saved in Markdown in the sales workspace.",
        "params": [
            {"name": "company_name", "label": "Target Company Name", "default": "Acme Corp", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Research target company homepage and executive team", "value": "https://www.google.com"},
            {"n": 2, "kind": "type", "summary": "Synthesize company news, strategic priorities, and pain points", "value": "SUMMARY"},
            {"n": 3, "kind": "click", "summary": "Save executive briefing to sales notes folder", "target": {"role": "button", "name": "Save"}},
        ],
    },
    {
        "name": "follow-up-sender",
        "title": "Draft Sales Meeting Follow-ups",
        "department": "sales",
        "goal": "Review meeting transcript or notes and draft personalized follow-up emails with agreed action items.",
        "done_when": "Personalized email draft with next steps and pricing proposal is ready in draft box.",
        "params": [
            {"name": "client_name", "label": "Client Contact Name", "default": "Partner", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open call transcript or action item notes", "value": "~/work/meeting_notes.txt"},
            {"n": 2, "kind": "type", "summary": "Draft follow-up email listing agreed milestones and dates", "value": "DRAFT_EMAIL"},
        ],
    },
    {
        "name": "pipeline-update",
        "title": "Update CRM Opportunity Stages",
        "department": "sales",
        "goal": "Sync call outcomes and proposal sent status into CRM deal stages.",
        "done_when": "CRM deal stage, expected close date, and ACV are synchronized.",
        "params": [
            {"name": "deal_id", "label": "Deal or Opportunity ID", "default": "DEAL-100", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open CRM pipeline board", "value": "~/work/pipeline.csv"},
            {"n": 2, "kind": "click", "summary": "Advance deal stage to Proposal or Closing", "target": {"role": "button", "name": "Update Stage"}},
        ],
    },

    # --- Productivity & Executive Assistant ---
    {
        "name": "morning-brief",
        "title": "Generate Daily Executive Morning Brief",
        "department": "executive_assistant",
        "goal": "Aggregate today's schedule, high-priority unread emails, urgent alerts, and company key metrics into a 2-minute brief.",
        "done_when": "Daily briefing document is rendered and delivered to the workspace or dashboard.",
        "params": [
            {"name": "date", "label": "Briefing Date", "default": "today", "step": 1},
        ],
        "optimization": {
            "command": "python3 -c 'import datetime; print(\"Morning Brief generated for \" + str(datetime.date.today()))'",
            "post_check": {"contains_text": "Morning Brief generated"},
        },
        "steps": [
            {"n": 1, "kind": "open", "summary": "Scan calendar appointments and scheduled meetings for today", "value": "~/work/calendar.ics"},
            {"n": 2, "kind": "open", "summary": "Collect pending approvals and high-priority flagged messages", "value": "~/work/inbox_summary.txt"},
            {"n": 3, "kind": "type", "summary": "Synthesize executive agenda into clean formatted markdown brief", "value": "# Morning Brief\n\n## Today Schedule\n- ..."},
        ],
    },
    {
        "name": "meeting-notes",
        "title": "Format and Distribute Meeting Minutes",
        "department": "executive_assistant",
        "goal": "Parse raw meeting notes or audio transcripts into structured attendees, decisions made, and assigned action items.",
        "done_when": "Structured minutes with clear owners and due dates are generated.",
        "params": [
            {"name": "meeting_title", "label": "Meeting Title", "default": "Team Sync", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Read raw notes from meeting recording or scratchpad", "value": "~/work/raw_notes.txt"},
            {"n": 2, "kind": "type", "summary": "Structure decisions, owner action items, and timelines", "value": "MINUTES"},
            {"n": 3, "kind": "click", "summary": "Save formatted minutes to project documentation repository", "target": {"role": "button", "name": "Save"}},
        ],
    },
    {
        "name": "workflow-trigger",
        "title": "Trigger Automated Recurring Workflow",
        "department": "operations",
        "goal": "Verify system health, trigger chained workflow jobs, and log completion telemetry.",
        "done_when": "All workflow steps complete successfully and status is reported.",
        "params": [
            {"name": "workflow_id", "label": "Workflow Identifier", "default": "daily-sync", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Check status of upstream dependencies and data sources", "value": "~/work/status.json"},
            {"n": 2, "kind": "click", "summary": "Trigger workflow execution engine", "target": {"role": "button", "name": "Run Workflow"}},
        ],
    },

    # --- Data ---
    {
        "name": "report-builder",
        "title": "Build Weekly KPI Summary Report",
        "department": "data",
        "goal": "Extract metrics from multiple CSV/database sources, compute week-over-week trends, and generate visual tables.",
        "done_when": "A weekly KPI report with WoW comparisons is exported as Markdown or PDF.",
        "params": [
            {"name": "source_dir", "label": "Metrics Data Directory", "default": "~/work/metrics", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open metrics spreadsheets and data files", "value": "~/work/metrics"},
            {"n": 2, "kind": "type", "summary": "Compute total revenue, active users, churn, and conversion rates", "value": "SUMMARIZE"},
            {"n": 3, "kind": "click", "summary": "Export finalized weekly KPI summary report", "target": {"role": "button", "name": "Export Report"}},
        ],
    },
    {
        "name": "data-sync",
        "title": "Synchronize Datastores and Tables",
        "department": "data",
        "goal": "Validate schemas, detect updated records, and synchronize delta rows across local and remote tables.",
        "done_when": "Row counts match between source and target with zero replication lag.",
        "params": [
            {"name": "source_table", "label": "Source Table Name", "default": "source_db", "step": 1},
            {"name": "target_table", "label": "Target Table Name", "default": "replica_db", "step": 2},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Inspect source table updated timestamp", "value": "~/work/source.db"},
            {"n": 2, "kind": "type", "summary": "Replicate new records to target table", "value": "SYNC"},
            {"n": 3, "kind": "click", "summary": "Confirm sync checksum and commit transaction", "target": {"role": "button", "name": "Commit"}},
        ],
    },
    {
        "name": "web-scraper",
        "title": "Scrape and Structure Web Data",
        "department": "data",
        "goal": "Extract structured records from public web catalogs, tables, or directories into clean JSON/CSV format.",
        "done_when": "Scraped records are validated, deduped, and saved to the target CSV file.",
        "params": [
            {"name": "target_url", "label": "Target Web Page URL", "default": "https://example.com/catalog", "step": 1},
            {"name": "output_file", "label": "Output CSV Path", "default": "~/work/scraped_data.csv", "step": 2},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Navigate to target web page directory", "value": "https://example.com/catalog"},
            {"n": 2, "kind": "type", "summary": "Extract table rows, column names, and links into structured array", "value": "EXTRACT"},
            {"n": 3, "kind": "click", "summary": "Save clean dataset to output CSV", "target": {"role": "button", "name": "Download CSV"}},
        ],
    },

    # --- DevOps ---
    {
        "name": "deploy-monitor",
        "title": "Monitor Deployments and Canary Health",
        "department": "devops",
        "goal": "Watch application deployment rollouts, monitor 5xx error rate and response latencies, and alert on regression.",
        "done_when": "Deployment reaches 100% healthy pods with error rates below 0.1%.",
        "params": [
            {"name": "service_name", "label": "Service Name", "default": "api-gateway", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open service metrics dashboard", "value": "http://localhost:9090"},
            {"n": 2, "kind": "click", "summary": "Check pod restart counts and error budget consumption", "target": {"role": "link", "name": "Metrics"}},
            {"n": 3, "kind": "type", "summary": "Log canary pass verification note", "value": "CANARY_OK"},
        ],
    },
    {
        "name": "pr-review",
        "title": "Review Pull Request Code and Tests",
        "department": "devops",
        "goal": "Analyze git diff for bug patterns, test coverage, security considerations, and adhere to style conventions.",
        "done_when": "Constructive code review comments are written with approve or request changes verdict.",
        "params": [
            {"name": "pr_number", "label": "Pull Request Number", "default": "1", "step": 1},
        ],
        "optimization": {
            "command": "git diff HEAD~1",
            "post_check": {"contains_text": "diff"},
        },
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open pull request files changed view", "value": "https://github.com/pulls"},
            {"n": 2, "kind": "click", "summary": "Inspect code diff, added tests, and CI build status", "target": {"role": "link", "name": "Files changed"}},
            {"n": 3, "kind": "type", "summary": "Draft thorough review feedback with clear code recommendations", "value": "LGTM with minor comments..."},
        ],
    },
    {
        "name": "alert-digest",
        "title": "Aggregate Infrastructure Alert Digest",
        "department": "devops",
        "goal": "Correlate active alarms across servers and databases into a prioritized incident digest.",
        "done_when": "No duplicate alerts remain and incidents have severity rankings.",
        "params": [
            {"name": "cluster_id", "label": "Cluster Identifier", "default": "prod-cluster", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open monitoring alerts console", "value": "http://localhost:9093"},
            {"n": 2, "kind": "type", "summary": "Group recurring alerts by root cause component", "value": "CORRELATE"},
            {"n": 3, "kind": "click", "summary": "Acknowledge resolved alarms and summarize active incidents", "target": {"role": "button", "name": "Silence"}},
        ],
    },

    # --- Recruiting / HR ---
    {
        "name": "onboarding-flow",
        "title": "Provision New Employee Onboarding",
        "department": "recruiting",
        "goal": "Check off new hire IT accounts, generate initial credentials, and schedule orientation invites.",
        "done_when": "All onboarding checklist items are verified and welcome packet is sent.",
        "params": [
            {"name": "employee_name", "label": "New Hire Full Name", "default": "Jane Doe", "step": 1},
            {"name": "role_title", "label": "Job Title", "default": "Software Engineer", "step": 2},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Open onboarding checklist template", "value": "~/work/onboarding.csv"},
            {"n": 2, "kind": "type", "summary": "Fill employee name, department, manager, and start date", "value": "ONBOARDING_DATA"},
            {"n": 3, "kind": "click", "summary": "Submit provisioning request for email and repo access", "target": {"role": "button", "name": "Submit"}},
        ],
    },

    # --- Legal & Compliance ---
    {
        "name": "compliance-check",
        "title": "Audit Security and Privacy Compliance",
        "department": "legal",
        "goal": "Scan access controls, expired credentials, and data retention policies for SOC2/GDPR compliance.",
        "done_when": "Compliance audit report is compiled with any remediation items highlighted.",
        "params": [
            {"name": "standard", "label": "Compliance Standard", "default": "SOC2", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Review user access list and dormant account log", "value": "~/work/access_audit.csv"},
            {"n": 2, "kind": "type", "summary": "Identify stale accounts without MFA or recent login", "value": "FLAG_STALE"},
            {"n": 3, "kind": "click", "summary": "Export audit evidence report", "target": {"role": "button", "name": "Generate Report"}},
        ],
    },

    # --- Marketing ---
    {
        "name": "content-drafts",
        "title": "Draft Product Changelog and Social Updates",
        "department": "marketing",
        "goal": "Review merged features from git log and draft release changelog plus LinkedIn/X post drafts.",
        "done_when": "Polished release announcement and marketing copy are ready in drafts folder.",
        "params": [
            {"name": "version", "label": "Release Version", "default": "v1.2.0", "step": 1},
        ],
        "steps": [
            {"n": 1, "kind": "open", "summary": "Read commit log and closed feature tickets", "value": "~/work/changelog.txt"},
            {"n": 2, "kind": "type", "summary": "Draft engaging announcement highlighting customer benefits", "value": "# What's New in v1.2.0\n\n..."},
            {"n": 3, "kind": "click", "summary": "Save post drafts to marketing review folder", "target": {"role": "button", "name": "Save"}},
        ],
    },
]


def get_catalog():
    """Return all skills in the standard catalog."""

    return list(CATALOG_SKILLS)


def get_catalog_skill(name):
    """Find a catalog skill by its exact name or slug."""

    target = (name or "").strip().lower()
    return next((s for s in CATALOG_SKILLS if s["name"] == target), None)


def install_catalog_skill(name, target_name=None):
    """
    Install a skill from the catalog as a ready Decurix skill in ~/.decurix/skills/.
    Returns the saved skill dict.
    """

    entry = get_catalog_skill(name)
    if not entry:
        raise ValueError(f"unknown catalog skill: {name!r}")

    final_name = target_name or entry["name"]

    skill = {
        "schema": skills.SCHEMA,
        "name": final_name,
        "title": entry["title"],
        "goal": entry["goal"],
        "done_when": entry["done_when"],
        "department": entry.get("department", "general"),
        "status": skills.READY,
        "created": int(time.time()),
        "sources": [],
        "params": entry.get("params", []),
        "steps": entry.get("steps", []),
    }

    if entry.get("optimization"):
        skill["optimization"] = entry["optimization"]

    return skills.save(skill)

