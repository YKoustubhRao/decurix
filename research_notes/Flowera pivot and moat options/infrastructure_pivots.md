# "Picks and shovels" pivot options for a computer-use agent company (as of 24 September 2026)

Scope: three possible infrastructure pivots for Decurix/Flowera: (a) human computer-use demonstration/trajectory data and RL environments sold to labs, (b) agent verification, audit, governance and insurance infrastructure, (c) institutional knowledge capture / work-process intelligence. Every figure is dated. **Reported** means a named outlet or the company said it. **Estimate** means an analyst projection or an unverified third party. Company-supplied growth numbers are self-reported unless stated otherwise.

Decurix assets these options would draw on: isolated persistent Linux (XFCE) desktop VMs with Chromium and LibreOffice, and Windows apps shown inside; a recorder that turns XInput2 events into actions, with accessibility-tree elements, before/after frames and privacy redaction; importers for Chrome Recorder, BrowserGym/AgentLab and OpenHands; a deterministic replay engine with an outcome taxonomy (verified, completed_unverified, halted, reconciliation_required, needs_approval, needs_human); an approval gate; the `bench/gui` harness (20 tasks with outcome checks); MiniWoB replay; SKILL.md export; and a measured autonomy rate.

---

## Q1. (a) Data and RL environments: how big is lab spending, who sells, at what prices, is there demand for desktop/enterprise-app environments, and what are the risks?

### Takeaway
Demand is real and large. In September 2025 The Information reported that Anthropic had discussed spending more than $1B on RL environments over the following year, and the human-data vendors next door now run at $0.5–2B gross each. Prices are well documented: about $20k per "UI gym" website clone, about $300k for a high-fidelity clone of a complex product like Slack, and $200–2,000 per task. Enterprise and desktop workflows are named as the next growth area. The structure is hostile to a small entrant, though: labs are moving work in-house, Anthropic is reportedly pushing a standard sandbox spec so vendors can be swapped, most environment startups have only 1–3 customers, the big data foundries are buying the specialists (Mercor bought Deeptune), and pretraining on unlabeled video is starting to replace human demonstrations.

### Cited Findings

