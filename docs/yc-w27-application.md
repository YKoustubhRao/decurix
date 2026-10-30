# Decurix — YC Winter 2027 application draft

Prepared on 28 October 2026 and revised on 30 October 2026 from Koustubh Rao's answers, the current codebase, and the recorded product demo. Text under each question is for copying into the form. Bracketed items are attachments or numbers only you can supply. Pricing is a proposal, not an existing customer agreement.

## Founders

**Who writes code, or does other technical work on your product? Was any of it done by a non-founder? Please explain.**

I do all the technical work: architecture, the agent, the desktop app, and deployments. I use Claude Code heavily to write and debug code. No non-founder has done technical work on the product.

**Are you looking for a cofounder?**

I am open to a cofounder with strong sales and customer development experience. An early commercial hire is another option. Until then I am building the product and supporting the first deployment myself.

## Company

**Company name**

Decurix

**Describe what your company does in 50 characters or less.**

OS for local AI workers that learn and collaborate

**Company URL, if any**

[Enter your live company website, or leave blank.]

**If you have a demo, attach it below.**

[Attach `decurix-demo.mp4`: the 3-minute product demo, under 100 MB.]

**Please provide a link to the product, if any.**

[Enter a public product or demo link if available; otherwise leave blank.]

**What is your company going to make? Please describe your product and what it does or will do.**

Decurix is an operating system for AI workers that learn and collaborate. Our ambition is to let anyone create, teach, run, and eventually buy or sell AI workers, and combine them into their own workforce.

A worker has its own isolated computer: a desktop, the same apps a person uses, files, memory, and learned procedures. You teach it a job by doing it once on its screen and explaining the rules. It studies the recording, writes down the rules of the job, marks which ones it saw and which it is only guessing, and asks about what it could not see. Then it does the next jobs itself, in a chat with the person it works for. Workers share skills through a team registry and hand work to each other.

What makes it usable for real work is that a worker never reports success it has not checked. Before it says done, it compares what it saved with the source, and a separate review holds the work to the rules it was taught. When something does not match or a value is ambiguous, it stops and asks instead of guessing, and each answer becomes a rule it follows next time.

The model is the user's choice: a local model, or Claude or Azure today. The worker's environment, memory, and learned procedures live in the user's installation, separate from the model, so a trained worker survives model changes and belongs to the person who trained it. With a local model, task data stays on the machine.

That makes a trained worker a unit of software people can own and distribute. We plan a marketplace where experts teach specialized workers and sell them: the seller sells the worker's know-how (its skills, rules, and memory), and the buyer brings their own model and runs the worker on their own device. For customers who pay more, we will train a small model on a worker's own checked runs, so routine work costs a fraction of the tokens. We want Decurix to become the OS for running that workforce on PCs and, later, mobile devices.

The first version is deployed at my father's accounting firm, where about 20 employees use it to read bills and bank statements and enter them into their ERP. Accounting is our first use case, not the product: nothing in the worker is specific to it, and we test it on spreadsheets, documents, web portals, and desktop accounting software.

**Where do you live now, and where would the company be based after YC?**

Bangalore, India / San Francisco, USA

**Explain your decision regarding location.**

I plan to move to San Francisco for YC and build the company there. I want to sell to US businesses directly and be close to AI founders, potential hires, and the people who could build specialized workers on Decurix. The accounting deployment in India is our proving ground; the ambition is a global worker OS and marketplace.

## Progress

**How far along are you?**

I have built a working v0 of the worker OS in two months: a control plane that runs isolated worker desktops, an agent that does the work on screen, teaching by demonstration, a team skill registry and handoff between workers, and the checks that decide when work is really done.

It is deployed at my father's accounting firm, where about 20 employees use it daily to read bills and bank statements and enter them into their ERP. They tell me when a task is slow or wrong, and I fix the product. The 3-minute demo is a real, unstaged run: a worker taught once enters new bills from other suppliers, asks when an item has no clear match, refuses to call work done that its checks have not confirmed, and stops on a supplier it was told never to create.

This is one unpaid deployment; we have no revenue yet. The worker is still slow (minutes per bill). The marketplace and small-model training are planned, not built.

**How long have each of you been working on this? How much of that has been full-time? Please explain.**

I started in early September 2026 and have worked on Decurix for about two months, nearly full-time. I am the sole founder.

**What tech stack are you using, or planning to use, to build this product? Include AI models and AI coding tools you use.**

The control plane and the agent's core loop are written in Go. Python provides the computer-use, document-reading, teaching, and verification layers. Each worker is a Docker container running a full Linux desktop, viewed in the browser over VNC; SQLite holds persistent state; Electron powers the desktop app for Windows and Mac.

The OS is separate from the model: users run local models through Ollama or connect Claude and Azure OpenAI models. Worker state and learned procedures are stored in the user's installation. I build with Claude Code.

**Are people using your product?**

Yes.

**How many active users or customers do you have? How many are paying? Who is paying you the most, and how much do they pay you?**

