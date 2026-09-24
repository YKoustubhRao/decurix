# Vertical wedges for a demonstration-learned, auditable, self-hosted GUI agent (research as of 2026-09-24)

Scope: which verticals make AI agents operate legacy desktop/GUI software with no APIs, how much labor is at stake, who is already funded to attack each one, and which sub-niches are still open, ranked for a small, technical, self-hosted team (Decurix/Flowera: a persistent VM per worker, AT-SPI + CDP + Windows host-app driving, record-once/replay-deterministic with self-healing, approval gates, before/after screenshot audit trail, secret redaction, local models).

Method note: about 45 web searches and fetches on 2026-09-24. Every dated figure carries its date. Where a figure comes from an aggregator or vendor blog rather than a primary source, it says so. The eCFR fetch for 21 CFR 11.10 was blocked by a federalregister.gov redirect, so that regulation's wording is cited to the eCFR URL from well-established text and not re-read live (see Gaps).

---

## Q1. Candidate verticals: labor spend, GUI-only share, funded startups, and gaps

### Takeaway
Healthcare revenue cycle and prior authorization is the biggest and best-documented pool of labor spend, but in September 2026 it is also the most crowded, with nine-figure rounds (Candid $120M, Latent $80M, Commure $70M at $7B, Tennr $101M, Luminai $38M, Anterior $40M). A federal FHIR prior-auth API mandate (Jan 2026/Jan 2027) is also eroding the "no API" premise at the payer end. The better wedges for a small self-hosted team are verticals where (a) the system of record is a Windows desktop or Citrix app with no DOM, (b) funded startups work around the GUI rather than inside it, and (c) regulation rewards an independent audit trail. The strongest matches are accounting-firm desktop tax software, GxP/life-sciences quality operations, customs brokerage entry work, and the long tail of healthcare practice-management desktops (dental/DSO, home health/hospice, behavioral health). Freight brokerage is crowded and email/voice-centric, and mostly does not need GUI driving.

### Cited Findings