**Spending and market size**
- **Reported, Sept 2025:** The Information reported that Anthropic leaders had discussed spending more than $1B on RL environments over the next year. a16z's Jennifer Li: "All the big AI labs are building RL environments in-house… [but] are also looking at third-party vendors." — [TechCrunch, 21 Sep 2025](https://techcrunch.com/2025/09/21/silicon-valley-bets-big-on-environments-to-train-ai-agents/)
- **Reported (Epoch interviews, Jan 2026):** contracts are "often six to seven figures per quarter". One neolab researcher saw contracts of $300k–$500k. RL spending is still "a fraction of compute costs": OpenAI's 2026 R&D compute is projected at about $19B. — [Epoch AI, "An FAQ on RL Environments", 12 Jan 2026](https://epoch.ai/gradient-updates/state-of-rl-envs)
- **Estimate (Jan 2026):** Anthropic is "estimated to be spending on the order of tens of millions annually" with coding and computer-use environment vendors, and is likely to grow that 3–5x into 2026. OpenAI "has already signed multiple seven-figure RL-environment and human-data contracts". — [Data Gravity (Chris Zeoli), 29 Jan 2026](https://www.datagravity.dev/p/rl-environments-for-agentic-ai-who)
- **Estimate, secondary source:** SemiAnalysis has "reportedly estimated" the combined RL-environment market at about $1B in annual revenue as of mid-2026. This comes through a secondary summary that says it did not verify the figure. — [Zylos Research, 15 Jul 2026](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- **Estimate:** traditional labeling is "roughly a $5B market today (growing in excess of 50% YoY)". Data labeling has "roughly three $1B+ revenue players (Scale, Mercor, Surge) and more than five $100M+ players (Turing, Micro1, Invisible, Handshake, etc.)". — [Data Gravity, Jan 2026](https://www.datagravity.dev/p/rl-environments-for-agentic-ai-who)
- **Estimate, conflicting:** Mordor puts the AI data-labeling market at $2.32B in 2026, rising to $6.53B in 2031. Definitions vary widely between research firms, so this should not be compared directly with the vendor run-rates below. — [Pebblous, Jul 2026](https://blog.pebblous.ai/report/expert-data-labor-market-2026/en/)

**Prices**
- **Reported:** UI gyms (website clones) cost about $20,000 per website, and "OpenAI has purchased hundreds of sites for ChatGPT Agent training". These are one-time purchases, reused for future models, and their trajectories are fed back into mid-training. Other vendors clone Slack, Salesforce, AWS terminals, OneDrive, Gmail, Discord and Atlassian. — [SemiAnalysis, 6 Jan 2026](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science)
- **Reported (interviews):** a higher-fidelity replica of a complex product like Slack costs about $300k. Tasks cost "$200 to $2000 mostly; $20k per task would be rare". Mechanize estimates about $2,400 of compute is spent per task during RL. Exclusive deals cost about 4–5x more than non-exclusive ones. — [Epoch AI, Jan 2026](https://epoch.ai/gradient-updates/state-of-rl-envs)
- **Reported (company):** Prime Intellect's Environments Hub pays bounties of about $100–500 for open-access environments and about $1,000–5,000+ for application-only ones. — [Zylos, Jul 2026](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- **Reported:** expert contractor rates: Surge $50–200/hr for frontier experts, Mercor about $85/hr on average and $200+ for seniors, Handshake AI $100–125/hr. — [Pebblous, Jul 2026](https://blog.pebblous.ai/report/expert-data-labor-market-2026/en/)
- **Reported:** "Frontier labs pay $5,000+ for a single decent coding task." — [Refolk, 26 Jul 2026](https://www.refolk.ai/blog/rl-environments-talent-pool-bespoke-labs)

**Incumbent human-data vendors (revenue and valuation)**
- Mercor: $2B annualized *gross* revenue run rate, crossed per CEO Brendan Foody in July 2026. Last confirmed valuation $10B (Series C, $350M, Oct 2025). In talks at $20B in July 2026, not closed. Bought RL-environment startup Deeptune (enterprise-app recreations "from spreadsheets to Salesforce"; had raised a $43M Series A led by a16z in March 2026). Suffered a data breach and contractor lawsuits in April 2026, and Meta paused work with it. — [RuntimeWire, 9 Jul 2026](https://runtimewire.com/article/mercor-20b-valuation-talks-deeptune-acquisition); [Sacra](https://sacra.com/c/mercor/)
- Handshake AI: about $1B gross annualized revenue in April 2026, up from $550M in January 2026 and about $5M a year earlier. — [The Information via TEXXR, 14 Apr 2026](https://texxr.com/1166950/handshake-reaches-1b-annualized-revenue)
- Micro1: $500M gross run rate in August 2026, up from $100M eight months earlier. It keeps about 60–70% of gross, so net is about $150–200M. Off-the-shelf data sold to several customers reportedly earns 80–90% gross margin. — [TechCrunch, 20 Aug 2026](https://techcrunch.com/2026/08/20/ai-data-startup-micro1-reaches-500m-gross-run-rate-amid-ai-training-boom/)
- Surge AI: over $1B revenue in 2024 while bootstrapped (Reuters). In July 2025 it sought up to $1B at a $15B+ valuation. Later reports cite about $20–25B, but no closed primary round is confirmed. — [Reuters, 1 Jul 2025](https://www.reuters.com/business/scale-ais-bigger-rival-surge-ai-seeks-up-1-billion-capital-raise-sources-say-2025-07-01/); [GetLatka](https://getlatka.com/blog/surgeai-revenue-valuation)
- Scale AI: Meta bought 49% for $14.3B in June 2025 (about a $29B valuation). Afterwards "multiple AI labs mostly stopped contracting Scale". — [SemiAnalysis, Jan 2026](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science)
- Incumbents "arguably have the highest share of the [environments] market". — [Tanay Jaipuria, 10 Feb 2026](https://www.tanayj.com/p/rl-and-the-era-of-experience)

**RL-environment startups (funding)**
- Prime Intellect: $130M Series A in July 2026 at a $1B valuation, with a reported $100M annualized revenue and about 6,000 customers. It runs the open Environments Hub ("Hugging Face for RL environments"). Figures are from company and press reports as relayed by Zylos. — [Zylos, Jul 2026](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- Bespoke Labs: $40M combined seed and Series A (Wing VC and 8VC), disclosed July 2026. HUD: $16M Series A. Mechanize: about $9M angel round (April 2025), reportedly at about a $500M valuation. — [Refolk, Jul 2026](https://www.refolk.ai/blog/rl-environments-talent-pool-bespoke-labs); [Zylos](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- Tracker figures, unverified: AfterQuery about $30.5M, Fleet AI about $15M, Datacurve about $17.7M, Gray Swan about $40M. — [Zylos](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- Computer-use specialists:
  - Fleet AI replicates Salesforce, Excel and browser/desktop workflows.
  - Plato recreates websites and "a full Linux desktop".
  - Matrices raised about $5M (single source) and self-reports "7-figure contracts" with unnamed labs.
  - Halluminate (YC S25) has raised about $160K per PitchBook, has about $1.3M ARR per an unverified GetLatka estimate, and has pivoted to "RL environments for financial services".
  - Habitat (2–10 people) focuses on "code and desktop-style interaction".
  - Source: [RL List directory, 2026](https://www.rl-list.com/); [RL List: Halluminate](https://www.rl-list.com/vendors/halluminate); [RL List: Matrices](https://www.rl-list.com/vendors/matrices)
- Halluminate says it builds "bespoke environments on a per customer basis (think enterprise sites or even full VMs loaded with applications and data)". It names Epic, SAP and Salesforce simulators, and legacy desktop apps such as Epic in healthcare, as a "huge" use case. — [Launch HN, 11 Aug 2025](https://news.ycombinator.com/item?id=44865290)

**Human computer-use demonstration data specifically**
- Markov (YC, Aug 2026) sells "synchronized screen recordings, mouse/keyboard actions, and annotations from real workflows", specialising in expert desktop apps (CAD, architecture, engineering, finance). It claims "15,000+ hours of data sold to frontier AI labs". Price per hour was not disclosed. — [YC Launch, 11 Aug 2026](https://www.ycombinator.com/launches/SaA-markov-data-for-computer-use-ai)
- Markov's open "Computer Use Large" dataset: 48,478 videos, about 12,300 hours (AutoCAD, Blender, Excel, Photoshop, Salesforce, VS Code), under CC-BY-4.0. — [Agent Wars, 14 Mar 2026](https://www.agent-wars.com/news/2026-03-14-markov-ai-releases-48k-screen-recording-dataset-for-training-computer-use-agents)
- OpenTrain markets "trajectory demonstrations" by vetted trainers who work "inside your VMs, sandboxes, staging accounts". — [OpenTrain](https://www.opentrain.ai/solutions/computer-use-agent-training/)
- **Anecdotal, low reliability:** on a podcast, a founder said "several frontier AI labs" approached his company in late 2025 to buy knowledge workers' desktop screen recordings because "there is nowhere on the market you can simply buy work recordings". He also said Meta pushed employee screen recording and met heavy pushback, and that Anthropic bought a video-trained digital-worker startup before launching Cowork. The acquisition claim is unverified. — [Mossfire Insights, 12 Jul 2026](https://insights.mossfire.xyz/en/posts/ep41-screen-recording-rush/)

**Is there demand for desktop and enterprise-app environments, not just web?**
- An RL-environment founder told Epoch: "I think enterprise workflows are going to explode this year… sometimes computer use (e.g. a specific ERP that has no backend)." Environments take the form of MCP tools, Playwright browser tasks, or screenshot-based computer use, often clones of Slack or SAP. One lab researcher warned that "everyone… vibe code[s] a buggy website which isn't useful." — [Epoch AI, Jan 2026](https://epoch.ai/gradient-updates/state-of-rl-envs)
- "Computer use environments… require considerable amount of rote software engineering… This can be slow for the labs to do which is why much of it has been outsourced." — [SemiAnalysis, Jan 2026](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science)
- HUD's tooling wraps any software (a game, a browser, Google Sheets) in a Docker container with an MCP server that exposes `click(x,y)`/`type(text)`. Each task has a prompt, setup and success criteria, and telemetry is kept for training. This is architecturally close to Decurix's `gui` + `bench/gui`. — [SemiAnalysis, Jan 2026](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science)
- Induction Labs' Photon-1 runs online RL on Linux VMs covering "five desktop environments: LXQt, Xfce, MATE, GNOME, and Plasma". This shows labs and neolabs train on full Linux desktops. — [edgeX, 26 Jul 2026](https://pro.edgex.exchange/en-US/news/article/induction-labs-photon-1-trains-ai-agents-from-video)

**Risks**
- **Labs moving work in-house:** "substantially more in-housing" recently; job postings at xAI and Anthropic. — [Epoch AI](https://epoch.ai/gradient-updates/state-of-rl-envs)
- **Vendors made interchangeable:** SemiAnalysis reportedly describes Anthropic working with a dozen or more vendors while pushing a standardized sandbox spec, a strategy that reduces lock-in to any one vendor. Secondary source. — [Zylos](https://zylos.ai/research/2026-07-15-rl-environments-as-a-service-agentic-training-infrastructure/)
- **Customer concentration:** "Almost all companies are seed stage… with less than 20 employees, focused on 1 to 3 customers at most", mostly in exclusive contracts. — [SemiAnalysis](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science). The market is predicted to consolidate to 3–5 winners by 2030. — [Data Gravity](https://www.datagravity.dev/p/rl-environments-for-agentic-ai-who)
- **Talent:** about 1,288 US research engineers have RL and PyTorch skills, roughly 40% of the sampled pool is at Meta or DeepMind, and top expert contractors earn seven figures. — [Refolk, Jul 2026](https://www.refolk.ai/blog/rl-environments-talent-pool-bespoke-labs)
- **Video pretraining replacing human demonstrations:**
  - VideoAgentTrek mined 1.52M steps from 39,000 YouTube tutorials and improved OSWorld-Verified from 9.3% to 15.8%. — [arXiv 2510.19488, Oct 2025](https://arxiv.org/abs/2510.19488)
  - Induction Labs pretrained on about 18 years of public screen video and then needed "fewer than 35,000 computer-use trajectories". — [edgeX, Jul 2026](https://pro.edgex.exchange/en-US/news/article/induction-labs-photon-1-trains-ai-agents-from-video)
- **Low-quality supply:** "There's a large amount of useless bad environments out there." — [Epoch AI](https://epoch.ai/gradient-updates/state-of-rl-envs)

### Inferences
- Decurix's stack lines up closely with what labs buy: a dockerized desktop, a task, an outcome verifier, and full action telemetry. Its differentiators are **real Linux desktop plus LibreOffice plus Windows-app fidelity** (most vendors clone websites), **accessibility-tree-grounded human demonstrations with secrets redacted at the source**, and **importers that normalize BrowserGym/OpenHands trajectories**. `bench/gui`'s 20 tasks with deterministic checks is effectively a small environment pack. At Epoch's $200–2,000 per task, 20 tasks are worth roughly $4k–40k once. The value is in scaling that to thousands of verified tasks on enterprise-like apps.
- The fastest revenue is probably a services-shaped contract with one lab or neolab for desktop and office-suite tasks with verifiers: six figures per quarter within 3–6 months if a buyer relationship exists. Without one, the realistic buyers are the second tier: neolabs, browser/computer-use agent companies, and the data foundries themselves (Mercor, Turing, Handshake), which buy or acquire environment builders.
- A $1B version would be an "environment factory" for enterprise desktop software: thousands of high-fidelity, reusable, composable app environments plus a network of human demonstrators. That is Mercor-plus-Deeptune territory, and it takes lab trust, research depth and heavy capital. The likelier outcome for a small team is acquisition by a data foundry, as with Deeptune.
- Human demonstration hours are the weakest part of (a) to lead with. Markov already gives large datasets away, and video pretraining is lowering the value of labeled demonstrations. Verified environments and tasks, where the reward check is the product, hold value better.

### Gaps
- No public price per hour or per trajectory for human computer-use demonstration data was found. Markov discloses hours sold, not price.
- No independent confirmation of the SemiAnalysis "~$1B market" estimate or of Anthropic's standardized sandbox spec. Both come via Zylos, which says it did not verify them.
- The claim that Anthropic bought a video/digital-worker startup before Cowork (Mossfire podcast) is unverified.
- No lab has publicly named a vendor for desktop or LibreOffice environments specifically. Whether labs pay a premium for Linux-desktop office fidelity over web clones is inferred, not shown.

---

## Q2. (b) Verification, audit, governance and insurance: who buys, who has raised, what regulation forces, and is there a gap for verifiable execution records for computer-use agents?

### Takeaway
This is a hot, well-funded category, and incumbents are buying their way in:
- Zenity raised $125M (Aug 2026).
- Dynatrace agreed to buy Arize for $915M (Aug 2026).
- Cisco agreed to buy Galileo (Apr 2026).
- Check Point bought Lakera (2025).
- AIUC raised $55M in total and runs AIUC-1, now audited by Schellman.
- Dedicated agent insurance exists (Armilla up to $25M limits, Munich Re aiSure via Mosaic).

The regulatory push is slower than planned: the Digital Omnibus moved EU high-risk obligations to 2 December 2027. The revised Product Liability Directive, which treats software as a product, applies from 9 December 2026.

Almost all existing tools target API, SaaS and LLM-trace agents. None found centres on screen-level evidence for computer-use agents, meaning before/after frames, the resolved UI target, approval-gate events and an outcome class that never overstates success. Insurers say what limits them is underwriting data, which is exactly what such records would supply. That is a real but narrow gap, likely to be captured as a feature by an observability, security or certification player unless it is sold into them.

### Cited Findings

**Certification and insurance**
- AIUC raised a $40M Series A (Ribbit Capital, 15 Sep 2026) on top of a $15M seed (NFDG, July 2025), $55M in total.
  - AIUC-1 tests agents against about 5,000 risk and attack combinations, produces a report of about 100 pages, and recertifies quarterly. AI agents run the tests and humans verify the audit.
  - Certified or customer companies include Cursor, ElevenLabs, Harvey, KPMG, Lovable, UiPath and Fin/Intercom.
  - AIUC covers AI-specific risks "up to $50m of losses" through partner insurers.
  - Source: [TechCrunch, 15 Sep 2026](https://techcrunch.com/2026/09/15/early-anthropic-hire-former-metr-coo-have-found-a-way-to-rein-in-rogue-ai-agents/); [SiliconANGLE, 15 Sep 2026](https://siliconangle.com/2026/09/15/ai-agent-certification-startup-aiuc-raises-40m-to-begin-auditing-frontier-models/); [AIUC product page](https://aiuc.com/product)
- AIUC-1 covers six domains: data & privacy, security, safety, reliability, accountability and societal risk.
  - Schellman was accredited as the first auditor (3 Feb 2026), and Coalfire is also listed.
  - Quarterly updates included "Q2-2026: Strengthening MCP security, agent permissions & third-party risk" and "Q1-2026: Detailed evidence requirements".
  - UiPath is a Founding Technical Contributor, "to shape the standard for automation agent security".
  - The Cloud Security Alliance integrated AIUC-1 into its STAR registry.
  - Source: [aiuc-1.com](https://www.aiuc-1.com/)
- Armilla (a Lloyd's coverholder since 2024; standalone product launched April 2025, originally underwritten with Chaucer) expanded to limits of up to $25M per insured. It explicitly covers "failures of AI agents, including incorrect decision-making, improper tool use, and escalation errors", plus defence costs and insurable fines under the EU AI Act and Colorado AI Act. Each policy includes independent AI certification. — [Beinsure, 1 Feb 2026](https://beinsure.com/news/armilla-ai-expands-standalone-ai-liability-cover-to-25m/); [Armilla](https://www.armilla.ai/ai-insurance)
- Munich Re aiSure is performance cover with "parametric-like" claims settled on measured performance. Mosaic has underwritten and distributed it since 26 Feb 2026, with up to EUR/USD/CAD 15M initial capacity. Testudo, a Lloyd's coverholder, launched in January 2026, and HSB is US-distributed. AXA, Allianz and Zurich are "in product development" and have not launched. — [agentinsured.eu tracker, as of 17 Aug 2026](https://agentinsured.eu/articles/european-ai-agent-insurance-market-tracker-june-2026); [Munich Re](https://www.munichre.com/en/solutions/for-industry-clients/insure-ai.html)
- Insurers say the binding constraint is data, not capital: "Carriers cannot price AI agent risk accurately without claims experience." Underwriters ask for pre-deployment testing evidence, a description of human oversight, incident history and certification. — [agentinsured.eu, Aug 2026](https://agentinsured.eu/articles/european-ai-agent-insurance-market-tracker-june-2026)
- **Lower reliability:** one source says exclusions include "Deployments lacking the audit telemetry required by the policy schedule" and "Circumvention of approval gates". This site is itself a pre-launch intermediary, and its policy-schedule citations were not independently verified. — [agentinsured.eu, 14 Apr 2026](https://agentinsured.eu/articles/what-ai-agent-insurance-will-cover)
- ISO generative-AI exclusions, available from 1 Jan 2026, may remove general-liability coverage for GenAI-caused injury and damage. Standalone products take two approaches: Munich Re and Armilla require ongoing performance monitoring, while Testudo requires no pre-deployment assessment. — [Gallagher Re, Mar 2026](https://www.ajg.com/gallagherre/-/media/files/gallagher/gallagherre/news-and-insights/2026/march/rethinking-insurance-for-the-ai-era.pdf)

**Security and governance vendors (funding and M&A)**
- Zenity: $125M Series C led by Norwest (3 Aug 2026), about $185M in total, more than 230 employees. It blocks or modifies agent actions before they execute, based on inferred intent. Gartner (April 2026) called it "the Company to Beat in AI Agent Governance". Norwest cites a projection of agentic AI security growing from $1.65B (2026) to $13.5B (2032), a 42% CAGR. That is an estimate, and its source is not named. — [Zenity](https://zenity.io/company-overview/newsroom/company-news/zenity-raises-125-million-to-secure-the-era-of-1-billion-ai-agents); [Norwest](https://www.norwest.com/blog/the-agent-is-the-new-perimeter-why-were-leading-zenitys-series-c/); [Calcalist](https://www.calcalistech.com/ctechnews/article/b1ahbbcbfe)
- Other raises and deals:
  - Noma Security: about $132M in total, including a $100M Series B (2025).
  - WitnessAI: $58M (Jan 2026).
  - Palo Alto Networks bought Protect AI; SentinelOne, Check Point and CrowdStrike all entered AI security by acquisition.
  - Source: [AI2Work, 11 Aug 2026](https://ai2.work/blog/zenity-lands-125m-series-c-to-police-enterprise-ai-agent-actions)
- AIR came out of stealth with $50M across two seed rounds (Sequoia, Greenoaks, 1 Sep 2026) to vet the skills and tools agents use. Named competitors: Noma, Zenity, Astrix (agent identity), Operant AI. — [TechCrunch, 1 Sep 2026](https://techcrunch.com/2026/09/01/air-raises-50m-to-help-companies-vet-the-skills-and-add-ons-ai-agents-use/)
- Check Point announced the Lakera acquisition in September 2025. Lakera had raised a $21M Series A. — [CRN, 16 Sep 2025](https://www.crn.com/news/security/2025/check-point-to-buy-ai-cybersecurity-startup-lakera-to-boost-agentic-ai-security)
- Demand signals: only 21% of 3,235 leaders report mature agentic AI governance (Deloitte 2026), and Gartner expects more than 40% of agentic AI projects to be cancelled by end of 2027. — [AI2Work, Aug 2026](https://ai2.work/blog/zenity-lands-125m-series-c-to-police-enterprise-ai-agent-actions)

**Observability and evaluation (M&A)**
- Dynatrace agreed to buy Arize for $915M (about $815M in cash) on 13 Aug 2026. It says AI observability is "projected to exceed $10 billion by 2030", an estimate. Arize says it collects "billions of agent trajectory events". — [Dynatrace IR, 13 Aug 2026](https://ir.dynatrace.com/news-events/press-releases/detail/435/dynatrace-to-acquire-ai-observability-leader-arize); [Arize blog](https://arize.com/blog/a-new-chapter-with-dynatrace/)
- Cisco announced its intent to acquire Galileo on 9 Apr 2026, to fold into Splunk's AI Agent Monitoring. Mintlify bought Helicone on 3 Mar 2026. — [Morph, Jun 2026](https://www.morphllm.com/ai-agent-observability-tools); [TechTarget, 13 Aug 2026](https://www.techtarget.com/it-strategy/news/366648773/Dynatrace-acquires-Arize-for-AI-agent-development)
- Braintrust raised an $80M Series B (ICONIQ, 17 Feb 2026). Customers include Ramp, Notion, Replit, Stripe and Zapier. — [Braintrust](https://www.braintrust.dev/blog/announcing-series-b); [ICONIQ](https://www.iconiq.com/growth/insights/partnering-with-braintrust-the-observability-layer-for-ai)
- Pricing examples (July 2026): AgentOps Pro $40/month, Galileo Pro $100/month for 50k traces, Braintrust Pro $249/month. Both AgentOps and Braintrust offer "time travel"/replay-style debugging of agent sessions. — [Morph, Jun 2026](https://www.morphllm.com/ai-agent-observability-tools)

**EU AI Act and liability timeline (enacted law)**
- The Digital Omnibus on AI, Regulation (EU) 2026/1744, has been in force since 27 July 2026. Its path: Parliament vote 16 June (423–57), Council 29 June, signed 8 July, published in the Official Journal 24 July. — [CSA](https://labs.cloudsecurityalliance.org/research/csa-research-note-eu-ai-act-high-risk-deadline-omnibus-20260/); [Oikonomakis Law, 19 Aug 2026](https://oikonomakislaw.com/the-ai-act-after-the-digital-omnibus-what-applies-now-what-was-deferred-and-what-this-means-for-regulated-firms/)
- New dates: Annex III high-risk requirements (employment and workers' management, credit, education, biometrics and others) move from 2 Aug 2026 to **2 Dec 2027**. Annex I product-embedded AI moves to **2 Aug 2028**. The dates are fixed, with no trigger that could bring them earlier. Article 50 transparency, Article 5 prohibitions and GPAI obligations are *not* deferred. — [Bratby Law, 26 Aug 2026](https://bratby.law/digital-omnibus-ai-high-risk-delay/); [EU AI Act Service Desk timeline](https://ai-act-service-desk.ec.europa.eu/en/ai-act/timeline/timeline-implementation-eu-ai-act)
- Cooley lists high-risk AI for public authorities as moved to 2 Aug 2030. — [Cooley, 3 Aug 2026](https://cdp.cooley.com/digital-ai-omnibus-delays-key-deadlines-introduces-new-rules/)
- The revised Product Liability Directive (2024/2853) applies from 9 Dec 2026 and reclassifies software, including AI, as a product for strict liability. It is "the nearest regulatory deadline" for agent liability in Europe. — [agentinsured.eu, Aug 2026](https://agentinsured.eu/articles/european-ai-agent-insurance-market-tracker-june-2026)
- Practitioner advice: conformity assessment and post-market monitoring "cannot be reconstructed for a period during which nothing was recorded". — [Oikonomakis Law](https://oikonomakislaw.com/the-ai-act-after-the-digital-omnibus-what-applies-now-what-was-deferred-and-what-this-means-for-regulated-firms/)

### Inferences
- **The gap is real.** Security vendors (Zenity, Noma, AIR) reason about intent, identity and tool permissions for API and SaaS agents. Observability vendors (Arize, Braintrust, Galileo) record LLM spans. AIUC-1 certifies agents through red-teaming. None of the sources describes a product that proves *what a computer-use agent did on a screen*: before/after frames, the UI element acted on, whether an approval gate fired, and an outcome class that separates "verified" from "completed_unverified" from "reconciliation_required". Decurix's outcome taxonomy and approval-gate audit trail are unusually close to what insurers (performance-measured cover) and auditors (evidence requirements, quarterly recertification) need.
- **The gap is narrow and likely to become a feature.** Buyers (CISOs, risk and compliance) buy platforms. Incumbents are consolidating by acquisition: Dynatrace/Arize, Cisco/Galileo, Check Point/Lakera, Palo Alto/Protect AI. A standalone "computer-use flight recorder" probably ends up as an OEM component or acquisition target for an observability, security, RPA (UiPath is an AIUC-1 contributor) or certification player, rather than a $1B company alone.
- **Fastest revenue:** sell evidence and testing, not a platform. Options: (1) a computer-use test harness and evidence pack that helps agent vendors pass AIUC-1 or Armilla underwriting, sold per certification cycle (quarterly recertification makes it recurring); (2) partner with an insurer or MGA as its telemetry and underwriting-data provider for computer-use/RPA agents. Both need 1–2 design partners and could produce revenue in about 3–6 months. The EU deadlines now arrive in December 2026 (PLD) and December 2027 (Annex III), so compliance urgency in the EU is 2027, not 2026.
- **The $1B version** is "the black box recorder and underwriting data layer for autonomous computer use", mandated by insurers and standards the way SOC 2 evidence tools (Vanta, Drata) rode SOC 2. It depends on computer-use agents becoming a large insured risk class, and on a standard such as AIUC-1 or an insurer schedule requiring screen-level telemetry, which is not shown yet.

### Gaps
- The EU AI Act's record-keeping duties (Art. 12 automatic logging for high-risk systems; deployer log retention) were not re-verified against the official text in this pass.
- The agentinsured.eu claims that policies exclude deployments "lacking audit telemetry" or where approval gates were bypassed could not be checked against actual policy wordings.
- No source gives the market size or pricing for AI agent insurance premiums, or AIUC-1 certification fees.
- No product was found marketing screen-level execution evidence for computer-use agents. That absence rests on the searches run here, not an exhaustive scan. RPA vendors' own audit logs (UiPath, Automation Anywhere) were not researched.
- Revenue figures for Zenity, Noma, AIUC and Armilla were not disclosed.

---

## Q3. (c) Knowledge capture and work-process intelligence: market, buyers, competitors, and whether demonstration-to-skill (SKILL.md) is an interoperable asset

### Takeaway
This is the most proven *enterprise* budget of the three:
- Scribe reached a $1.3B valuation (Nov 2025) and $100M ARR (April 2026) and repositioned as "workflow context" for agents with an MCP server.
- Skan AI raised $63M (Aug 2026), growing 300% year on year and serving 7 of the 10 largest US banks.
- Mimica, Guidde and a wave of "company brain" startups (a YC 2026 Request for Startups theme) are all chasing the same "capture how work is done so agents can do it" story.

SKILL.md is now a real cross-vendor format (Claude Code, Codex, Copilot/VS Code, Cursor, Gemini CLI, Microsoft Agent Framework, 25+ tools). But on 18 June 2026 OpenAI shipped Codex **Record & Replay**, which turns a macOS demonstration into an editable SKILL.md. That commoditizes the headline Decurix demonstration-to-skill capability. What is left to differentiate on is Linux/Windows/VM coverage, EU availability (Codex excludes the EEA, UK and Switzerland at launch), deterministic verified replay, privacy redaction, and enterprise governance.

### Cited Findings

**Workflow capture and task mining**
- Scribe: $75M Series C at a $1.3B post-money valuation (led by StepStone, Nov 2025). It had about 120 employees, 5M+ users and 94% of the Fortune 500 (45% paying), and had documented 10M+ workflows across 40,000 applications. It launched Scribe Optimize, which maps workflows and ranks automation candidates. — [TechCrunch, 10 Nov 2025](https://techcrunch.com/2025/11/10/scribe-hits-1-3b-valuation-as-it-moves-to-show-where-ai-will-actually-pay-off/); [Scribe](https://scribe.com/library/scribe-raises-series-c)
- Scribe passed $100M ARR in April 2026 (about $8.3M revenue that month). It has 6M+ employees with the app installed and about 600,000 organizations. Pricing runs "from a $20 subscription to five- to seven-figure contracts". It launched Agents and an MCP Server "that gives agents direct access to Scribe's workflow intelligence". Total funding is $130M. — [Forbes, 10 May 2026](https://www.forbes.com/sites/rashishrivastava/2026/05/10/this-ai-startups-software-watches-employees-as-they-work-to-reduce-inefficiencies/); [Scribe press release, 11 May 2026](https://scribe.com/library/100-million-arr-press-release); [Sacra, May 2026](https://sacra.com/c/scribe/)
- Sacra: Scribe Optimize "competes for spend that currently goes to process mining vendors, management consulting, and AI readiness programs" (CIO and operations budgets). — [Sacra](https://sacra.com/c/scribe/)
- Skan AI: $63M Series C (Cathay Innovation and Dell Technologies Capital, 12 Aug 2026), about $120M in total. Growth of 300%+ year on year for the second year running, and about 150% net dollar retention. Customers include 7 of the 10 largest US banks and a quarter of the Fortune 50. At one bank it observed 11.2M context switches across 1,500 finance staff and reports $18M in annualized savings (company-reported). It sells private on-prem appliances on Nvidia. — [VentureBeat, 12 Aug 2026](https://venturebeat.com/data/skan-ai-raises-63-million-betting-that-watching-how-employees-actually-work-is-the-missing-layer-of-enterprise-ai); [Cathay Innovation](https://cathayinnovation.com/skan-ai-raises-63-million-co-led-by-cathay-innovation/)
- Mimica: $26.2M Series B (Paladin Capital, 24 Sep 2025), $36.7M in total. ARR grew more than 570% over 18 months, and it serves 30+ large enterprises. Positioned as "record employees and infer workflow diagrams to automatically train AI agents". — [Mimica](https://www.mimica.ai/articles/mimica-secures-26-2-million-in-series-b-funding-to-train-agentic-ai-on-real-enterprise-workflows); [CB Insights](https://www.cbinsights.com/company/mimica/financials)
- Guidde: $50M Series B (PSG Equity, Feb 2026). It captures "every click, scroll… interaction with the HTML page" as "Video Ground Truth", pitched as training data for both humans and agents. — [VentureBeat, 25 Feb 2026](https://venturebeat.com/orchestration/visual-imitation-learning-guidde-trains-ai-agents-on-human-expert-video)
- Celonis: last priced at about $13B (August 2022, $400M equity plus $600M debt), about $2.4B raised, 3,000+ employees and 5,000+ deployments per a company-profile aggregator (August 2026). No current revenue figure was found. — [Silicon Valley Invest Club profile](https://siliconvalleyinvestclub.com/companies/celonis/)
- Process-mining market size **estimates diverge by about 6x**: Mordor puts 2026 at $0.85B (rising to $2.01B by 2031, 18.6% CAGR). Fortune Business Insights puts 2026 at $5.45B (rising to $58.18B by 2034). MarketsandMarkets gives $1.8B (2023) rising to $12.1B (2028). The vendor lists include Skan, KYP.ai and Soroco. — [Mordor, Jan 2026](https://www.mordorintelligence.com/industry-reports/process-mining-software-market); [Fortune BI](https://www.fortunebusinessinsights.com/process-mining-software-market-104792); [MarketsandMarkets](https://www.marketsandmarkets.com/Market-Reports/process-mining-market-176608355.html)
- Gartner Peer Insights keeps a "Task Mining Tools" category listing Celonis, Mimica, Soroco and KYP.ai. — [Gartner Peer Insights](https://www.gartner.com/reviews/market/task-mining-tools)

**"Company brain" and knowledge capture**
- "Company Brain" is idea #05 in YC's 2026 Request for Startups (Tom Blomfield). The thesis is $50–100 per employee per month. It cites Glean at a $4.6B valuation, which is an older figure. — [YC Insight](https://ycinsight.com/company-brain)
- Modus: $10M seed (Insight Partners, July 2026) for a "Context Warehouse" that learns from usage patterns. — [The Next Web, 29 Jul 2026](https://thenextweb.com/news/modus-10m-seed-insight-partners-context-warehouse-enterprise-ai)
- YC 2026 companies in this space include Savant ("capture… undocumented procedures, and serve them to your employees and agents"), Hyper, Cerenovus (S26) and screenpipe (records the screen into searchable memory). — [YC productivity directory](https://www.ycombinator.com/companies/industry/productivity); [Narracomm, Jun 2026](https://www.narracomm.com/cerenovus-cerenovus-ai-the-ai-company-brain-that-turns-every-file-email-and-message-into-a-living-map-of-your-business-yc-s26/)
- A vendor estimate (not independent): a 200-person company loses €490k–600k a year to knowledge it cannot find or keep. "When a senior expert leaves… 42 percent of what they knew goes with them." Germany has 13.4M workers reaching retirement age within 15 years. — [Superkind, 8 Jun 2026](https://superkind.ai/blog/company-brain-cost)

**Demonstration-to-skill and interoperability**
- OpenAI shipped **Codex Record & Replay** on 18 June 2026 (Codex App 26.616): "Show Codex a workflow once. Reuse it as a skill."
  - It turns a macOS demonstration (actions plus visible window content) into an inspectable, editable SKILL.md with inputs, steps, verification steps and decision points, and replays it through Computer Use, browser tools and plugins.
  - It is macOS only and requires Computer Use. It **excludes the EEA, UK and Switzerland at launch**, and Computer Use can be disabled centrally with `requirements.toml`.
  - Guidance says it works best on short, stable, predictable workflows, and generalizing across branches is weak.
  - Source: [OpenAI Developer Community, 18 Jun 2026](https://community.openai.com/t/introducing-record-replay/1384088); [TechsCurrent, 23 Jun 2026](https://techscurrent.com/2026/06/openai-codex-record-replay-workflow-skills/); [ChatForest, 20 Jun 2026](https://chatforest.com/builders-log/openai-codex-record-replay-workflow-skill-macos-builder-guide/)
- Context from the same source: in June 2026, non-developers were about 20% of Codex's 5M weekly users and growing more than 3x as fast as developers. OpenAI shipped six role plugins bundling 62 apps and 110 skills. — [ChatForest](https://chatforest.com/builders-log/openai-codex-record-replay-workflow-skill-macos-builder-guide/)
- The Agent Skills open standard (SKILL.md, from Anthropic) is supported "across 25+ editors", including Claude Code, Cursor, VS Code/Copilot, OpenAI Codex, Gemini CLI, Amp, Goose, Databricks and Spring AI (as of Feb 2026). Microsoft Agent Framework documents native SKILL.md support (docs updated 18 Sep 2026), including MCP-served skills over `skill://`. Microsoft ships 193+ Azure skills in the format. — [agentskills.io spec](https://agentskills.io/specification); [agent-config.com](https://agent-config.com/guides/agent-skills-standard/); [Microsoft Learn, Sep 2026](https://learn.microsoft.com/en-us/agent-framework/agents/skills); [MicrosoftDocs/Agent-Skills](https://github.com/microsoftdocs/agent-skills)

**Buyer resistance and privacy**
- Anecdotal: Meta's push for company-wide screen recording met "enormous pushback, first on employee privacy, second, you're plainly going to fire me". By contrast, banks such as JPMorgan already record every keystroke for compliance and keep it 10–15 years. This is a podcast claim and was not verified. — [Mossfire Insights, Jul 2026](https://insights.mossfire.xyz/en/posts/ep41-screen-recording-rush/)

### Inferences
- Buyers are CIOs, COOs, transformation and operations leaders, and heads of automation or CoE in large regulated enterprises (Skan: banks and insurers; Scribe: 45% of the Fortune 500 paying). They pay for (1) "where should we automate" discovery and (2) agent-ready context. The budgets already exist: process mining, consulting, documentation and enablement.
- The Decurix pitch (record, then a verified skill, then an agent does it, and knowledge stays when people leave) sits between Scribe/Guidde (documentation), Skan/Mimica (observation at scale) and Codex Record & Replay (personal demonstration-to-skill). Its unusual pieces: **the skill is executable and checked** (deterministic replay, an outcome taxonomy, verification, an approval gate), **it runs in an isolated VM rather than on the employee's own laptop** (a much weaker surveillance story than always-on task mining, which matters given Meta-style pushback and EU works councils), and **Windows apps can be shown inside the VM**.
- **SKILL.md export creates interoperability, not a moat.** Because the format is universal, a Decurix-taught skill runs in Codex, Claude Code, Copilot and Microsoft Agent Framework. That helps adoption, but it also means the skill artifact is portable away from Decurix. OpenAI producing SKILL.md from demonstrations natively commoditizes the capture step for Mac users outside Europe. The defensible layer would be verified replay, fleet governance (who taught what, with what approval rules, health and drift of skills, per Decurix's `experience.health`) and the audit trail. That is the knowledge-capture pivot merging with option (b).
- **Fastest revenue:** the "handover" wedge (capture a departing or retiring expert's workflows as verified, runnable skills) sold as a fixed-fee engagement to mid-market or EU firms that Codex does not serve. Five-figure pilots in about 3 months are plausible. Scaling needs connectors and the enterprise security reviews that Skan says took years.
- **The $1B version** is Scribe or Skan-scale: a system of record for "how work is done" that every agent draws on. Getting there takes a distribution wedge (Scribe's was a free browser extension at 5M+ users) that a VM-based product does not naturally have. The VM approach fits regulated, high-sensitivity work (banks, healthcare, public sector) better than bottom-up virality.

### Gaps
- Celonis's current (2026) revenue and valuation were not found. The $13B mark dates from 2022.
- No revenue figures were found for Skan, Mimica, KYP.ai, Soroco, Tango or Guidde. Only growth rates, all self-reported.
- Market size is unreliable given the 6x spread between research firms, and no Gartner or IDC figure was obtained.
- No data was found on whether enterprises pay for knowledge-transfer or handover tools as a separate line item.
- It is unverified whether EU/German works-council or GDPR rules, or the AI Act's Annex III "workers' management" category (applying 2 Dec 2027), would classify employee workflow recording as high-risk. This matters for (c) in Europe and needs legal review.
- How well Codex Record & Replay generalizes, and whether Windows/Linux or EU availability will follow, is unknown.

---

## Q4. Cross-cutting: how fast can a small team get revenue, and what does the billion-dollar version of each look like?

### Takeaway
Ranked by speed to first revenue for a small team with Decurix's current assets:
1. **(a) Services-style environment and task contracts** are fastest *if* a lab or neolab relationship exists. Contracts run six to seven figures per quarter, but spend is concentrated and labs are moving work in-house.
2. **(b) Evidence and testing for certification and underwriting**: 3–6 months with a design partner. Small at first, with platform-scale upside only if standards or insurers come to require screen-level telemetry.
3. **(c) Knowledge-capture pilots**: 3–6 months, with the largest proven enterprise budgets but the most direct competition from OpenAI.

The assets are most valuable where the three overlap: verified, redacted, replayable execution records of desktop work.

### Cited Findings
- Environment contracts are "six to seven figures per quarter". — [Epoch AI](https://epoch.ai/gradient-updates/state-of-rl-envs)
- Most environment vendors serve 1–3 customers. — [SemiAnalysis](https://newsletter.semianalysis.com/p/rl-environments-and-rl-for-science)
- The Deeptune acquisition by Mercor (July 2026) shows data foundries buying environment builders. — [RuntimeWire](https://runtimewire.com/article/mercor-20b-valuation-talks-deeptune-acquisition)
- AIUC-1 recertification is quarterly, with an accredited-auditor ecosystem (Schellman, Coalfire). — [SiliconANGLE](https://siliconangle.com/2026/09/15/ai-agent-certification-startup-aiuc-raises-40m-to-begin-auditing-frontier-models/); [aiuc-1.com](https://www.aiuc-1.com/)
- Insurers are held back by missing underwriting data. — [agentinsured.eu](https://agentinsured.eu/articles/european-ai-agent-insurance-market-tracker-june-2026)
- Scribe reached $100M ARR about 7 years after founding, with about 120 employees at the Series C. — [Forbes](https://www.forbes.com/sites/rashishrivastava/2026/05/10/this-ai-startups-software-watches-employees-as-they-work-to-reduce-inefficiencies/); [TechCrunch](https://techcrunch.com/2025/11/10/scribe-hits-1-3b-valuation-as-it-moves-to-show-where-ai-will-actually-pay-off/)
- Micro1 shows that data reusable across customers ("off-the-shelf") earns 80–90% gross margin, against about 30–40% net on bespoke contractor work. — [TechCrunch, Aug 2026](https://techcrunch.com/2026/08/20/ai-data-startup-micro1-reaches-500m-gross-run-rate-amid-ai-training-boom/)

### Inferences
- **Are the assets worth more as infrastructure than as an end-user product?** Partly. The most saleable asset to labs is the *environment plus verifier plus trajectory-capture* machinery (desktop VMs, `bench/gui`-style outcome checks, importers), not the end-user app. The most saleable asset to enterprises and insurers is the *verified execution record* (outcome taxonomy plus approval gate plus redacted frames). The demonstration-to-skill capture is the least defensible piece now that OpenAI ships it natively.
- **A hybrid seems most defensible:** keep a thin enterprise product (VM-isolated record, then verified skill, then agent, with EU/on-prem availability where Codex is not) whose exhaust is (1) evidence packs for certification and insurers and (2), with consent and redaction, reusable non-exclusive enterprise-desktop task environments for labs. Micro1's margin data suggests reusable non-exclusive packs beat bespoke exclusive work on margin, even though exclusive work pays 4–5x more per deal.
- **Billion-dollar versions, one per option:**
  - (a) an "environment factory" for enterprise desktop software, likely to be acquired by or to compete with Mercor, Surge or Turing;
  - (b) the SOC 2-style evidence and telemetry layer for insured computer-use agents;
  - (c) a Skan/Scribe-scale "context graph of work" for regulated enterprises.
  Each needs capital and a distribution or lab relationship that Decurix does not have yet.

### Gaps
- No verified data on how long environment vendors take to land their first lab contract, or how often they win one.
- No pricing found for certification-readiness tooling, or for telemetry/data partnerships with insurers.
- Buyer interviews would be needed to test whether labs value LibreOffice/Linux-desktop fidelity, and whether insurers or auditors would accept screen-level evidence.
