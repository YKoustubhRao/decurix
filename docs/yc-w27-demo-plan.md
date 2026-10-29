# Decurix — YC Winter 2027 product demo plan

Status: planned, not recorded. Screen recording is deferred at the founder's request. Target: under three minutes and under 100 MB, matching the supplied application form.

## The claim to demonstrate

Decurix is an OS for local AI workers that learn and collaborate, powered by models the user supplies. Demonstrate the platform through the accounting deployment: a person teaches a worker a bill-entry workflow, it applies that workflow to a different bill, processes a related bank-statement entry, and shows the saved ERP records. End with the vision of a marketplace where people buy and sell trained workers.

Use the ERP application and workflow actually used at the accounting firm. Use a dedicated test company and fictional documents, with the accountant's debit/credit rules. The recording should identify the data as demo data.

## Prepare before recording

1. Run a stable Decurix build and the working ERP integration used by the firm. Pick a worker without this particular taught workflow already installed.
2. Create two clearly different fictional bills: teaching example A for INR 12,500 and fresh example B for INR 18,200. Change invoice number, date, and amount. Use a simple transaction whose account and tax treatment an accountant can verify.
3. Prepare a fictional bank statement with a matching payment for example B. Keep invoice references and bank references visible enough to demonstrate matching.
4. Have the accountant specify the target accounts, debit/credit mapping, matching rules, and what counts as a completed save. Do not invent those rules for the video.
5. Confirm the final ERP entries can be reopened and read. Remove unrelated customer data, credentials, and other applications from the recording surfaces.
6. Rehearse the complete workflow. If a capability does not work, shorten the claim to the working behavior rather than simulating a result.
7. Have the actual model choice and retained workflow visible in the product. Explain local worker state accurately: if the demo uses a hosted model, relevant task context reaches that provider. Claim entirely local task processing only for a verified local-inference setup.

## Recording sequence

| Time | Screen | Point to communicate |
|---|---|---|
| 0:00–0:15 | Worker list, actual model choice, then the worker's ERP and source documents | Decurix is an OS for local AI workers. Users supply the model and teach workers their jobs. Show the accounting use case. |
| 0:15–0:55 | Teach with bill A: demonstrate entry and explain mapping/checks | The worker receives a procedure and business rules from a person. Show the retained workflow when saved. |
| 0:55–1:45 | Submit bill B; watch the worker use its different invoice number and amount | The worker applies the taught workflow to fresh inputs. Make the difference from bill A visible. |
| 1:45–2:15 | Process the matching bank-statement entry | Show the second part of the actual deployed workflow and any necessary human question honestly. |
| 2:15–2:45 | Reopen saved ERP records and compare with documents | Show invoice identity, amount, debit/credit mapping, and the saved bank entry. Explain what Decurix checked and what the person checked. |
| 2:45–2:55 | Return to the worker/task view and its retained workflow | State: one deployment at the founder's accounting firm, about 20 active users, no revenue. Explain the ambition: people teach workers, buy and sell them, and assemble their own workforce. |

If the sequence does not fit, prioritize teaching, one fresh bill, and saved-result readback. Show the bank statement briefly as another supported input rather than compressing away the proof of correct work.

The marketplace is a stated future ambition. Do not present a checkout, worker sale, or simulated marketplace screen as a shipped capability. If current skill sharing and handoff work reliably in the recorded build, a brief real view can support the collaboration claim; otherwise keep this as narration about the platform direction.

## Optional exception

If the working product reliably asks about ambiguous account or party matching, replace a portion of the bank-statement segment with one such case. Show the real question and answer. Do not claim the answer was retained unless a subsequent run demonstrates it. Do not add an exception merely to make the recording dramatic.

## Recording and editing requirements

- Record the actual product interaction. A timeline may compress waiting, but label sped-up footage or time cuts.
- Do not conceal interventions or present edited timing as measured throughput.
- Use short narration tied to what appears on screen. Avoid architecture slides and feature lists.
- Do not include synthetic practice/simulation in the demo unless it is integrated, reachable, and working in the recorded build.
- Show the saved result in the ERP rather than relying on an agent's success message.
- Do not claim quantified savings, accuracy, or token reduction from a single demonstration.
- Check that text is legible and the exported file is within the form's three-minute/100 MB limits. Keep the one-minute founder video separate.

## Evidence needed before adding performance claims

Measure several fresh tasks of the same kind, recording all model calls, repairs, questions, human time, and final correctness. Compare them with manual completion of the same work. A token-use or speed chart belongs in a later demo only if these measurements support it.
