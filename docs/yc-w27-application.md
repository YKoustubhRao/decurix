# Decurix — YC Winter 2027 application draft

Prepared on 28 October 2026 from Koustubh Rao's supplied answers and the current codebase. Text under each application question is intended for copying. Bracketed instructions identify attachments or optional fields that still need to be supplied. Suggested pricing is a proposal, not an existing customer agreement.

## Founders

**Who writes code, or does other technical work on your product? Was any of it done by a non-founder? Please explain.**

I do all the technical work, using Claude Code to help write and debug code. I am responsible for the architecture, implementation, and deployments. No non-founder has done technical work on the product.

**Are you looking for a cofounder?**

I am open to a cofounder with strong sales and customer development experience. An early commercial hire is another option. I am currently building the product and supporting the first deployment myself.

## Company

**Company name**

Decurix

**Describe what your company does in 50 characters or less.**

OS for local AI workers that learn and collaborate

**Company URL, if any**

[Enter your live company website, or leave blank if you do not have one.]

**If you have a demo, attach it below.**

[Attach a product screen recording of up to three minutes and under 100 MB. See the separate demo plan.]

**Please provide a link to the product, if any.**

[Enter a publicly accessible product or demo link if available; otherwise leave blank.]

**If login credentials are required for the link above, enter them here.**

[Provide a dedicated demo account only if needed.]

**What is your company going to make? Please describe your product and what it does or will do.**

Decurix is an operating system for local AI workers that learn and collaborate. Our ambition is to let anyone create, teach, run, and eventually buy or sell AI workers.

A worker has its own isolated desktop, applications, files, memory, and learned procedures. A person teaches it by demonstrating work and explaining rules. It applies those procedures to new jobs, asks questions, checks saved results, and retains corrections. Workers can share skills and hand work to each other.

Users supply the LLM. Decurix keeps the worker's environment, memory, and learned procedures in the user's installation, separate from the model. Users can choose local inference to process task data locally, or connect a supported hosted provider that receives the context needed for the job.

We plan a marketplace where experts can teach specialized workers and sell them for others to install, adapt, and combine into a workforce. We want Decurix to become the OS for running that workforce on PCs and mobile devices.

The first version is already deployed at my father's accounting firm. About 20 employees use it to read bills and bank statements and enter ERP debit and credit forms. This is our first use case for a platform intended for small and midsize businesses globally.

**Where do you live now, and where would the company be based after YC?**

Bangalore, India / San Francisco, USA

**Explain your decision regarding location.**

I plan to move to San Francisco for YC and build the company there. I want to work directly with US business customers and be close to AI founders, potential hires, and people who could build specialized workers on Decurix. The accounting deployment gives us a starting point; my ambition is to build a global worker OS and marketplace.

## Progress

**How far along are you?**

I have built a working v0 of the local worker OS and deployed it at my father's accounting firm. About 20 employees actively use it to read bills and bank statements and enter ERP debit and credit forms. I receive feedback about slow tasks and failures and update the product. This is one unpaid deployment; we have no revenue yet. Buying and selling workers is part of the planned marketplace.

**How long have each of you been working on this? How much of that has been full-time? Please explain.**

I started in early September 2026 and have worked on Decurix for about two months, nearly full-time. I am the sole founder.

**What tech stack are you using, or planning to use, to build this product? Include AI models and AI coding tools you use.**

The control plane and agent orchestration are written in Go. Python provides the computer-use, document-processing, teaching, and verification layers. We use Docker for isolated worker desktops, SQLite for persistent state, a browser-based desktop viewer, and Electron for the desktop application.

The OS is separate from the model: users supply local models through Ollama or configure supported Claude and Azure-hosted models. Worker state and learned procedures are stored in the user's installation. I use Claude Code to help develop the product.

**Are people using your product?**

Yes.

**How many active users or customers do you have? How many are paying? Who is paying you the most, and how much do they pay you?**

We have one organization using Decurix: my father's accounting firm, with approximately 20 active users. They use it for bill and bank-statement entry into ERP debit and credit forms. There are no paying customers and no revenue.

**Do you have revenue?**

No.

**If you are applying with the same idea as a previous batch, did anything change? If you applied with a different idea, why did you pivot and what did you learn from the last idea?**

Not applicable.

**If you have already participated or committed to participate in an incubator, accelerator or pre-accelerator program, please tell us about it.**

None.

## Idea

**Why did you pick this idea to work on? Do you have domain expertise in this area? How do you know people need what you're making?**

I picked this idea because I want people to build and control their own AI workforce. Someone who knows how to do a job should be able to teach a worker, keep that knowledge in their own installation, and eventually sell the trained worker to others. The user should choose the model that powers it.

