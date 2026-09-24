# Moat frameworks for AI application and agent companies (2024 to September 2026)

Scope: which moats hold for AI apps and agents, based on companies at about $1B+ valuation or $100M+ ARR, and how they bear on Decurix/Flowera. Flowera is a self-hosted VM per AI worker, with record, learn, replay and heal, approval gates, an audit trail and an autonomy metric.

Source-quality note: many 2026 ARR figures come from aggregators (Sacra, valueaddvc.com/"Value Add VC" by Trace Cohen, GetLatka, ARR Club). These are estimates, not company disclosures, and they sometimes disagree with each other. Where a primary source (company blog, SEC filing, Reuters/Bloomberg/TechCrunch) exists it is cited first. "ARR" in AI is often an annualized run rate of usage, not contracted recurring revenue (see Q2). Treat every ARR figure below as "reported" unless marked otherwise.

---

## Q1. Case studies: what the moat actually is, ARR/valuation, and how it was built

### Takeaway
Across the $1B+ AI-app winners, the moat that actually holds is rarely the model or the prompt. It is some mix of five things:
- deep deployment into one buyer's workflow, often with forward-deployed or embedded engineers (Harvey, Sierra, Palantir);
- a clear, billable unit of outcome (Sierra, Crosby, EvenUp);
- a cornered supply side or neutrality (Surge, Mercor);
- distribution acquired or bought (Cognition buying Windsurf);
- trust/sovereignty positioning (Palantir, Legora's EU residency).

Several "moats" have been visibly eroded in 2026:
- Harvey's proprietary model lost to frontier models on its own benchmark.
- Anthropic's Claude plugins absorbed legal integrations.
- Cursor and Windsurf were sold rather than staying independent.

### Cited Findings

**Harvey (legal)**
- $550M raise at $15.5B valuation, co-led by Diffusion and Lightspeed (Sep 9, 2026). The raise followed "Harvey's first post-trained open-weight model" and the launch of Harvey LAB (Legal Agent Benchmark). Positioning: helping legal teams "build and own their intelligence." — [Harvey blog](https://www.harvey.ai/blog/harvey-raises-dollar550m-at-a-dollar155b-valuation-to-help-legal-teams-own-their-intelligence); [Reuters](https://www.reuters.com/legal/government/legal-ai-startup-harvey-reaches-155-billion-valuation-new-funding-round-2026-09-09/); Bloomberg says $15.6B ([Bloomberg](https://www.bloomberg.com/news/articles/2026-09-09/legal-ai-startup-harvey-hits-15-6-billion-value-with-550-million-round)).
- ARR "passed $400mn", with 3,000+ organisations (up from 1,300 in March). That implies about 39x ARR. Harvey also acquired Guardrails AI (agent security). — [The Next Web, Sep 9 2026](https://thenextweb.com/news/harvey-550m-round-15-6bn-guardrails-acquisition)
- The March 2026 round was $200M at $11B. It was explicitly used to "expand the agents customers run on Harvey and grow the embedded legal engineering teams supporting them." — [Harvey blog, Mar 25 2026](https://www.harvey.ai/blog/harvey-raises-at-dollar11-billion-valuation-to-scale-agents-across-law-firms-and-enterprises)
- Valuation path: $8B (Dec 2025) → $11B (Mar 2026) → $15.5B (Sep 2026). At least eight priced rounds since 2023, and $1.55B+ raised. — [TechCrunch](https://techcrunch.com/2026/09/09/harvey-hits-15-5b-valuation-months-after-reaching-11b/)
- **Moat erosion:**
  - Harvey built a custom legal LLM, tested it against frontier models on its own BigLaw Bench, and the frontier models won. Seven frontier models now outperform the original Harvey system, and Harvey went multi-model.
  - Anthropic launched 20+ legal connectors (including Harvey itself) and 12 practice-area plugins. The analysis argues these absorb "the integration moat."
  - Harvey reportedly runs 25,000 custom workflows and 400,000+ agentic queries a day. The argument is that this creates "a retention moat, not a growth moat."
  - Source: [LegalRealist AI, May 2026](https://legalrealist.ai/posts/26-harvey-v-legora/) (independent analysis, opinionated).
- A VC teardown calls Harvey and Legora "near-identical" "thin LLM wrappers" in "a distribution race, not a technology battle", and likens the dynamic to Uber vs. Lyft. — [Altis report, Mar 2026](https://www.altis.vc/reports/harvey-legora)
- Law firms now contract with both Harvey and Legora and "hot-swap" seats between lawyers. Legora's average ACV compressed from about $280K to about $100K under discounting. Sacra puts Harvey at $300M ARR (May 2026). — [Sacra, Jun 2026](https://sacra.com/research/100m-year-harvey-for-the-rest-of-the-world/)
- Harvey's token consumption reportedly grew from about 1T/month (Jan 2026) to 12–13T/month (May 2026), with 25,000+ custom agents deployed. — [TechTimes, Aug 2026](https://www.techtimes.com/articles/324348/20260813/legal-ai-arms-race-legora-eyes-10b-harvey-nears-155b-one-month.htm)

**Legora (legal challenger)**
- $550M Series D at $5.55B (Mar 2026). Reportedly in talks at $10B+ (Aug 2026). ARR went from $3M to $100M in about 18 months (April 2026).
- It differentiates on EU data residency via Azure ("a procurement gate") and model-agnosticism.
- Source: [TechTimes](https://www.techtimes.com/articles/324348/20260813/legal-ai-arms-race-legora-eyes-10b-harvey-nears-155b-one-month.htm); [Business Insider, Mar 2026](https://www.businessinsider.com/legora-founder-harvey-legal-tech-ai-platform-competition-2026-3)

**Sierra (customer-service agents)**
- $950M Series E at $15.8B post-money, led by Tiger Global and GV (May 4, 2026). $150M ARR in eight quarters. — [CNBC](https://www.cnbc.com/2026/05/04/bret-taylor-sierra-fundraise-openai.html)
- Sacra estimates $200M ARR in May 2026, up from about $130M at end-2025 and $26M at end-2024. Revenue comes from usage- and outcome-based contracts, with "high-touch implementation and ongoing optimization services bundled into multi-year enterprise agreements." — [Sacra](https://sacra.com/c/sierra/)
- Bret Taylor on outcome pricing: "if the AI agent resolves the case, no human intervention, there's a pre-negotiated rate… If we do have to escalate to a person, that's free." He argues token usage is not correlated with value. — [Stripe interview, Mar 2026](https://www.youtube.com/watch?v=n4E4xNYCkYM)
- About $1.50 per resolution (vs Intercom Fin $0.99 plus seats), custom contracts from about $150K/yr, 40%+ of the Fortune 50 (aggregator). — [valueaddvc, Jul 2026](https://valueaddvc.com/blog/how-does-sierra-ai-make-money-outcome-based-pricing-enterprise-agents-and-the-business-model-breakdown)

**Decagon (customer-service agents)**
- $250M Series D at $4.5B (Jan 2026), then a tender at $4.5B (Mar 2026). It had not disclosed revenue since late 2024. — [TechCrunch](https://techcrunch.com/2026/03/04/decagon-completes-first-tender-offer-at-4-5b-valuation/); [Forbes](https://www.forbes.com/sites/alexyork/2026/02/06/ai-agent-startup-decagon-triples-valuation-to-45-billion/)
- Sacra estimates $100M annualized revenue in July 2026, up from $44M at end-2025. — [Sacra](https://sacra.com/c/decagon/)
- Reportedly deliberately avoids the forward-deployed-engineer model Sierra uses (per Newcomer, via aggregator). NRR/churn is undisclosed. — [valueaddvc pulse](https://valueaddvc.com/pulse/decagon-100m-arr-ai-customer-agents-2026)

**Glean (enterprise search/agents)**
- $150M Series F at $7.2B (Jun 2025), with $100M+ ARR at the time. Glean calls itself "the system of context for organizational intelligence." — [Glean](https://www.glean.com/blog/glean-series-f-announcement)
- About $200M ARR roughly nine months later, via a "permissions-aware knowledge graph" and model neutrality (15+ LLMs). The main risk is Microsoft 365 Copilot bundling. — [Futurum](https://futurumgroup.com/insights/glean-doubles-arr-to-200m-can-its-knowledge-graph-beat-copilot/)
- About $300M ARR by May 2026 (aggregator estimate), partly consumption-based. — [valueaddvc](https://valueaddvc.com/blog/glean-valuation-revenue-2026-300m-arr-enterprise-ai-search)

**Abridge (clinical documentation)**
- $5.3B valuation, set in Jun 2025 and reaffirmed by a $316M Series E extension in Apr 2026. $100M+ ARR. 250+ health systems, including all 40 Kaiser hospitals. Best in KLAS for ambient AI (Feb 2026). — [valueaddvc, Aug 2026](https://valueaddvc.com/blog/abridge-valuation-2026-5-3b-100m-arr-and-how-the-ai-scribe-beat-nuance-and-ambience) (aggregator; Kaiser and KLAS are verifiable facts)

**EvenUp (personal-injury law)**
- $150M Series E at $2B+ (Oct 2025), with ARR doubling YoY.
- Its "Piai" model is trained on "hundreds of thousands of injury cases." It has helped resolve 200,000+ cases for $10B+ in damages. 2,000+ firms use it.
- It uses human expert review (100+ legal/medical experts). It claims its demands are "69% more likely to achieve policy limit settlements."
- Its largest customer pays $4M+/yr.
- Sources: [EvenUp](https://www.evenuplaw.com/blog/evenup-2b-valuation/); [Fortune](https://fortune.com/2025/10/07/exclusive-evenup-raises-150-million-series-e-at-2-billion-valuation-as-ai-reshapes-personal-injury-law/); [EvenUp on human review](https://www.evenuplaw.com/blog/leveraging-ai-human-review-in-demand-letters-the-evenup-difference/)
- ARR Club lists $100M ARR (Jul 2026). — [ARR Club](https://www.arr.club/evenup)
- This is the clearest example here of an outcome-labelled data flywheel: case inputs are linked to settlement outcomes.

**Hebbia (finance)**
- Total funding about $161M (Series B $130M, Jul 2024). Used by "over 40% of the largest asset managers by AUM." — [LinkedIn company page](https://linkedin.com/company/hebbia)
- A Business Insider headline (Aug 2026) says its "existential challenge is doing it again." GetLatka lists about $24.6M ARR for 2025 (low reliability).
- Hebbia is a cautionary case: an early lead in a horizontal document-agent product without a clear lock-in.

**Cursor / Anysphere (coding)**
- $2.3B Series D at $29.3B (late 2025). $1B → $2B ARR (Feb 2026) → about $4B (Jun 2026), with about $2.6B from enterprise.
- SpaceX agreed on Jun 16, 2026 to acquire it for $60B all-stock. The deal closed Aug 14, 2026.
- Cursor's margin engine is reselling inference at a markup, plus its in-house "Composer" model credits.
- Sources: [valueaddvc](https://valueaddvc.com/blog/how-does-cursor-make-money-subscriptions-token-pricing-and-the-business-model-breakdown); [valueaddvc on the close](https://valueaddvc.com/blog/spacex-closes-60-billion-cursor-acquisition-the-largest-startup-exit-ever); deal effectiveness per a regulatory filing cited by [GCN](https://gcn.com/cognition-ai-47-billion-funding-round/21603/). These are aggregators; I did not fetch a primary SpaceX/Anysphere filing.
- Claude Code reached more than $2.5B run-rate after launching in May 2025. This is direct competition from the model supplier. — [New Market Pitch](https://newmarketpitch.com/blogs/news/ai-code-assistant-cursor-overvalued)

**Cognition (Devin + Windsurf)**
- Run-rate grew from $492M (May 2026) to about $900M (Sep 2026). It raised at about $47–48B, after $26B in May 2026 and $10.2B in Sep 2025.
- The ARR jump came largely from buying Windsurf (July 2025, after Google's $2.4B license-and-hire deal).
- Critic's line: "53x run-rate revenue for a category where switching cost is basically a config file."
- Sources: [GCN/Bloomberg-sourced](https://gcn.com/cognition-ai-47-billion-funding-round/21603/); [valueaddvc pulse](https://valueaddvc.com/pulse/cognition-devin-48b-valuation-2026)

**Palantir (AIP + FDEs)**
- Q2 2026: revenue $1.94B (+93% Y/Y). US commercial revenue $764M (+149% Y/Y). US commercial TCV $2.13B (+153%). US commercial customers 653 (+35%). FY26 guidance $8.15–8.158B. — [Palantir Q2 2026 press release (SEC)](https://www.sec.gov/Archives/edgar/data/1321655/000132165526000039/a2026q2ex991pressrelease.htm); [Q2 business update](https://investors.palantir.com/files/Palantir%20-%20Q2%202026%20Business%20Update.pdf)
- 2026 positioning is **sovereignty**. Customers' "competitive advantage should never become the training data for future models." AIP "keeps that alpha sovereign." Palantir is moving into post-training (SFT and RL) on open models for customers.
- Palantir says: "only Palantir has FDEs. Everyone else has sparkling sales engineers." It cites a bake-off where a frontier lab's deployment team failed and Palantir won a $10M ACV contract. — [Q2 2026 call transcript](https://transcripts.platformaeronaut.com/transcripts/PLTR-2Q26-transcript)
- The stack it credits: "data integration… ontology and actions, security and audit, workflows… agent orchestration with telemetry and observability, evals and customer-specific benchmarks." — [same](https://bullfincher.io/companies/palantir-technologies/earnings-call/fy2026-q2)

**Data vendors (Scale / Surge / Mercor / Handshake)**
- Surge AI earned over $1B revenue in 2024 while bootstrapped, and sought a raise at $15B+. — [Reuters, Jul 2025](https://www.reuters.com/business/scale-ais-bigger-rival-surge-ai-seeks-up-1-billion-capital-raise-sources-say-2025-07-01/)
- Mercor: $2B+ *gross* annualized revenue (Jun 2026). 60–70% is paid to contractors, so net is about $600–800M. It was in talks at about $20B after $10B in Oct 2025. — [Dealroom](https://dealroom.co/news/137121-mercor-doubles-to-2b-gross-revenue-run-rate-as-ai-labs-buy-expert-data/); [TNW](https://thenextweb.com/news/mercor-20-billion-valuation-deeptune-acquisition)
- **Neutrality as the moat:** after Meta bought 49% of Scale for $14.3B (Jun 2025), Google, OpenAI and xAI cut or reduced work with Scale. That volume flowed to Surge and Mercor. — [GetLatka summarizing Reuters](https://getlatka.com/blog/surgeai-revenue-valuation); [Pebblous](https://blog.pebblous.ai/blog/ai-data-supply-oligopoly/en/)
- Four firms (Scale, Surge, Mercor, Handshake) hold more than 75% of an estimated $8.5B market. The asset is the expert network ("the barrier to entry was their moat"). OpenAI is reportedly building in-house data teams to reduce reliance. — [Pebblous, citing Deedy Das's market map](https://blog.pebblous.ai/blog/ai-data-supply-oligopoly/en/)

**AI-native services / roll-ups**
- General Catalyst's Creation strategy: $1.5B fund (Fund XII, up from $800M). Portfolio includes Eudia (bought Johnson Hana, 300+ person legal delivery team), Crescendo, Long Lake, Titan MSP, Dwelly, Integral and Accrual.
- Targets: take single-digit-growth services firms to 10–20% growth and "double profit margins, often targeting 30–40%" ("Rule of 60").
- "Automation metrics lead. Financial results follow": the first KPIs are "tasks processed without human intervention."
- Sources: [GC, Future of Services](https://www.generalcatalyst.com/stories/the-future-of-services); [GC Europe, Apr 2026](https://www.generalcatalyst.com/stories/europes-ai-transformation-in-services)
- GC's portfolio claims EBITDA margins doubled within 12 months, "some hitting $100M in EBITDA in <2 years" (podcast claim, unaudited). — [Sourcery](https://www.sourcery.vc/p/breaking-inside-general-catalysts)
- GC is now applying this to scaled companies too (the AMEX GBT deal via Long Lake; Janus Henderson take-private via Percepta). — [PitchBook, May 2026](https://pitchbook.com/news/articles/general-catalysts-6-3b-amex-deal-puts-its-ai-roll-up-strategy-on-display)
- Crosby (AI-native "neofirm"): $400M valuation, $60M Series B (Apr 2026), about $400 flat fee per contract, 58-minute median turnaround. — [Entrepreneur News on LinkedIn](https://www.linkedin.com/posts/entrepreneur-newsorg_entrepreneurnews-crosby-ailawfirm-activity-7497412357950885890-JpJ8)
- Basis (agentic accounting): $100M Series B at $1.15B (Feb 2026), serving 7 of the top 25 US accounting firms. — [Finance Brief](https://getfinancebrief.com/story/basis-ai-accounting-unicorn-valuation)

**UiPath (the RPA incumbent closest to Flowera)**
- ARR $1.938B (+12% Y/Y) at Jul 31, 2026. Net new ARR $37M. Net retention 109%. — [UiPath Q2 FY27 8-K](https://ir.uipath.com/financials/sec-filings/content/0001734722-26-000047/path-2026731xex991.htm)
- FY26: ARR $1.853B (+11%), first full year of GAAP profitability, "AI product ARR" nearly $200M. 16 of the top 20 deals included AI products. UiPath says it is "not seeing AI agents replacing deterministic unattended automation in production."
- Its claimed moats: installed base, "deterministic + agentic" under governance, vertical expertise, two decades of enterprise trust.
- Sources: [UiPath FY26 results](https://ir.uipath.com/news/detail/431/uipath-reports-fourth-quarter-and-full-year-fiscal-2026-financial-results); [Q4 FY26 call](https://exa.ai/library/markets/stocks/PATH/earnings/FY2026/Q4/transcript)

### Inferences
- Among the case studies, the companies whose moat looks most durable combine (a) a measured, billable outcome, (b) human-in-the-loop delivery that produces outcome-labelled data, and (c) regulated or trust-gated buyers: EvenUp, Sierra, Abridge and Palantir. The ones most exposed sell a capability the model vendor can ship as a plugin: horizontal legal copilots, coding IDEs, document Q&A.
- The coding category shows that even $2–4B ARR and 70% Fortune 1000 penetration did not prevent a "sell to a compute owner" outcome, and that model suppliers (Claude Code, Codex) compete directly. Revenue scale is not a moat when the supplier is also a competitor.
- Palantir's 2026 message, "your alpha must not become lab training data; run sovereign on open models, with ontology + audit + evals", is almost exactly Flowera's self-hosted, engine-agnostic architecture. That is evidence a sovereignty-framed moat is currently rewarded by buyers. But Palantir's version depends on FDE-heavy delivery and a mature ontology.
- UiPath is still growing ARR 11–12% with 107–109% NRR. Installed-base-plus-governance moats erode slowly rather than collapse, which matters for Flowera as a new entrant: the incumbent is not dying fast.

### Gaps
- No primary-source ARR for Sierra, Decagon, Glean (2026), Cursor or Cognition; all are aggregator or leak estimates.
- NRR/churn is undisclosed for almost all the private companies.
- I did not research Rippling (compound startup), Veeva, ServiceTitan or Pilot in this pass. There is no sourced evidence here on compound-product or vertical system-of-record moats beyond commentary (see Q2).
- Hebbia's current ARR and valuation are unreliable.
- Crosby, Basis, Eudia: no disclosed gross margins, so the "software margins for services" claim is unverified.

---

## Q2. Which moats are argued to be real for AI apps, and which are debunked

### Takeaway
The 2026 consensus, sharpened by the "SaaSpocalypse", sorts the moats into four groups.

- **Debunked or eroding:**
  - prompts and "workflow encoded in a markdown file";
  - thin wrappers;
  - proprietary fine-tuned or pretrained models;
  - static proprietary datasets;
  - generic switching costs, because agents make migration cheap;
  - process power.
- **Holding or strengthening:**
  - counter-positioning against seat-based incumbents (outcome/labor-budget pricing);
  - cornered resources, meaning regulated data, licenses/approvals, and neutrality;
  - dynamic, outcome-labelled feedback loops embedded in the workflow;
  - multi-party systems of record and network effects;
  - trust/compliance/sovereignty as a procurement gate.
- **Mixed:** outcome pricing works where success is unambiguous. The "own the labor budget" thesis (Foundation Capital $4.6T, Sequoia "services: the new software") is widely endorsed but is a TAM thesis, not a barrier by itself.

### Cited Findings

**The SaaSpocalypse as the market's verdict**
- Anthropic launched Claude Cowork on Jan 12, 2026, and 11 open-source job-function plugins on Jan 30.
- The S&P Software & Services index lost 25% between Jan 12 and Feb 23. About $285B was wiped in one day, and LegalZoom fell nearly 20%.
- A partial rebound followed after Anthropic announced connectors *with* incumbents (Docusign, FactSet, Intuit, Salesforce) on Feb 24.
- Source: [DeepLearning.AI The Batch, Feb 27 2026](https://www.deeplearning.ai/the-batch/claude-cowork-plugins-trigger-a-saas-stock-selloff-but-partnerships-lead-to-slight-rebound)
- IGV fell 24% in Q1 2026, its worst quarter since 2008, then recovered to −3% YTD by August. Salesforce is down 40%+ since end-2024. Airtable sold to Bending Spoons for under $1.3B (peak about $12B). 86% of H1 2026 private deal value went to AI companies. — [CNBC, Aug 7 2026](https://www.cnbc.com/2026/08/07/saaspocalypse-debate-intensifies-as-software-stocks-swing-wildly.html)
- "These plugins are just markdown files… Any workflow that can be described in a markdown file is no longer a defensible business." The same author lists what survives: proprietary data that improves with usage, hardware in the loop, regulatory moats, and multi-stakeholder systems of record (Veeva, Procore). — [Shail Kaveti, Feb 2026](https://skaveti.substack.com/p/the-saaspocalypse-how-claude-cowork)
- Counter-view: systems of record (Salesforce, ServiceNow, Workday) face "a winnable transition" to "systems of action". Only workflow wrappers and thin AI apps face "existential commoditization." — [Raphaëlle d'Ornano, Feb 2026](https://www.decodingdiscontinuity.com/p/285-billion-saaspocalypse-wrong-panic)
- ServiceNow says 50% of net new business is now non-seat-based. HubSpot launched pure outcome-based pricing on Apr 14, 2026. — [Crown Point Advisory](https://crownpointadvisorygroup.com/framework/what-is-the-saaspocalypse/) (consultancy; some stats labelled "directional")

**Wrappers, fine-tunes and static data (debunked)**
- Jasper raised at $1.5B (2022). Revenue fell from about $120M (2023) to about $55M (2024) after ChatGPT added native writing features. — [Boltzbit](https://www.boltzbit.com/post/when-the-model-takes-away-the-moat); [Second Lane](https://secondlane.io/insights/with-wrappers-extinction-regulatory-is-the-only-ai-moat-left/)
- BloombergGPT: GPT-4 beat domain-specific models including it on financial NLP benchmarks. — [arXiv 2305.05862](https://arxiv.org/html/2305.05862). The critique that "it never entered the workflow… it was a press release, not a flywheel" is from [Dave Friedman](https://davefriedman.substack.com/p/owning-data-isnt-enough-in-the-age).
- Harvey's own custom model lost to frontier models on BigLaw Bench (see Q1). Legora runs mainly on frontier models. — [LegalRealist](https://legalrealist.ai/posts/26-harvey-v-legora/)
- Static datasets "face 12–18 month vulnerability windows"; an a16z-documented support bot got "zero incremental benefit after collecting 40% of its query distribution." — [tianpan.co, Apr 2026](https://tianpan.co/blog/2026-04-16-when-ai-features-create-moats) (secondary; the a16z original is the 2019 "Empty Promise of Data Moats")
- Fine-tuning an open model on 10K domain examples now costs under $2,000. Synthetic data is projected at 60% of training data by 2028 (Gartner). "Feedback loop data" (corrections, escalations, outcomes) is what counts. — [valueaddvc, May 2026](https://valueaddvc.com/blog/the-data-advantage-myth-why-proprietary-data-alone-wont-save-you)

**Helmer's 7 Powers applied to AI**
- Boris Cherny (Claude Code) argues:
  - switching costs erode ("you can just use the model and… port from one thing to a different thing");
  - process power erodes (a model "can just hill climb anything" given a target);
  - network effects, scale economies and cornered resources persist.
  - Source: [Howardism summary, May 2026](https://www.howardism.dev/articles/seven-powers-applied-to-ai)
- The same summary adds Emergence Capital data: average enterprise AI spend grew 2.2x in 2025 ($16K → $34K/mo) while the median company ran only 6–8 AI vendors. "Winning the first deal is now substantially more valuable." In other words, switching costs are forming even as they erode. — [Howardism](https://www.howardism.dev/articles/seven-powers-applied-to-ai)
- A VC view: the three powers that matter most in 2026 are Counter-Positioning, Cornered Resource and Process Power. The ones founders most often claim and cannot defend are Switching Costs, Scale Economies and Branding. The record 2025–26 Series A rounds were priced on cornered resources: frontier talent, self-generated proprietary data, clinical evidence. — [Iryna Krepchuk, Jun 2026](https://irynakrepchuk.substack.com/p/redefining-the-moat-helmers-7-powers)
- YC discussion on counter-positioning: per-seat incumbents cannot easily move to work-delivered pricing. It gives Legora as a counter-position against Harvey's fine-tuning bet. — [notes on YC video](https://alanhou.org/blog/yc-ai-startup-moats/)
- AI weakens SaaS moats and strengthens marketplace moats (liquidity, reputation data). — [European Internet Ventures](https://www.europeaninternetventures.com/articles/seven-powers-ai)
- Lock-in from complexity is itself vulnerable: "A firm locked into Harvey in 2027 can use Claude to analyze its workflows, extract the logic, and reconstruct equivalents." — [LegalRealist](https://legalrealist.ai/posts/26-harvey-v-legora/)

**Owning the labor budget / services-as-software**
- Foundation Capital's $4.6T figure = $2.3T of salaries (sales/marketing, software engineering, security, HR) + $2.3T of outsourced IT and business-process services (Gartner). — [Foundation Capital](https://foundationcapital.com/ideas/ai-leads-a-service-as-software-paradigm-shift)
- FC's lessons-learned essay: the currency is "the speed with which you can turn promises into provable results", and it names "deep integration and outcome insurance" as the winning moves. — [Foundation Capital](https://foundationcapital.com/ideas/the-4-6t-services-as-software-opportunity-lessons-from-the-first-year)
- Sequoia (Julien Bek, Mar 5, 2026): "The next $1T company will be a software company masquerading as a services firm." About $6 is spent on services for every $1 on software. "A copilot sells the tool. An autopilot sells the work." — [summary at thedeepfeed](https://www.thedeepfeed.ai/posts/2026-04-30-sequoia-services-as-software-thesis/) (secondary summary; I did not fetch the primary essay)

**Outcome pricing and revenue quality**
- Outcome pricing is "highest margins, lowest churn." One AI SDR vendor charges $200 per attended meeting at 94% gross margin. Usage-priced categories saw about 90% price compression within 12 months, and some agent segments have 70% churn. — [Paid.ai report](https://paid.ai/blog/ai-monetization/the-saas-agent-transition-report-from-vibe-pricing-to-value-capture) (vendor-authored; treat as directional)
- Margins: Lovable about 35%, StackBlitz 40%, Replit 23% (per The Information). "Vibe revenue", e.g. 11x counting LOIs as ARR. — [Leonis AI 100](https://www.leoniscap.com/research/the-leonis-ai-100)
- Emergent's "$100M ARR" was one month of about $8.3M collected revenue annualized. Critics call this "vibe revenue run rate." — [Harro](https://harro.com/2026/04/01/explained-what-emergents-100-million-arr-claims-reveal-about-how-ai-startups-report-revenue/)

**Context and workflow knowledge (the new consensus "durable layer")**
- a16z (Aug 2026): computer-use execution "is getting abstracted away… If clicking the right button is no longer the hard part, it is no longer the moat." What is durable is "the context and knowledge of the workflow… the tribal knowledge… how to verify output reliably… increasingly in a single recorded video of someone doing the job once."
- The same piece: agent cost is about $6–8/hour of inference (range $3–15), versus offshore BPO at about $10/hour and US back-office at $30–45/hour. It also says "the harness drives real-world cost as much as the model does."
- Source: [a16z, "Can Agents Use a Computer Yet?"](https://a16z.com/can-agents-use-a-computer-yet-weve-got-the-data/)

### Inferences
- A usable framework for the founder, in five tiers:
  1. **Structurally debunked:** prompts/plugins, UI wrappers, generic RPA-style execution, fine-tuned models as such, static datasets.
  2. **Eroding but still retention-relevant:** integration and workflow switching costs, process know-how.
  3. **Durable if built deliberately:**
     - (a) outcome-labelled feedback data produced *in the course of doing the work*;
     - (b) counter-positioned business models (outcome or labor-budget pricing, self-hosted/sovereign deployment) that seat-based SaaS and cloud-only labs find awkward to copy;
     - (c) cornered trust assets such as certifications, regulatory approvals, insurability and neutrality;
     - (d) multi-party systems of record.
  4. **Accelerants, not moats:** distribution, FDE-style deployment, brand. They win the first deal, which Emergence data says is now worth more.
  5. **Moat-by-structure:** owning the service delivery (roll-ups/neofirms), where the moat is operational margin plus the acquired customer base.
- Flowera's "one VM per employee that records and automates" pitch is exposed on tier 1: execution and record-to-skill are being commoditized (Q3). Its defensible version must live in tiers 3a–3c.

### Gaps
- No rigorous cohort data comparing outcome-priced and seat-priced AI app retention; only vendor surveys.
- I did not fetch the primary Sequoia essay or Stratechery/Elad Gil/Sarah Tavel/NFX/Greylock pieces in this pass. Summaries are secondary.
- The a16z 2019 "Empty Promise of Data Moats" is cited only second-hand.

---

## Q3. Is proprietary human-computer-interaction / trajectory data a genuine moat?

### Takeaway
Real-work trajectory data is currently **scarce and valuable to frontier labs**. Labs acquire it through employee surveillance (Meta), contractor programs (OpenAI + Handshake), acquisitions (Anthropic–Vercept), and consumer products that collect demonstrations (OpenAI Codex Record & Replay, Claude Cowork "Record a Skill").

But three things undercut it as a moat for an *application* company:
- the labs are now collecting it directly at massive scale;
- mined public video (YouTube tutorials) partly substitutes;
- **the record → skill → replay feature itself shipped from both OpenAI (Jun 18, 2026) and Anthropic (Jul 21, 2026).**

Generic demonstrations are a commodity input. Company-specific demonstrations tied to verified outcomes, corrections and approvals are defensible only if they stay inside a customer-controlled system and compound into measurably higher autonomy.

### Cited Findings

**How the labs are acquiring it**
- **Meta MCI (Apr 21, 2026):** tracking software on US employees' computers captures "mouse movements, clicks and keystrokes" plus "occasional snapshots" to train agents on things like "choosing from dropdown menus and using keyboard shortcuts." — [Reuters via Yahoo](https://uk.news.yahoo.com/exclusive-meta-start-capturing-employee-162745587.html). Legal experts say the practice would likely violate GDPR in Europe and is illegal in Italy (same source).
- **OpenAI + Handshake "Project Stagecraft" (reported Jan 2026):** contractors paid at least $50/hour uploaded real work products (PowerPoints, spreadsheets, code) from past and current jobs; 3,000–4,000 freelancers. — [AI2Work summary](https://ai2.work/blog/meta-tracks-employee-keystrokes-and-clicks-to-build-ai-agents) (secondary)
- **Anthropic acquired Vercept (Feb 25, 2026)** "to advance Claude's computer use", after Sonnet 4.6 reached 72.5% on OSWorld. Vercept's product "Vy" was wound down. — [Anthropic](https://www.anthropic.com/news/acquires-vercept); [The Decoder](https://the-decoder.com/anthropic-acquires-vercept-to-give-claude-sharper-eyes-for-reading-and-controlling-computer-screens/)
- **OpenAI Codex Record & Replay (Jun 18, 2026, v26.616):** a person demonstrates a macOS workflow once and gets an editable SKILL.md with variable inputs. It stores "semantic intent rather than pixel coordinates." macOS-only and excluded from the EEA/UK/CH at launch. — [Digital Applied](https://www.digitalapplied.com/blog/openai-codex-record-replay-no-code-agent-skills-guide); [Daniel Vaughan](https://codex.danielvaughan.com/2026/07/06/record-and-replay-programming-by-demonstration-codex-cli-skill-generation-open-agent-skills-standard/)
- **Anthropic "Record a Skill" in Claude Cowork (Jul 21, 2026):** Pro/Max/Team users screen-record and narrate a task and get back a rerunnable skill. Commentary: "A shared library of company-specific skills, recorded by employees… is a stickier moat than any single benchmark win." — [Let's Data Science](https://letsdatascience.com/blog/claude-cowork-record-a-skill-teach-ai-by-demonstration)
- The SKILL.md format (Agent Skills) is adopted by 30+ agent products, so recorded skills are portable across Codex, Claude Code, Gemini CLI, Cursor, etc. — [Daniel Vaughan](https://codex.danielvaughan.com/2026/07/06/record-and-replay-programming-by-demonstration-codex-cli-skill-generation-open-agent-skills-standard/)

**Substitutes and prices**
- Mining public video: VideoAgentTrek (ICLR 2026) recovered 1.52M action steps from 39,000 YouTube tutorials. OSWorld success rose from 9.3% to 15.8%. — [paper note](https://en.papernotes.org/ICLR2026/llm_agent/videoagenttrek_computer-use_pretraining_from_unlabeled_videos/)
- a16z mentions Standard Intelligence's action model trained on an 11-million-hour video dataset. It says labs and startups "poured hundreds of millions into computer-use RL environments" (Mechanize, Habitat, Fleet, Chakra, Deeptune, Matrices, Originator). OSWorld-Verified went from 42% to 85% in a year, above about 72% for humans. — [a16z, Aug 2026](https://a16z.com/can-agents-use-a-computer-yet-weve-got-the-data/)
- Anthropic is reportedly weighing $1B+/year on RL environments across a dozen-plus vendors (unconfirmed report relayed by blog). — [Pebblous](https://blog.pebblous.ai/blog/ai-data-supply-oligopoly/en/)
- Pay rates:
  - Mercor about $85–95/hour average for experts, $175–200+/hour for credentialed profiles;
  - Handshake $100–125/hour;
  - Outlier's mid-tier rates fell from $28–35 to $18–22/hour as automation absorbed easy work;
  - sources: [HeroHunt](https://www.herohunt.ai/blog/hiring-ai-training-talent-labs-guide-2026/); [arXiv 2605.03295](https://arxiv.org/html/2605.03295v1).
  - No public per-trajectory or per-hour price for *screen-recording/computer-use* data was found.
- Vendors now sell exactly this category:
  - Enjamb Labs licenses "expert trajectories" and "digital twins of industry software seeded with real history", priced "per corpus, per environment, per eval slice… exclusive or shared." — [Enjamb Labs](https://www.enjamblabs.com/labs)
  - Beyond Labs sells "agent trajectory capture" (screen + terminal + reasoning). — [Beyond Labs](https://beyondlabs.io/ai-data)
- A founder of an interview-copilot product (20M users) said in a podcast that frontier labs approached him in late 2025 to buy knowledge-worker desktop screen recordings: "there is nowhere on the market you can simply buy work recordings." — [Mossfire podcast transcript, Jul 2026](https://insights.mossfire.xyz/en/posts/ep41-screen-recording-rush/) (single anecdotal source; some claims in it are unverified or wrong, e.g. it describes the Anthropic acquisition inexactly)
- **Handshake lab work:** Garrett Lord says Handshake collects "trajectories and other long-horizon demonstrations that involve stitching together step by step instruction following." — [arXiv 2605.03295](https://arxiv.org/html/2605.03295v1)

### Inferences
- **As a product feature**, record → learn → replay is no longer a differentiator. OpenAI and Anthropic ship it to paying users with a portable skill format, and it is priced into a $20–200/month subscription. Flowera's own skill export is already SKILL.md/Agent-Skills compatible, which cuts both ways: good for adoption, bad for lock-in.
- **As a data asset**, trajectory data is valuable, but its buyers are also its most powerful collectors. An app company selling it to labs becomes a Mercor-style supplier: large, but concentrated on a handful of buyers, with labor-pass-through margins and neutrality/security risk (Scale's exodus, Mercor's breach).
- **The defensible slice** is data the labs structurally cannot get:
  - trajectories in customers' *private, self-hosted* environments (regulated industries, EU where Meta-style capture is likely unlawful and where Codex Record & Replay did not launch);
  - paired with **human corrections, approval decisions and verified outcomes**, which is Flowera's escalation-ladder and autonomy data.
  - This matches Foundation Capital's "decision traces" and a16z's "context and knowledge of the workflow" (Q5). It is a per-customer moat (switching cost and compounding autonomy), not a cross-customer network effect, unless customers consent to pooling.
- Privacy redaction at the source is a prerequisite for that position, not a moat in itself. It does, however, support a trust/compliance claim (Q4).

### Gaps
- No verified prices for computer-use trajectories, per hour or per task. No public disclosure of Vercept's acquisition price.
- No evidence yet that company-specific demonstration libraries raise retention or NRR measurably; this is claimed, not shown.
- Unknown whether Codex/Cowork recorded skills are used to train lab models by default (the podcast claim that individual-plan data is used for training is unverified).

---

## Q4. Does "trust infrastructure" (audit, approval, verification, agent insurance) become a moat?

### Takeaway
Trust infrastructure is becoming a **procurement gate**, not yet a moat by itself:
- certification standards (AIUC-1) with Big Four and auditor adoption;
- Lloyd's-backed agent insurance;
- EU AI Act logging duties;
- incumbents such as UiPath and Palantir selling "governance + audit + deterministic execution."

It becomes a moat when it produces something competitors cannot fake. The candidates are an insurable loss history, a certification that takes quarters to earn and must be renewed, or an audit/decision record that regulators and insurers rely on. It is weakest as a standalone "approval/audit layer" product, which is already crowded (Contro1, Auditable AI, Workday, Microsoft Entra agent logs).

### Cited Findings

**AIUC and agent insurance**
- AIUC came out of stealth in Jul 2025 with a $15M seed led by Nat Friedman (NFDG). — [Agent Insured](https://agentinsured.eu/articles/what-ai-insurance-underwriters-ask-before-writing-policy)
- AIUC then raised a $40M Series A led by Ribbit Capital (reported Sep 2026). — [Forkast](https://forkast.news/aiuc-raises-40m-to-build-the-certification-and-insurance-layer-that-makes-agent-governance-auditable/)
- AIUC-1 covers 51 requirements and 130 controls across six pillars (data and privacy, security, safety, reliability, accountability, society). It requires 5,000+ adversarial tests and quarterly retests. — [Agent Insured](https://agentinsured.eu/articles/what-ai-insurance-underwriters-ask-before-writing-policy); [Forkast](https://forkast.news/aiuc-raises-40m-to-build-the-certification-and-insurance-layer-that-makes-agent-governance-auditable/)
- Adoption:
  - ElevenLabs became the first AIUC-1-backed insured agent in Feb 2026 (5,835 tests across 14 risk categories). — [ElevenLabs](https://elevenlabs.io/blog/aiuc-announcement)
  - KPMG was the first Big Four firm certified, in Aug 2026. — [Forkast](https://forkast.news/aiuc-raises-40m-to-build-the-certification-and-insurance-layer-that-makes-agent-governance-auditable/)
  - Schellman was the first accredited auditor (Feb 2026). — [AIUC-1](https://www.aiuc-1.com/)
  - UiPath is a founding technical contributor. The Q3 2026 update adds coding agents. — [AIUC-1](https://www.aiuc-1.com/)
  - AIUC advertises coverage up to $50M. — [AIUC](https://aiuc.com/product)
- Armilla is a Lloyd's coverholder writing affirmative AI liability with limits up to $25M per organisation. Its "AI Agent Mistakes" cover explicitly includes "failure to escalate." — [Armilla](https://www.armilla.ai/); [Agent Insured](https://agentinsured.eu/articles/what-ai-insurance-underwriters-ask-before-writing-policy)
- Underwriters ask for four things: a system description, a governance/oversight record, an incident log, and a scope-constraints statement ("what human approval gates exist"). Plan on 90 days. — [Agent Insured](https://agentinsured.eu/articles/what-ai-insurance-underwriters-ask-before-writing-policy)
- Skepticism: the category is "nascent", and the policies are untested in a large systemic failure. — [Forkast](https://forkast.news/aiuc-raises-40m-to-build-the-certification-and-insurance-layer-that-makes-agent-governance-auditable/)

**Incumbents selling trust**
- UiPath: "Enterprises don't simply pay for code. They pay for trust, for operability, and for governance." — [UiPath Q4 FY26 call](https://exa.ai/library/markets/stocks/PATH/earnings/FY2026/Q4/transcript)
- Palantir credits "security and audit… evals and customer-specific benchmarks" as part of AIP's advantage. — [Q2 2026 call](https://bullfincher.io/companies/palantir-technologies/earnings-call/fy2026-q2)

**Regulation as a gate**
- Legora's EU data residency is described as "not a preference — it is a procurement gate." — [TechTimes](https://www.techtimes.com/articles/324348/20260813/legal-ai-arms-race-legora-eyes-10b-harvey-nears-155b-one-month.htm)
- EU AI Act Article 50 transparency obligations start Aug 2, 2026. OpenAI excluded Record & Replay from the EEA/UK/CH at launch without giving a reason. — [Digital Applied](https://www.digitalapplied.com/blog/openai-codex-record-replay-no-code-agent-skills-guide)
- "Regulatory moats. FDA clearance, SOC 2… Compliance isn't a prompt. It's a system." — [Kaveti](https://skaveti.substack.com/p/the-saaspocalypse-how-claude-cowork)
- "Regulatory is the only AI moat left" (opinion). — [Second Lane](https://secondlane.io/insights/with-wrappers-extinction-regulatory-is-the-only-ai-moat-left/)

**Box's Aaron Levie on deterministic access control**
- "agents can't keep secrets… you have to rely on deterministic systems to control access levels."
- "people underestimate how many years of organizational wiring [permissions] have now been incorporated into software."
- Source: [Foundation Capital, "The case for context graphs"](https://foundationcapital.com/ideas/the-case-for-context-graphs)

### Inferences
- Flowera's approval gate, audit trail, redaction and self-hosting are exactly the artifacts underwriters and certifiers request: oversight record, incident log, scope constraints and human approval gates. The strategic option is to make Flowera the fastest path to an AIUC-1 certificate or an insured deployment for computer-use agents. ElevenLabs claims its platform gets customers "75% of the way" to certification. That turns compliance work into distribution and a switching cost.
- Stronger, but longer-dated: if Flowera accumulates verified autonomy rates and incident histories across deployments, that dataset is what insurers need to price agent risk. AIUC's model ties audit outcomes to premiums. An "autonomy rate + incident log" per workflow is a candidate cornered resource *if* it becomes the number insurers or auditors accept.
- Risk: the approval/audit layer on its own is being built by many vendors (Q5) and by platforms (Microsoft Entra, Workday, ServiceNow, UiPath Maestro). Trust infrastructure as a standalone product has low barriers. It compounds only when tied to execution (the actions are gated where they happen) and to an external validator (insurer, auditor, regulator).

### Gaps
- No data on claims paid, premiums, or whether AIUC-1 certification has measurably shortened sales cycles or reduced churn.
- No evidence yet of enterprises *requiring* agent insurance in RFPs at scale.
- No information on whether any insurer covers computer-use/desktop agents specifically.

---

## Q5. What makes a product "the system of record for agent work"? Who is pursuing it?

### Takeaway
The best-developed thesis is Foundation Capital's "context graphs" (Dec 2025 onward). The claim is that systems-of-agents startups sit "in the execution path at commit time." They can therefore persist *decision traces* (inputs gathered, policy applied, exception granted, approver, outcome). Incumbent systems of record, which store current state, cannot, and those traces compound into precedent that lets autonomy "ratchet."

To be *the* record, a product must be:
- in the write path, not added by ETL afterwards;
- authoritative for approval and authorization, not just observability;
- replayable;
- cross-system;
- relied on by someone outside engineering (auditors, regulators, insurers).

Many players claim the label:
- Workday's "Agent System of Record" (Feb 2025);
- Glean ("system of context");
- UiPath Maestro;
- Palantir's ontology;
- startups such as Contro1, Reload and Auditable AI.

No one owns it yet. Investors expect the winners to be vertical, and the "why" is only partially capturable.

### Cited Findings

**Foundation Capital's context-graph thesis**
- Decision traces capture "what happened in this specific case (we used X definition, under policy v3.2, with a VP exception, based on precedent Z…)."
- "Systems of agents startups have a structural advantage. They sit in the execution path… Incumbents… can't insert themselves into an orchestration layer they were never part of."
- It "starts with human-in-the-loop: the agent proposes, gathers context, routes approvals, and records the trace… more of the path can be automated."
- Source: [Foundation Capital, "AI's trillion-dollar opportunity: Context graphs"](https://foundationcapital.com/ideas/context-graphs-ais-trillion-dollar-opportunity); [Ashu Garg](https://ashugarg.substack.com/p/ais-trillion-dollar-opportunity-context)
- Their own caveats:
  - the main objection is "you can't actually capture the why"; the answer is to "capture the how… and infer the why from patterns";
  - "Decisions have a half-life", so precedent must age out;
  - there is no consensus on where the graph should live (warehouse vs orchestration layer vs a "context OS").
  - Endorsements: Dharmesh Shah ("a system of record for decisions, not just data"), Aaron Levie ("the era of context"), Arvind Jain (Glean).
  - Source: [FC, "Context graphs, one month in," Jan 2026](https://foundationcapital.com/ideas/context-graphs-one-month-in)
- "The best context graph companies will be verticalized… to build one, you need to be embedded in a specific workflow." "Early movers who move in 2026… create this flywheel that then becomes the moat." The panel explicitly draws the "UiPath analogy… you can just watch someone complete a workflow and then automate it." — [FC, "Why context graphs are the missing layer"](https://foundationcapital.com/ideas/why-context-graphs-are-the-missing-layer-for-ai)
- Levie's counterpoint: agents may keep using existing systems as tools because those tools already encode permissions. Recreating a graph outside them is "a huge uphill battle." — [FC, "The case for context graphs"](https://foundationcapital.com/ideas/the-case-for-context-graphs)

**Who is pursuing it**
- **Workday** launched an "Agent System of Record" to manage fleets of its own and third-party agents (onboarding, access, monitoring, cost). Analysts question whether this belongs to HR or IT. — [Diginomica, Feb 2025](https://diginomica.com/workday-launches-system-of-record-ai-agents-it-or-hr)
- **Contro1** markets "the system of record for your AI agents": agent inventory, runtime human approval, HMAC-signed evidence, and seven recorded events per consequential action. Free tier and a $5K pilot. — [Contro1](https://contro1.com/resources/system-of-record-for-ai-agents)
- **Reload** positions as "a system of record for AI agents" and raised $2.275M (Feb 2026). — [FindArticles](https://www.findarticles.com/reload-launches-epic-to-give-ai-agents-shared-memory/)
- **Auditable AI** is an open-source "system of record for AI-agent decisions" that replays decisions under live state and rolls back stale actions. — [auditable.run](https://auditable.run/)
- **Glean**: "system of context for organizational intelligence." — [Glean](https://www.glean.com/blog/glean-series-f-announcement)
- **UiPath Maestro** (built on Temporal) is pitched so that "workflows are defined in a way AI agents can generate and modify directly while remaining fully transparent to business stakeholders and auditors." Maestro Case covers exception-heavy processes (investigations, approvals). — [UiPath Q4 FY26 call](https://exa.ai/library/markets/stocks/PATH/earnings/FY2026/Q4/transcript); [UiPath Q2 FY27](https://ir.uipath.com/financials/sec-filings/content/0001734722-26-000047/path-2026731xex991.htm)
- **Anthropic's Team-plan shared skill libraries** are framed as "institutional memory a rival cannot copy." — [Let's Data Science](https://letsdatascience.com/blog/claude-cowork-record-a-skill-teach-ai-by-demonstration)
- **Palantir**: "AIP is where your sovereign AI is built, deployed, and compounded." — [Q2 2026 call](https://transcripts.platformaeronaut.com/transcripts/PLTR-2Q26-transcript)

### Inferences
- **Criteria for "system of record for agent work"**, drawn from the sources:
  1. It is the write path: the action executes through it.
  2. Every consequential action carries identity, authority, approver, evidence and outcome.
  3. It is replayable and auditable after models and data change.
  4. Its precedent is used to raise autonomy (the ratchet).
  5. It is readable and relied on by non-engineers: compliance, auditors, insurers.
  6. It spans the systems a "glue" role touches (RevOps, FinOps, support engineering).
  7. It is hard to extract, because the history is created at decision time and cannot be reconstructed by a newcomer.
- **Flowera already has pieces of 1–4:**
  - execution inside the VM;
  - the approval gate;
  - the recordings/audit trail;
  - task records (Task → Execution → Steps → Interventions → Outcome);
  - the autonomy metric;
  - the heal → model → human escalation ladder, whose corrections are kept only when they worked.
  - That maps closely onto FC's "human-in-the-loop → precedent → autonomy ratchet."
- **Flowera's gaps against criteria 5–7:**
  - its record is desktop/UI-centric rather than tied to business objects (accounts, invoices, cases);
  - its horizontal "one VM per employee" framing conflicts with the investor view that winners will be vertical;
  - it has no external party yet relying on its records.
- **Competitive threat:** Anthropic/OpenAI own the record → skill loop for individual users, and UiPath owns governed enterprise orchestration. Flowera's distinctive corner is the *self-hosted, per-worker, cross-engine* record of what was done, who approved it, what was corrected and the verified outcome. It is most defensible in regulated or data-sovereign buyers where cloud labs' desktop capture is legally or contractually barred (EU/GDPR, finance, health, defense), exactly where Palantir's "sovereign AI" is winning.
- **Candidate moat to build deliberately** (combining Q2–Q5): a vertical "system of record for delegated work" in a regulated, exception-heavy back-office function, made up of:
  - (a) decision traces and correction data that raise a published autonomy rate (process data that compounds per customer);
  - (b) pricing on outcomes or labor budget (counter-positioning against seat-based SaaS and per-token labs);
  - (c) records packaged as insurer- or auditor-grade evidence (a trust procurement gate);
  - (d) deployment on-prem/self-hosted (a sovereignty cornered position).
  - The option of pooling anonymized trajectories for lab licensing is secondary and risky (concentrated buyers, privacy exposure).

### Gaps
- No revenue or traction data for any "system of record for agents" vendor. The category is pre-revenue or early, and it is unclear whether buyers treat it as a separate budget line or a feature of Microsoft, ServiceNow, Workday or UiPath.
- No evidence yet that decision-trace or context-graph accumulation has produced measurable retention or autonomy gains at any named company. PlayerZero is cited by FC, an investor, without metrics.
- It is unresolved whether agent records will live in the orchestration layer, the data warehouse or the identity layer (Microsoft Entra now logs agent sign-ins per Contro1's description; I did not verify this at Microsoft).