One organization: my father's accounting firm, with about 20 active users, for bill and bank-statement entry into their ERP. There are no paying customers and no revenue.

**Do you have revenue?**

No.

**If you are applying with the same idea as a previous batch, did anything change? If you applied with a different idea, why did you pivot and what did you learn from the last idea?**

Not applicable.

**If you have already participated or committed to participate in an incubator, accelerator or pre-accelerator program, please tell us about it.**

None.

## Idea

**Why did you pick this idea to work on? Do you have domain expertise in this area? How do you know people need what you're making?**

I studied computer science at IIT Bombay and worked as a quant developer at QRT. There, a typical task was: read a ticket, plan it, have AI write the code, test it, raise a pull request. AI did the heavy lifting, but I still carried every task through the same steps, every time. The intelligence was there; what was missing was a worker that knew how the job is done and could be trusted to do it.

Most office work looks like this. I want people to build and control their own AI workforce: someone who knows how to do a job should be able to teach a worker once, keep that knowledge in their own installation, choose the model that powers it, and eventually sell the trained worker to others.

My father's accounting firm gave me the first use case: staff spend their days re-typing bills and bank statements into an ERP. I am a technical founder; the firm's staff provide the accounting expertise and tell me when it is wrong. About 20 of them use it every day. That shows the work is real, but it is one firm close to me and unpaid; testing paid demand outside it is the first thing I want to do in the batch.

**Who are your competitors? What do you understand about your business that they don't?**

Demonstration-based automation (Sola), RPA (UiPath), computer-use agents (ChatGPT agent, Claude Cowork), and accounting-specific products such as Basis for the work we are starting with.

Our thesis is that a persistent worker will become a unit of software people create and distribute. Models will keep changing; a worker's environment, learned procedures, memory, and permissions should stay under the user's control and move to the next model unchanged. A person should be able to teach expertise once, reuse it across jobs, and license the trained worker to others.

The second thing we understand: in real work, clicking is not the hard part; knowing when the work is right is. A worker that is right 95% of the time and does not know which 5% is unusable. RPA breaks when a screen changes, and general agents guess. Decurix never reports success it did not check, asks instead of guessing, and turns every answer into a rule. The metric we optimize is human supervision per correctly completed job, which falls as a worker learns. That trust is what will make a worker worth buying from someone else.

**How do or will you make money? How much could you make?**

A free basic version for individuals and paid business licenses for deploying and managing a workforce. Users supply their own models, so our margin does not depend on model costs. On top: a marketplace where creators sell trained workers and skills, with Decurix taking a fee, and a premium tier that trains a small model on a worker's checked runs so routine work costs far less to run.

Pricing has not been tested. For scale, 100,000 businesses paying an average of $1,000 a month would be $1.2 billion in annual license revenue, before marketplace fees. Our ambition is to become the OS through which people build, distribute, and run AI workers across industries; the first test is whether a second business will pay for a worker.

**If you had any other ideas you considered applying with, please list them.**

1. A verifiable professional profile built from demonstrated work (coding, communication, problem-solving) instead of a self-reported resume.
2. An interview platform where a candidate's AI agent is interviewed by an employer's AI agent, examining its skills and domain knowledge, with an LLM evaluating the exchange.

## Equity

**Have you formed ANY legal entity yet?**

No.

**If you have not formed the company yet, describe the planned equity ownership breakdown among the founders, employees and any other proposed stockholders.**

Koustubh Rao, founder and CEO: 100% initially. Equity for a future cofounder or employees has not been allocated.

**Have you taken any investment yet?**

No.

**Are you currently fundraising?**

Yes. I am applying to YC to raise our first funding. I have not applied to other funding programs.

## Curious

**What convinced you to apply to Y Combinator? Did someone encourage you to apply? Have you been to any YC events?**

I want to build Decurix into a global OS and marketplace for AI workers. I have built and deployed the first version; I want YC's help finding customers beyond my father's firm, testing the business model, and building a company around that ambition. Friends encouraged me to apply. I have not attended any YC events.

**How did you hear about Y Combinator?**

I discovered Hacker News through a post and started following it to stay current with technology. That led me to Y Combinator.

## Batch preference

**What batch do you want to apply for?**

Winter 2027.

## Founder video

The script and talking points for the one-minute founder video are in [yc-w27-founder-video.md](yc-w27-founder-video.md).

## Final review notes

- The one-liner is exactly 50 characters, including spaces.
- Update the work duration and active-user count if you submit later.
- Keep the family relationship and unpaid status explicit; partners will ask, and it is stronger said first.
- Do not claim measured time savings, accuracy, or token savings until measured.
- Model choices are Ollama, Claude, and Azure. Do not claim a ChatGPT or Claude subscription powers a worker.
- Hosted models receive the task context; only a local model keeps all processing on the machine.
- The marketplace, selling a worker for the buyer's own model, small-model training, and mobile are plans. The team skill registry and handoff exist; installing a skill does not transfer its prior trust or grant permissions.
- Supply the website/product links and attach both videos.

Writing reference: https://www.ycombinator.com/howtoapply