**Horizontal agentic/RPA baseline (the incumbents and the horizontal "record once" competitors)**
- Gartner: the RPA software market grew 14.5% to $3.6B in 2024. GenAI, computer-use tools and agentic automation *slowed* RPA growth that year, and UiPath, Microsoft and Automation Anywhere led. — [Gartner Market Share Analysis: RPA 2024](https://www.gartner.com/en/documents/6842834) (snippet via search). Precedence Research gives a far larger "RPA market" ($35.27B entering 2026). This is a services-inclusive, non-comparable definition, and it conflicts with Gartner's software-only figure. — [Precedence Research](https://www.precedenceresearch.com/robotic-process-automation-market)
- UiPath Q2 FY2027 (quarter ended 2026-07-31, reported 2026-09-03): revenue $410M (+13% YoY), ARR $1.938B (+12%), net new ARR only $37M, dollar-based net retention 109%. — [UiPath IR](https://ir.uipath.com/news/detail/461/uipath-reports-second-quarter-fiscal-2027-financial-results)
- Sola (a16z-led $17.5M Series A, Aug 2025; ~$21M total): customers "record a process once on their computer using a Sola extension" and Sola turns it into a running, adaptable agent, aimed at traditional industries such as logistics and healthcare. This is the closest horizontal analogue to Flowera's record-once model. — [a16z](https://a16z.com/announcement/investing-in-sola/); [SiliconANGLE 2025-08-14](https://siliconangle.com/2025/08/14/sola-solutions-raises-17-5m-enhance-enterprise-process-automation/). No Series B found as of Sept 2026. — [Tracxn](https://tracxn.com/d/companies/sola/__xTEmWPLROqxTpUhuowLvzxUcT4XeCwU06eH4mbfTo44)
- Automat (YC; $15.5M Series A led by Felicis, $19.25M total) turns "screen recordings, SOPs, and reference materials" into agentic workflows that operate software like human users, positioned against UiPath/AA/Blue Prism. It publishes content on automating SAP and Citrix without APIs. — [Automat blog](https://runautomat.com/blog/automat-raises-15-5m-series-a); [Automat SAP/Citrix](https://runautomat.com/blog/automate-sap-citrix-without-apis)
- Minicor (YC P26, spring 2026): "self-healing desktop automations" on Windows VMs for AI startups whose customers "run on legacy desktop software with no APIs" (healthcare, automotive, financial services, logistics). It runs on-prem, in the cloud or "behind a Citrix login", and pairs deterministic Python automation with AI agents for recovery, claiming 93-96% click accuracy against 80-85% for pure computer use. No funding amount found. — [YC launch](https://www.ycombinator.com/launches/Pkq-minicor-self-healing-desktop-automations-that-scale); [Launch HN](https://news.ycombinator.com/item?id=48280729)
- Orby AI (Large Action Model, "observe-learn-automate") was acquired by Uniphore on 2025-08-28, terms undisclosed. — [Uniphore](https://www.uniphore.com/press-releases/uniphore-announces-the-acquisitions-of-orby-ai-and-autonom8/)
- Twin Labs (Paris): $10M seed led by LocalGlobe, Jan 2026, no-code autonomous agents, cloud-first. — [Dealroom](https://app.dealroom.co/news/feed/twin-labs-raises-10m-to-launch-no-code-ai-agents-that-work-autonomously-not-just-assist-1)
- Magical: $41M total ($35M Series A led by Coatue, 2022). It now positions as "AI employees" for healthcare admin and revenue cycle, claiming >90% end-to-end reliability. — [Magical blog](https://www.getmagical.com/blog/magical-raises-41m-in-funding-after-automating-over-a-billion-repetitive-tasks-at-over-10-000-companies); [getmagical.com](https://www.getmagical.com/)

**1. Healthcare revenue cycle (RCM) and prior authorization**
- CAQH Index (published 2026-02-19, 2024 data): US healthcare avoided $258B through electronic transactions, with a remaining **$21B** savings opportunity from fully automating manual and partially manual transactions. More than 50% of health plans and 25% of provider organizations use AI in admin workflows. — [CAQH](https://www.caqh.org/blog/2025-caqh-index-shows-u.s.-healthcare-avoided-258-billion-and-accelerated-automation-interoperability-and-ai-adoption)
- AHA "Costs of Caring" (2026-03): hospitals spent **$43B in 2025** trying to collect from insurers for care already delivered, including ~$18B on overturning denials. The average hospital employed ~64 administrative and billing staff (6.5% of employment, 2024). Labor is 56% of hospital spending. — [TechTarget on AHA](https://www.techtarget.com/revcyclemanagement/news/366640113/AHA-Hospitals-spent-43B-chasing-pay-from-denials-prior-auths); [AHA report](https://www.aha.org/guides-and-reports/2026-03-09-2025-cost-caring-report)
- Funded startups (dates of rounds):
  - Candid Health: $120M Series D (Sixth Street, 2026-07-22), after a $52.5M Series C in Feb 2025. — [RevCycleAI deals](https://revcycleai.com/deals-and-raises/)
  - Commure: $70M growth round at a $7B valuation (General Catalyst, 2026-05-19). — [RevCycleAI deals](https://revcycleai.com/deals-and-raises/)
  - Latent Health: $80M Series A (Spark Capital + Transformation Capital, 2026-03-19), AI prior auth and medication access. — [FinSMEs](https://www.finsmes.com/2026/03/latent-health-raises-80m-in-series-a-funding.html). A Forbes piece the same week describes a "$600 million AI startup" in drug prior-auth paperwork with >$20M annualized revenue. That matches Latent by date and round, but the search snippet did not name it, so the attribution is inferred. — [Forbes 2026-03-18](https://www.forbes.com/sites/rashishrivastava/2026/03/18/this-600-million-ai-startup-is-speeding-up-paperwork-so-patients-get-drugs-faster/)
  - Tennr (referral intake, fax): $101M Series C at $605M (IVP, 2025-06-18), $159M total. No 2026 round found. — [Fortune](https://fortune.com/2025/06/18/tennr-health-tech-ai-patient-referral-ivp-a16z-lightspeed-iconiq-series-c/)
  - Luminai: $38M Series B (Peak XV, 2026-04-09), $60M total. Referral intake, registration, order processing, pharmacy renewals, payer contract management and denial appeals for health systems. — [PR Newswire](https://www.prnewswire.com/news-releases/luminai-raises-38-million-series-b-to-scale-intelligence-across-health-system-operations-302737804.html)
  - Anterior (payer-side prior auth / utilization management): $40M (Feb 2026), $64M total, with Sequoia, NEA and FPV. — [PR Newswire](https://www.prnewswire.com/news-releases/anterior-closes-40-million-to-accelerate-health-plan-ai-adoption-bringing-total-funding-to-64-million-302685959.html)
  - Cohere Health: $90M Series C (Temasek, 2025-05-14). — [RevCycleAI deals](https://revcycleai.com/deals-and-raises/). A RevCycleAI blog snippet claiming "$7B valuation" for Cohere appears to conflate it with Commure's $7B round. Treat it as unreliable. — [RevCycleAI blog](https://revcycleai.com/blog/cohere-health-vendor-deep-dive/)
  - Infinitus (AI phone calls to payers): $51.5M Series C led by a16z (Nov 2024). — [Healthcare IT Today](https://www.healthcareittoday.com/2024/11/25/infinitus-systems-raises-51-5-million-series-c-funding-on-the-strength-of-ai-guardrails/)
  - Other: AKASA ($60M Series B, 2021), SmarterDx ($50M Series B, 2024). — [RevCycleAI deals](https://revcycleai.com/deals-and-raises/)
- Aggregate funding: digital health AI investment reached $7.4B in H1 2026 (vendor/media aggregator). Late-2025/early-2026 agentic health AI rounds averaged $155M. — [MarketScale](https://www.marketscale.com/industries/healthcare/digital-health-ai-investment-hits-74b-in-h1-2026-as-cms-hospitals-and-startups-all-accelerate-adoption)
- **An API mandate erodes "no API" at the payer end:** CMS-0057-F requires impacted payers (MA, Medicaid/CHIP managed care, QHPs) to run FHIR APIs, including a Prior Authorization API. Operational provisions apply from 2026-01-01, and the APIs are due by **2027-01-01**. Decision timelines are 72h urgent and 7 days standard. — [CMS fact sheet](https://www.cms.gov/newsroom/fact-sheets/cms-interoperability-prior-authorization-final-rule-cms-0057-f); [Health Samurai](https://www.health-samurai.io/articles/understanding-the-cms-0057-f-interoperability-and-prior-authorization-final-rule)

**2. Healthcare long tail: dental/DSO, home health and hospice, post-acute**
- Dental practice-management systems (Dentrix, Eaglesoft, Open Dental) are the systems of record. The AI vendors found (Verifiq, Savvy Agents, Dentina.AI, VideaHealth, Pearl) focus on insurance verification, voice reception and imaging, and "integrate" with the PMS. No large funded pure-GUI back-office player was found. — [Aron.ai comparison](https://getaron.ai/blog/best-insurance-verification-software-for-dental-offices); [Mentera PMS comparison](https://mentera.ai/blog/dental-pms-comparison-ai-layer-integration)
- Home health and hospice: Enzo Health launched an "agentic EHR" (2026-06-01), which replaces the EMR rather than driving it. IO Health (2026-09-18) layers AI into existing EMR interfaces. WorldView does OCR intake. — [Enzo PR](https://www.prnewswire.com/news-releases/enzo-health-launches-first-agentic-ehr-for-home-health-connecting-referral-to-reimbursement-in-one-system-302786174.html); [IO Health](https://www.einpresswire.com/article/943390057/io-health-brings-real-time-ai-intelligence-directly-into-existing-home-health-and-hospice-emr-workflows)

**3. Insurance claims and underwriting (Guidewire, AS/400, legacy cores)**
- Insurtech funding was $1.63B in Q1 2026, and AI firms took 95.2% of it (secondary aggregator). — [FinanceX Magazine](https://www.financexmagazine.com/post/insurtech-s-agentic-awakening-943-million-in-q1-funding-claims-in-minutes-and-the-regulators-cat) (its headline says $943M, which conflicts internally with the $1.63B in the snippet, so treat it as unverified)
- Underwriting AI: Sixfold $30M Series B (Brewer Lane, with **Guidewire as a strategic investor**, Jan 2026). Federato $100M Series D. Kalepa ~$14M total, ~30 staff. — [FinTech Global](https://fintech.global/2026/01/30/insurtech-firm-sixfold-secures-30m-to-advance-ai-underwriting/); [Fabio Faschi market note](https://www.fabiofaschi.com/blog/the-state-of-the-insurance-tech-market-what-mgas-and-carrier-underwriting-teams-actually-need-to-buy-in-2026)
- Service/claims voice: Liberate raised $50M at $300M (Battery, Oct 2025), with voice agent "Nicole" for quoting, claims and endorsements. — [Insurance Business](https://www.insurancebusinessmag.com/us/news/technology/quick-everyone-lets-make-an-insurance-ai-startup-581088.aspx)
- Legacy prevalence: "approximately 45%-50%" of US P&C carriers with $100M-$1B in premium still ran core systems deployed before 2005 (per unnamed 2025 surveys cited by a vendor blog, so weak sourcing). Adacta's 2025 survey: 46% cite integration challenges and 45% high maintenance costs. — [Adacta](https://blog.adacta-fintech.com/state-of-insurance-legacy-system-modernization); [Intellias](https://intellias.com/insurance-legacy-system-transformation/)
- "The bottleneck is connecting those models to the 15-20 legacy systems that a typical carrier runs" (commentary). — [Tommaso Maria Ricci](https://www.tommasomariaricci.com/blog/ai-for-insurance-companies)

**4. Freight, logistics and customs brokerage**
- Freight is crowded with well-funded players working over email, voice and TMS integrations:
  - HappyRobot: $150M Series C at $1.2B (2026-08-04). — [Forkast](https://forkast.news/happyrobot-wants-1-2-billion-to-run-your-freight-by-voice/)
  - Loop: $95M Series C (Valor, 2026-04-17). — [BusinessWire](https://www.businesswire.com/news/home/20260417578056/en/Loop-Raises-$95M-Series-C-to-Scale-Its-AI-Platform-Across-the-Supply-Chain)
  - Pallet: $27M Series B (General Catalyst, May 2025), $50M total. Order entry, quoting, **portal updates**. — [BusinessWire](https://www.businesswire.com/news/home/20250527164246/en/Pallet-Secures-$27M-Series-B-Led-by-General-Catalyst-to-Scale-AI-Workforce-for-Logistics)
  - Vooma: $16.6M (Craft-led A + Index seed). — [FreightWaves](https://www.freightwaves.com/news/vooma-grabs-16-6m-in-funding-as-brokers-prepare-for-market-swing)
  - FleetWorks: $17M Series A (First Round, 2025). — [DC Velocity](https://www.dcvelocity.com/startup-gets-16-million-to-fund-its-ai-for-freight-brokers)
  - Cargofy: EUR 9.5M Series A (June 2026), 70+ tool connectors. — [EU-Startups](https://www.eu-startups.com/2026/06/cargofy-raises-e9-5-million-series-a-to-deploy-ai-digital-workers-across-freight-operations/)
  - Freight Hero: $5M seed (July 2026). — [FinSMEs](https://www.finsmes.com/2026/07/freight-hero-raises-5m-in-seed-funding.html)
- Customs brokerage is thin on funding and small rounds:
  - Pax AI: $4.5M seed (Initialized), duty drawback. — [Pax AI](https://www.paxai.com/blog/pax-ai-raises-4-5m-seed-funding-duty-drawback-ai-tariff-refunds)
  - GingerControl: $2.1M seed. — [Signalbase](https://www.trysignalbase.com/news/funding/gingercontrol-secures-21m-seed)
  - Tarifflo: ~$500K pre-seed, working with CBP on entry-summary documentation. — [TechBuzz](https://www.techbuzznews.com/tarifflo-leverages-agentic-ai-to-automate-tariff-classification/)
  - Amari AI emerged from stealth. — [LinkedIn](https://www.linkedin.com/posts/sambuddhabasu1_amari-ai-ai-for-customs-brokers-activity-7394537374309003264-Wciy)
  - Incumbents KlearNow and Digicust. — [KlearNow](https://klearnow.ai/)
- Amberd (customs AI) runs a **private LLM** and moves sensitive ACE data "into the broker's own cloud environment, preserving data ownership and the privacy posture required for regulated trade information". This is direct evidence that brokers value private deployment. — [SupplyChainBrain](https://www.supplychainbrain.com/articles/44567-private-ai-reshapes-customs-brokerage-under-a-regulatory-whiplash)

**5. Banking, credit unions and KYC**
- Core vendors are shipping their own agent layers: Fiserv agentOS (launched 2026-05-14, built with OpenAI on AWS Bedrock AgentCore, piloting at First Interstate and Boulder Dam CU), and an FIS Financial Crimes AI Agent built with Anthropic (GA H2 2026). — [Butler Brief](https://butlerbrief.substack.com/p/the-agent-layer-showed-up-inside); [CCG Catalyst](https://www.ccgcatalyst.com/thought-leadership/research-snapshot/sector-spotlight-ai-agents-and-connectors-for-banks-and-credit-unions/)
- Startups: Uptiq $25M (Feb 2026, AI agents in bank workflows) and Gradient Labs $26M (customer ops). — [Gradient Labs guide](https://gradient-labs.ai/guides/best-ai-agents-for-credit-unions)
- India: the RBI issued Guidance on Regulatory Principles for Model Risk Management on **2026-06-24**, covering every AI/ML model that shapes a business decision. On 2026-08-12 Governor Malhotra told banks they "cannot hide behind algorithms", requiring AI inventories, explainability and board-approved governance. The DPDP Act and RBI require key financial data to stay in India. — [AccuKnox](https://accuknox.com/blog/rbi-model-risk-management-ai-security); [Indoneo](https://www.indoneo.com/capital/india-rbi-ai-lending-governance-malhotra/); [mindzie](https://mindzie.com/2025/02/11/think-global-act-local-how-data-residency-regulations-shape-banks-use-of-large-language-models/)

**6. Mortgage**
- Balerion AI: $6M seed, agentic loan-file handling from application to close. Authentive uses **browser agents** integrated with Encompass. Encompass, BytePro and MeridianLink "remain foundational". ICE (the Encompass owner) unveiled its own AI voice/chat agents for servicing in 2026. — [NMP](https://nationalmortgageprofessional.com/news/balerion-ai-raises-6m-automate-mortgage-origination-process); [Forbes 2026-06-18](https://www.forbes.com/sites/josipamajic/2026/06/18/ai-is-rewriting-mortgage-origination-and-vcs-are-starting-to-notice/); [ICE IR](https://ir.theice.com/press/news-details/2026/ICE-Unveils-AI-Voice-and-Chat-Agents-for-Mortgage-Servicing-at-ICE-Experience-2026/default.aspx)

**7. Accounting firms and desktop tax software**
- Basis: $100M Series B at $1.15B (Accel, 2026-02-24), $138M total, used by ~30% of the top 25 accounting firms. It claims the first autonomous end-to-end 1065 return. — [CPA Practice Advisor](https://www.cpapracticeadvisor.com/2026/02/24/basis-raises-100-million-to-deploy-ai-agents-for-accounting-firms/178759/); [SiliconANGLE](https://siliconangle.com/2026/02/24/ai-accounting-startup-basis-secures-100m-1-15b-valuation-firms-adopt-agent-based-workflows/)
- Juno (CPA-founded tax-return AI): $12M. — [Crunchbase News](https://news.crunchbase.com/fintech/cpa-founded-ai-tax-return-startup-juno-seed-funding/)
- "Lacerte and UltraTax are not modern cloud applications with open APIs — they are desktop-installed software with complex interfaces built over decades". A firm's own assessment of 2026 AI tax prep was "No one is ready. No one." — [SynergyIQ](https://synergyiq.net/blog/blog-cpa-cch-axcess-ultratax-lacerte-scale-limitations.html); [WCG](https://wcginc.com/blog/ai-tax-prep-in-2026-not-quite-there-yet/)
- Labor: CPA exam first-time candidates fell to 27,994 in 2024, the fewest since tracking began in 2008 (NASBA), but 2025 was the highest since 2018. "300,000 accountants left the profession in the last five years" (widely repeated, secondary). — [Atlas CPA Index](https://atlascpaindex.com/news/cpa-exam-candidate-pipeline-2024-2025-data); [ACCWire](https://www.accwire.com/accountant-shortage/)

**8. Pharma and life-sciences quality (GxP)**
- Katalyze AI: $10.5M seed, an agentic OS for pharma manufacturing. — [ERP Today](https://erp.today/katalyze-ai-pharma-manufacturing-agentic-ai-funding)
- Modicus Prime: $8M total ($4.5M, May 2026) for AI audit-readiness in GxP. — [PR Newswire](https://www.prnewswire.com/news-releases/modicus-prime-raises-8m-to-enable-ai-audit-readiness-across-pharma-302763769.html)
- No startup found that runs GUI agents *inside* validated systems (LIMS, QMS, MES, SAP QM) as a product (see Q2).

**9. Government and public-sector case work**
- Code for America and Anthropic (announced 2026-05-08) are piloting a Claude-based agent that fills benefit applications from agency systems, with caseworker review and approval. — [Nextgov](https://www.nextgov.com/artificial-intelligence/2026/05/anthropic-and-nonprofit-partner-streamline-benefits-administration-ai/413455/); [Route Fifty](https://www.route-fifty.com/artificial-intelligence/2026/03/open-source-ai-assistant-shows-promise-california-caseworkers-service-delivery/412378/)
- Integrations involve "ETL from 5+ legacy systems including SNAP, Medicaid, tax records, and employment verification". — [The Neural Base](https://theneuralbase.com/ai-for-government/learn/beginner/benefits-eligibility/) (educational content, low authority)

**10. Legal (court e-filing, docketing)**
- Capital concentrates in research and drafting: Harvey reportedly raised $550M at $15.5B (Sept 2026, per a secondary blog), and Wordsmith raised a $70M Series B (June 2026, in-house front door). No funded startup focused specifically on court e-filing or docketing was found. — [Lawyerd](https://lawyerd.net/blog/top-legal-tech-startups-2026/)

**11. Auto dealerships (DMS: CDK, Reynolds)**
- Funding is voice/sales-centric: Toma $17M, Numa $55M, STELLA $34.7M (Reynolds is a strategic investor). — [Ringlead guide](https://www.ringlead.ca/blog/dealership-ai/ai-phone-agents-dealerships-2026/); [Thoughtly](https://thoughtly.com/blog/best-ai-phone-agents-for-automotive-dealerships-2026)

**12. Manufacturing ERP (SAP GUI)**
- "SAP GUI doesn't have a DOM; it's a native Windows application with its own rendering engine". Conduct (ex-Palantir founders) builds an AI layer over customized SAP environments. — [chatgpt.ca guide](https://chatgpt.ca/blog/automate-legacy-desktop-apps-ai-agents); [Startup House](https://startup-house.com/blog/integrating-ai-legacy-systems-modernization-strategy)

### Inferences
- **Healthcare RCM and prior auth is a trap for a small team, despite having the largest documented spend** ($43B/yr collections effort; a $21B CAQH automation gap). At least seven companies have raised $38M-$120M rounds in the last 15 months, and CMS-0057-F moves the payer end to FHIR APIs from 2027. Flowera's differentiation there would have to be "self-hosted infrastructure sold *to* these startups" (Minicor's positioning), not a vertical app.
- **Accounting desktop tax software is the most GUI-bound open niche found.** Lacerte, UltraTax, ProSeries and Drake are Windows desktop apps, often hosted on Citrix or RDS by firms. Basis's $100M targets the top-25 firms and cloud or structured workflows. The ~40,000+ small and mid-size US firms on desktop tax software, often with client data they are nervous about sending to cloud LLMs, look under-served. Tax data (IRC §7216 consent rules, IRS Pub 4557 safeguards) plausibly favors local processing. That is an inference: §7216 was not researched in this session.
- **Customs brokerage is under-funded relative to its regulatory and data-sensitivity profile** (all rounds found are ≤$4.5M). Brokers work in ABI/ACE-certified desktop software (e.g., CargoWise and others; not verified in this session) plus government portals. Amberd's private-LLM pitch shows buyers already respond to data-ownership arguments.
- **Healthcare practice-management long tail** (dental/DSO on Dentrix/Eaglesoft, home health/hospice, behavioral health) is served by vendors doing voice, insurance verification and replacement EHRs, not by GUI agents inside the incumbent desktop PMS. The replace-the-EHR entrants (Enzo) show the buyer pain, and their rip-and-replace path is slower than an overlay.
- **Freight brokerage is crowded and mostly channel work** (email, voice, load boards), where GUI driving is a minor share. Pallet's "portal updates" is the GUI slice and is already served.
- **Banking core back office is being claimed by the core vendors themselves** (Fiserv agentOS, FIS with Anthropic). A startup would sell *around* them, e.g. to Indian or Gulf banks bound by data localization, not into the US core.
- The horizontal "record once, agent runs it" category (Sola, Automat, Minicor, Twin; Orby absorbed by Uniphore) is funded and moving fast. Flowera's self-hosted, local-model, audit-first posture is the only axis on which none of them lead publicly: all found are cloud-first or unspecified, and Minicor alone mentions on-prem.

### Gaps
- No reliable 2026 labor-spend figures found for: freight brokerage back office, customs brokerage (number of licensed brokers, entries per year), credit-union back office, mortgage processing cost per loan, government caseworker headcount, or accounting-firm tax-prep hours.
- No quantified "share of work in GUI-only systems" exists for any vertical in the sources found. Every such share would be an estimate.
- ARR for most startups was not disclosed. Only Latent (>$20M annualized, inferred attribution) and Corgi (>$40M) surfaced.
- Funding for Minicor, Amari AI and Conduct was not found. Commure's valuation comes from a tracker blog, not a primary release.
- No funding data found for utilities or telecom back-office GUI agents. Those verticals were not researched in depth.

---

## Q2. Where regulation rewards a demonstration-recorded, deterministic, replayable, auditable agent

### Takeaway
Yes, in specific places. FDA's final Computer Software Assurance guidance (Sept 2025) and 21 CFR Part 11 explicitly require validated, risk-based assurance and secure, independent, time-stamped audit trails of operator actions. A deterministic replay of a human-approved demonstration, with before/after screenshots, is far easier to validate under CSA/GAMP 5 than a free-roaming LLM agent. That makes GxP life-sciences operations the clearest case where the product's architecture is itself the regulatory unlock. Secondary tailwinds: RBI model-risk guidance (June 2026), EU AI Act record-keeping (high-risk obligations deferred to Dec 2027; Art. 50 transparency live Aug 2026), and HIPAA/SOX audit expectations. These favor auditability but do not *require* determinism.

### Cited Findings
- FDA's CSA guidance for production and quality-system software was **finalized in September 2025**. It is risk-based and least-burdensome, focuses assurance on functions critical to product quality and patient safety, and leverages vendor data for lower-risk functions. — [FIVE Validation](https://fivevalidation.com/final-fda-computer-software-assurance-guidance/); [IntuitionLabs](https://intuitionlabs.ai/articles/fda-csa-guidance-pharma). One source says "final guidance (Feb 2026)", which conflicts on the date, so Sept 2025 is taken as the finalization. — [IntuitionLabs CSA vs CSV](https://intuitionlabs.ai/articles/csa-vs-csv-validating-ai-systems-fda-guidance)
- AI-enabled components in manufacturing, quality, clinical and lab systems should be validated with the same risk-based CSA logic under GAMP 5. — [Ideagen](https://www.ideagen.com/thought-leadership/blog/ai-enabled-systems-and-csa--future-proofing-your-gxp-compliance)
- For AI agents in GxP, audit logs "should capture the sequence of inputs, decisions, and tool calls that produced the output, recorded independently of the agent itself, consistent with ALCOA+". — [Trussed.ai / search synthesis](https://trussed.ai/resources/ai-governance-for-gxp-systems-validation-and-audit-guide); PHUSE US 2026 paper on validating agentic AI in GxP clinical data environments — [lexjansen.com PHUSE 2026](https://www.lexjansen.com/phuse-us/2026/ML/PAP_ML04.pdf)
- 21 CFR 11.10(e) requires "use of secure, computer-generated, time-stamped audit trails to independently record the date and time of operator entries and actions that create, modify, or delete electronic records". 11.10(a) requires validation of systems, and 11.10(f) operational system checks to enforce permitted sequencing of steps. — [eCFR 21 CFR 11.10](https://www.ecfr.gov/current/title-21/chapter-I/subchapter-A/part-11/subpart-B/section-11.10) (not re-fetched this session: the redirect was blocked)
- EU AI Act: Digital Omnibus provisional agreement (2026-05-06) defers Annex III high-risk obligations to **2027-12-02** and Annex I to **2028-08-02**. Art. 50 transparency still applies from 2026-08-02. The high-risk regime includes record-keeping, human oversight and logging. — [Gibson Dunn](https://www.gibsondunn.com/eu-ai-act-omnibus-agreement-postponed-high-risk-deadlines-and-other-key-changes/); [Pinsent Masons](https://www.pinsentmasons.com/out-law/news/rules-high-risk-ai-delayed-under-eu-omnibus-deal)
- RBI's MRM guidance (2026-06-24) covers every AI/ML model that shapes a business decision, and the Governor demanded AI inventories, explainability and board governance (2026-08-12). — [AccuKnox](https://accuknox.com/blog/rbi-model-risk-management-ai-security); [Indoneo](https://www.indoneo.com/capital/india-rbi-ai-lending-governance-malhotra/)
- Rocket Software's Sept 2026 mainframe launch is branded "governed, auditable AI agents". Governance and audit are now the incumbent's headline message in regulated legacy environments. — [GlobeNewswire 2026-09-23](https://www.globenewswire.com/news-release/2026/09/23/3367450/0/en/rocket-software-advances-governed-agentic-ai-on-the-mainframe-with-rocket-eva.html)
- In government benefits, the reference architecture stresses a human sign-off gate and WORM audit before determinations (an open-source reference implementation, not a regulation). — [GitHub benefits_eligibility_agent](https://github.com/virtualryder/benefits_eligibility_agent)

### Inferences
- **GxP is where "deterministic replay + audit" is a structural unlock, not just a feature.** A replay compiled from a recorded demonstration is a fixed, versioned script. It can be validated once (IQ/OQ/PQ-lite under CSA), and any deviation (self-heal, escalation) is a logged, reviewable event. That maps directly onto Part 11 audit trails and 11.10(f) sequencing checks. A free-form LLM agent is hard to validate because its behavior is not fixed. Flowera's healing ladder (heal → engine → person) needs a "validated mode" where a heal or engine choice pauses for human approval, or is logged as a deviation, to keep this property.
- The EU AI Act's back-office relevance is modest: most back-office automation is not Annex III high-risk, and the deadlines have slipped to 2027-28. It supports a message ("audit-ready") but is not a forcing function in 2026.
- Plausible tailwinds, not verified this session: SOX (internal controls over financial reporting processes touched by bots) and HIPAA Security Rule audit controls (45 CFR 164.312(b)).

### Gaps
- No evidence found of any regulator *specifically* endorsing record-and-replay agents, or of a pharma company validating a GUI agent inside a Part 11 system in production.
- No market sizing for GxP back-office labor (QA, batch record review, deviation/CAPA data entry) was found.
- HIPAA 164.312(b) and SOX were not researched in this session.

---

## Q3. Mainframe, green-screen and Citrix-delivered apps: size and who addresses them

### Takeaway
Mainframe work is being attacked from two sides. Incumbents like Rocket Software (EVA, Sept 2026) and IBM own ops and modernization. Seed-stage startups like Hypercubic (YC F25, $5.3M seed, Aug 2026) do modernization and "interact with mainframe terminals". The *operational data-entry* layer (clerks working 3270/5250 screens through Citrix) is still addressed mainly by legacy RPA over HLLAPI, and by a new wave of Windows-VM/Citrix agents (Minicor, Automat). No clean market size for "green-screen automation" was found.

### Cited Findings
- Hypercubic: $5.3M seed led by CIV, with YC, Afore, Multimodal and Pioneer (Axios, 2026-08-18). Ex-Apple AIML founders build agentic tools that "read legacy code, interact with mainframe terminals, and reconstruct system knowledge". Focus: modernization. — [Axios](https://www.axios.com/pro/enterprise-software-deals/2026/08/18/hypercubic-seed-round-modernize-mainframe-software); [Pulse 2.0](https://pulse2.com/hypercubic-raises-5-3-million-seed-to-turn-mainframe-modernization-into-a-software-driven-process/)
- Rocket Software expanded Rocket EVA (2026-09-23), "governed, auditable AI agents on the mainframe" for operational tasks. A Hanover Research study for Rocket found 81% of financial-services IT leaders report a very or extremely significant mainframe skills gap, and 87% expect AI to help within two years. — [GlobeNewswire](https://www.globenewswire.com/news-release/2026/09/23/3367450/0/en/rocket-software-advances-governed-agentic-ai-on-the-mainframe-with-rocket-eva.html); [SiliconANGLE](https://siliconangle.com/2026/09/23/rocket-software-brings-governed-ai-agents-to-mainframe-operations/)
- Green-screen RPA has relied on IBM's HLLAPI through terminal emulators for 30+ years. — [Rocket RPA PDF](https://www.rocketsoftware.com/sites/default/files/resource_files/mainframe-robotic-process-automation.pdf)
- Minicor runs automations "behind a Citrix login" on Windows VMs. Automat markets SAP and Citrix automation without APIs. — [YC launch](https://www.ycombinator.com/launches/Pkq-minicor-self-healing-desktop-automations-that-scale); [Automat](https://runautomat.com/blog/automate-sap-citrix-without-apis)
- UiPath (the incumbent for Citrix/mainframe RPA) is growing slowly: $37M net new ARR in Q2 FY27 on a $1.938B base. — [UiPath IR](https://ir.uipath.com/news/detail/461/uipath-reports-second-quarter-fiscal-2027-financial-results)

### Inferences
- Citrix and RDS are a pixel-only surface: no AT-SPI, no DOM. Flowera's current visual anchoring (`visual.py`, tuned on Chromium pixels only) would be the whole grounding stack there. The CLAUDE.md notes GTK/LibreOffice and other toolkits are unmeasured, so Citrix/3270 is an engineering bet, not a current strength. A 3270/5250 emulator with a text-screen API (s3270/tn5250j-style screen scraping) would give deterministic, text-exact grounding, and could be a cheap, defensible addition for insurance, banking and government green screens.
- Modernization (Hypercubic, Rocket, IBM watsonx Code Assistant for Z) is a different buyer (IT) from operational back-office automation (operations). Flowera fits the operations buyer.

### Gaps
- No credible market size for mainframe/green-screen or Citrix-specific automation was found. Aggregator RPA numbers vary by 10x ($3.6B Gartner software vs $35B Precedence).
- The share of insurance and banking clerical work done on 3270/5250 screens was not quantified.

---

## Q4. Air-gapped, classified and on-prem-only buyers: demand for local-model agents

### Takeaway
Demand is real and rising: sovereign AI sits at peak priority on Gartner's 2025 government hype cycle, federal agencies with AI contracts rose from 17 (2022) to 28 (2026), and Indian regulation (DPDP, RBI MRM June 2026) and brokers' data-ownership concerns push toward private deployment. Supply is fragmented: platform vendors (Katonic, Seekr, VDF.ai, 6clicks, ibl.ai) sell generic air-gapped LLM or agent platforms, not GUI agents that learn from demonstration. Selling into classified US defense is slow and accreditation-heavy (IL5/IL6, FedRAMP High) for a small team. Regulated commercial buyers in India, the EU and the Gulf, and self-hosting mid-market firms, are nearer-term.

### Cited Findings
- Sovereign cloud market projected at $195.35B in 2026 (vendor-cited forecast). Federal agencies holding AI contracts rose from 17 in 2022 to 28 in 2026. Gartner predicts ≥80% of governments will deploy AI agents for routine decision-making by 2028, and sovereign AI reached peak priority on the 2025 Gartner Hype Cycle for government services. — [Coder blog](https://coder.com/blog/ai-agent-security-sovereignty-defense); [ibl.ai](https://ibl.ai/blog/federal-agencies-sovereign-ai-infrastructure-2026)
- "Defense, intelligence, and critical-infrastructure operators often cannot use cloud AI APIs at all". Air-gapped requirements are set out by Seekr, 6clicks and Katonic (vendors). — [Seekr](https://www.seekr.com/resource/air-gapped-ai-requirements-sovereign-classified/); [6clicks](https://www.6clicks.com/resources/blog/deploying-ai-agents-in-classified-and-air-gapped-environments?hs_amp=true); [Katonic](https://www.katonic.ai/blog/air-gapped-ai)
- Counterpoint: in defense, sovereignty "no longer means air-gapped infrastructure". The threat model shifts to agent over-permissioning and credential compromise. — [Coder](https://coder.com/blog/ai-agent-security-sovereignty-defense)
- India: DPDP plus RBI require key financial data to stay in India. Many cloud AI services do not meet localization requirements, which restricts banks' use of external GenAI. — [mindzie](https://mindzie.com/2025/02/11/think-global-act-local-how-data-residency-regulations-shape-banks-use-of-large-language-models/); on-prem LLM vendors pitch banks directly — [Abacus](https://goabacus.co/blog/complete-guide-on-premise-ai-banks)
- Customs: Amberd's private-LLM, broker-owned-cloud architecture. — [SupplyChainBrain](https://www.supplychainbrain.com/articles/44567-private-ai-reshapes-customs-brokerage-under-a-regulatory-whiplash)

### Inferences
- Local models are a hard constraint for Flowera. On CPU hosts the product is slow (CLAUDE.md measures minutes-long cold loads on an 8 GB host). This makes **deterministic replay the enabler of air-gapped deployment**: a taught skill runs with no model at all (`/skill`, `replay.run`), and the model is only consulted on escalation. Buyers who cannot use cloud LLMs and cannot afford GPU fleets are best served by "model-free execution, model-assisted exceptions". This is a genuine, articulable differentiator against Sola, Automat and Twin (cloud, model-heavy).
- For a small team, classified US defense is likely a year-plus accreditation effort. Faster paths are Indian and Gulf banks/NBFCs and EU regulated mid-market via partners/SIs, and US mid-market firms that simply want client data kept on their own hardware (CPA firms, customs brokers, dental DSOs).

### Gaps
- No quantified demand data (surveys, RFP counts) for *local-model GUI agents* specifically.
- Gulf (UAE/Saudi) data-residency rules for banking AI were not researched this session.

---

## Q5. Ranked verticals for a small, technical, self-hosted team

### Takeaway
Rank (most to least attractive):
1. Accounting-firm desktop tax and bookkeeping operations
2. GxP life-sciences quality/regulatory data operations
3. Customs brokerage entry and compliance work
4. Healthcare practice-management long tail (dental/DSO, home health/hospice, behavioral health)
5. Regulated banks/NBFCs outside the US (India/Gulf) needing on-prem agents
6. P&C insurance green-screen/policy-admin operations
7. Government benefits case work
8. Mortgage
9. Hospital RCM/prior auth
10. Freight brokerage

The first four combine GUI-only desktop systems, a data-sensitivity or audit reason to self-host, and small or no funded GUI-native competition.

### Cited Findings (the evidence each rank rests on is cited in Q1-Q4; summarized here)
1. **Accounting / desktop tax (Lacerte, UltraTax, ProSeries, Drake).**
   - Why it fits: the software is desktop-installed with no open APIs ([SynergyIQ](https://synergyiq.net/blog/blog-cpa-cch-axcess-ultratax-lacerte-scale-limitations.html)), and practitioners judge 2026 tools not ready end to end ([WCG](https://wcginc.com/blog/ai-tax-prep-in-2026-not-quite-there-yet/)). The labor shortage is acute ([Atlas CPA Index](https://atlascpaindex.com/news/cpa-exam-candidate-pipeline-2024-2025-data)).
   - Competition: Basis ($1.15B) targets top-25 firms ([CPA Practice Advisor](https://www.cpapracticeadvisor.com/2026/02/24/basis-raises-100-million-to-deploy-ai-agents-for-accounting-firms/178759/)).
   - Unclaimed niche: 50-500-person regional firms on desktop tax software hosted on Citrix/RDS. The work is data entry from source documents into the return, clearing diagnostics, and e-file status checks. The record-once model fits seasonal staff showing the agent once.
2. **GxP quality and regulatory operations.**
   - Why it fits: CSA is final (Sept 2025), and Part 11 audit-trail requirements match the product's recording and approval gates ([FIVE Validation](https://fivevalidation.com/final-fda-computer-software-assurance-guidance/); [eCFR](https://www.ecfr.gov/current/title-21/chapter-I/subchapter-A/part-11/subpart-B/section-11.10)).
   - Competition: startups are small ($8-10.5M) and none drives GUIs ([ERP Today](https://erp.today/katalyze-ai-pharma-manufacturing-agentic-ai-funding); [PR Newswire](https://www.prnewswire.com/news-releases/modicus-prime-raises-8m-to-enable-ai-audit-readiness-across-pharma-302763769.html)).
   - Unclaimed niche: validated data entry and transfer between LIMS/QMS/ERP (e.g., CoA transcription, deviation/CAPA logging, batch record reconciliation) at CDMOs and mid-size pharma/medtech. Validation (IQ/OQ/PQ) is a slow sale but sticky, and the evidence package is itself a moat.
3. **Customs brokerage.**
   - Why it fits: tariff churn drives volume, buyers value data privacy ([SupplyChainBrain](https://www.supplychainbrain.com/articles/44567-private-ai-reshapes-customs-brokerage-under-a-regulatory-whiplash)), and every funded player has raised ≤$4.5M ([Pax AI](https://www.paxai.com/blog/pax-ai-raises-4-5m-seed-funding-duty-drawback-ai-tariff-refunds); [GingerControl](https://www.trysignalbase.com/news/funding/gingercontrol-secures-21m-seed)).
   - Unclaimed niche: keying entries and status into ABI desktop software and government portals for small and mid-size brokers. The licensed broker's liability favors approval gates and an audit trail.
4. **Healthcare practice-management long tail.**
   - Competition: incumbents' AI partners do voice, imaging and verification, while new entrants replace the EHR ([Mentera](https://mentera.ai/blog/dental-pms-comparison-ai-layer-integration); [Enzo](https://www.prnewswire.com/news-releases/enzo-health-launches-first-agentic-ehr-for-home-health-connecting-referral-to-reimbursement-in-one-system-302786174.html)).
   - Why it fits: HIPAA favors self-hosting.
   - Risk: Luminai, Magical and Tennr could move down-market.
5. **Non-US regulated banks (India first).** RBI MRM (June 2026) plus DPDP localization ([AccuKnox](https://accuknox.com/blog/rbi-model-risk-management-ai-security)). A long enterprise sale is best done through an Indian SI partner.
6. **P&C insurance ops.** Large pool of legacy cores ([Adacta](https://blog.adacta-fintech.com/state-of-insurance-legacy-system-modernization)), but underwriting and claims AI is well funded (Sixfold with Guidewire as investor, Federato $100M), and green screens need a new 3270/5250 grounding layer.
7. **Government benefits.** A real need, and Anthropic plus Code for America are already piloting ([Nextgov](https://www.nextgov.com/artificial-intelligence/2026/05/anthropic-and-nonprofit-partner-streamline-benefits-administration-ai/413455/)). Procurement cycles are hostile to a small team.
8. **Mortgage.** Rate-cycle sensitive, and ICE (the Encompass owner) is shipping its own agents ([ICE IR](https://ir.theice.com/press/news-details/2026/ICE-Unveils-AI-Voice-and-Chat-Agents-for-Mortgage-Servicing-at-ICE-Experience-2026/default.aspx)).
9. **Hospital RCM/prior auth.** Biggest money, most crowded, and a FHIR mandate from 2027 ([RevCycleAI](https://revcycleai.com/deals-and-raises/); [CMS](https://www.cms.gov/newsroom/fact-sheets/cms-interoperability-prior-authorization-final-rule-cms-0057-f)).
10. **Freight brokerage.** HappyRobot $1.2B, Loop $95M, Pallet $50M total, and the work is mostly email/voice ([Forkast](https://forkast.news/happyrobot-wants-1-2-billion-to-run-your-freight-by-voice/); [BusinessWire Loop](https://www.businesswire.com/news/home/20260417578056/en/Loop-Raises-$95M-Series-C-to-Scale-Its-AI-Platform-Across-the-Supply-Chain)).

### Inferences
- **Ranking criteria**, weighted for a small self-hosted team:
  - Is the system of record a GUI-only desktop/Citrix app? (Flowera's native strength)
  - Does the buyer have a reason to self-host or demand an audit trail? (moat against cloud-first Sola, Automat, Twin)
  - How much funded GUI-native competition is there? (fewer is better)
  - Can a 2-5-person team sell into it (mid-market, partner channels)?
  - Is the work repetitive enough for record-once/replay to cover most volume without a large model? (fits local CPUs)
- **Alternative horizontal play:** "Minicor-for-regulated/self-hosted", i.e. infrastructure that vertical AI startups (the RCM and freight companies above) embed to reach on-prem customers' desktop apps. This reuses the crowded verticals' money without competing with them. Minicor, a YC P26 company, occupies this position for cloud Windows VMs, and Flowera's differentiation there would be self-hosting, local models and audit.
- **Product gaps these wedges imply** (from CLAUDE.md, not external sources):
  - Windows-native apps inside the VM: today they run on the host via winhost, and that path is Windows-host only.
  - Citrix/RDS pixel grounding: `visual.py` has been tuned only on Chromium pixels.
  - A 3270/5250 text-screen driver.
  - A "validated mode" for GxP that turns self-heal into a logged deviation needing approval.
  - Accounting and tax desktop apps are Windows-only, so the winhost path, or a Windows guest VM, is on the critical path for wedge #1.

### Gaps
- No bottom-up TAM figures (number of firms × labor hours × wage) were computed or found for the top-4 wedges. The report writer should treat those TAMs as unquantified.
- Unverified: whether Basis, Juno or TaxGPT drive Lacerte/UltraTax GUIs directly or rely on import files. Also unverified: CargoWise/ABI desktop prevalence among small customs brokers.
- Buyer willingness to pay for self-hosting (versus a cloud BAA/SOC 2 vendor) has not been validated by surveys found in this session.