My father's accounting firm gave me direct access to the first use case: reading bills and bank statements and entering ERP debit and credit forms. I am a technical founder; the firm's staff provide accounting expertise and feedback. I built and installed Decurix there, and about 20 employees actively use it. Their feedback on real tasks guides the product. We have not yet tested paid demand outside that firm.

**Who are your competitors? What do you understand about your business that they don't?**

Our closest competitors include Sola's demonstration-based automation, UiPath, and computer-use agents such as ChatGPT agent and Claude Cowork. Accounting-specific products such as Basis also compete for the work we are starting with.

Our thesis is that a persistent worker will become a unit of software people create and distribute. Its environment, learned procedures, memory, and permissions should remain under the user's control as the underlying model improves. A person should be able to teach expertise once, reuse it across jobs, and license a trained worker to others.

Decurix combines local worker execution, user-supplied models, teaching, verification, and collaboration. We are building toward an OS and marketplace for these workers. In our first accounting use case, that means learning which record to use, where values come from, when to ask, and how to check what was saved. The key metric is human supervision per correctly completed job.

**How do or will you make money? How much could you make?**

We plan to offer a free basic version for individuals and paid business licenses for deploying and managing a workforce. Users supply their own models. We also plan a marketplace where creators sell trained workers and skills, with Decurix taking a fee on transactions.

Pricing has not been tested yet. For scale, 100,000 businesses paying an average of $1,000 per month would generate $1.2 billion in annual license revenue, before marketplace fees. This is an illustrative scale scenario. Our ambition is to become the OS through which people build, distribute, and run AI workers across industries.

**If you had any other ideas you considered applying with, please list them.**

1. A verifiable professional profile built from demonstrated work, including coding, communication, and problem-solving, rather than a self-reported resume.
2. An interview platform where a candidate's AI agent is interviewed by an employer's AI agent. The interview would examine the candidate agent's skills and domain knowledge, with an LLM evaluating the exchange.

## Equity

**Have you formed ANY legal entity yet?**

No.

**If you have not formed the company yet, describe the planned equity ownership breakdown among the founders, employees and any other proposed stockholders.**

Koustubh Rao, Founder and CEO: 100% initially. I am the sole founder. Equity for a future cofounder or employees has not been allocated.

**Have you taken any investment yet?**

No.

**Are you currently fundraising?**

Yes. I am applying to YC to raise our first funding. I have not applied to other funding programs.

## Curious

**What convinced you to apply to Y Combinator? Did someone encourage you to apply? Have you been to any YC events?**

I want to build Decurix into a global OS and marketplace for AI workers. I have built and deployed the first version, and I want YC's help finding customers beyond my father's firm, testing the business model, and building a company around that ambition. Friends encouraged me to apply. I have not attended any YC events.

**How did you hear about Y Combinator?**

I discovered Hacker News through a post and started following it to stay current with technology. That led me to learn about Y Combinator and its startup program.

## Batch preference

**What batch do you want to apply for?**

Winter 2027.

## Founder video — talking points

Record approximately one minute of Koustubh speaking naturally. Use these as prompts, not a script:

- Introduce yourself: Koustubh Rao, sole founder of Decurix, currently in Bangalore.
- Explain the product: an OS for local AI workers that learn and collaborate.
- Explain user control: users supply the model; worker environments, memory, and learned procedures live in their installation.
- Give the actual use case: reading bills and bank statements and entering ERP debit and credit forms.
- State progress: built since early September; one deployment at your father's firm; about 20 active users; no revenue yet.
- Explain the ambition: people teach specialized workers, sell them through a marketplace, and combine them into their own workforce.

Keep the founder video separate from the product demo. YC asks for a one-minute founder-only video and recommends spontaneous delivery from bullet points: https://www.ycombinator.com/video

## Final review notes

- The description is exactly 50 characters, including spaces.
- Update the work duration and active-user count if submission is later than this draft.
- Keep the family relationship and unpaid status explicit.
- Do not claim quantified time savings, accuracy, or declining token use until measured.
- Current default worker model choices are Ollama, Claude, and Azure. Do not promise that any ChatGPT or Claude subscription can power a worker without confirming the exact supported integration.
- Keep local execution, user-controlled worker state, and user-supplied models central. Hosted inference sends relevant task context to the selected provider; supplying a model does not by itself mean all processing stays local.
- Buying and selling workers is the marketplace vision, not an existing revenue source. Current skill sharing is a foundation; installing a skill does not transfer its prior execution trust or automatically grant permissions.
- Free personal access, paid business licenses, marketplace fees, and the $1,000/month example are proposed business-model and scale assumptions.
- Supply website/product URLs and the two video attachments where available.

Writing reference: https://www.ycombinator.com/howtoapply
