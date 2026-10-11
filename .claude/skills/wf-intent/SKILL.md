---
name: wf-intent
description: This skill should be used when the user runs `/wf-intent`, asks to "write requirements for X", "create an intent doc for X", "capture the requirements for X", or hands over a short description of a desired capability and wants it turned into a code-grounded requirements/intent document before any design starts. It analyses the current state of the system, checks existing ADRs, decides with the user whether a new or extended ADR is needed (drafting it as Proposed first when so), and finishes by writing a requirements document under docs/intents/ that /wf-design can consume.
---

# Workflow: Short Description → (ADR if needed) → Intent / Requirements Doc

Turn a short statement of what the author wants into a requirements document grounded in the real state of the system. This is the stage *before* `/wf-design`: it fixes **what** must be true and **why**, and any architectural decisions that constrain the design, but not **how** it will be built. The deliverable is documents (an intent doc, plus an ADR when one is needed) — this workflow never implements anything and never routes through plan mode / `ExitPlanMode`, because the documents are the deliverable.

Pipeline position: `wf-intent` (this) → `wf-design` → implementation.

## Input

The text following `/wf-intent` is the author's short description of what is required. If invoked with no text, ask directly (plain text, not a menu) what capability is needed.

## Step 1 — Frame the request and split it if needed

Restate the request in a few lines: the capability wanted, the apparent users, and what the author seems to want to keep out of scope. If the request mixes separable problems (for example "platform observability" and "agent observability"), propose the split explicitly and confirm which part this document covers; park the rest by name in the document's non-goals. This is a discussion step, so keep it in prose — do not start writing files.

## Step 2 — Analyse the current state

Establish what the system does today relating to the request, by reading, not by assuming:
- The relevant source, migrations, API handlers and configuration (`internal/`, `cmd/`, `deploy/`, `api/`).
- The living component guides `docs/components/orchestrator.md` and `container-launcher.md`. Where they disagree with ADRs or designs, trust them (per CLAUDE.md) and verify against code when it matters.
- Existing ADRs in `docs/architecture/`, completed and active designs in `docs/designs/`, and existing intent docs in `docs/intents/`.

For a wide search space, delegate the reading to an Explore agent or a fork and keep only conclusions. Every claim about current behaviour that the document will rely on must be traceable to a real `path:line`, migration or doc section; anything that cannot be verified by reading is flagged as an assumption.

Build a **baseline table**: capability → what exists today (with citation) → gap. This table goes into the document, and the gaps drive the requirements.

## Step 3 — Present findings and discuss with the author

Report back concisely in chat: what exists, what is missing, what the existing ADRs already decided that constrains this work, and any conflict or ambiguity found. Then give a **gap-driven proposal**: additional aspects the author did not mention but that best practice for this kind of system would include, each marked must/should/could with a one-line reason. Do not dump everything; prioritise.

Surface decisions that are genuinely the author's — priorities, scope boundaries, trade-offs between materially different approaches, security or trust judgement calls — and use `AskUserQuestion` for those that remain after investigation, with a recommended option first. For routine choices state a recommendation with its main trade-off and let the author redirect. Do not re-ask what the author already decided earlier in the conversation.

Wait for the author's answers before moving on; record each decision as it is made.

## Step 4 — Decide whether an ADR is needed

An ADR is needed when the work involves a decision that the design should not be free to re-make, or that changes a previously accepted one. Check against these triggers:

- A new technology, product or standard is being introduced (a datastore, a protocol, a library family, an infrastructure component).
- A cross-cutting convention is being set (identifiers, data model shape, error taxonomy, security rule).
- A previous ADR's decision is being changed, extended, or filled in where it was left open.
- A public contract (HTTP API, gRPC, persisted status model) will change in a way consumers can see.
- A trust, security, or blast-radius boundary moves.

An ADR is **not** needed when the work is only a feature or change that fits entirely inside existing ADR decisions — then say which ADRs it relies on and carry that in the intent doc's references.

State the verdict and the reasoning to the author. Distinguish **extending** an existing ADR (new ADR that builds on it, original left valid and untouched, since ADRs here are frozen historical records) from **superseding** it (rare; say so explicitly). If an ADR is needed, ask the author to confirm before drafting it. If the author declines, record the decisions as assumptions in the intent doc instead and flag them as undecided.

## Step 5 — Draft the ADR (only if needed)

Write the ADR directly into `docs/architecture/` using `references/adr-template.md`.

- File name: `week<N>-adr-<next number>-<kebab-case-title>.md`. Take the next ADR number from the highest existing one. For the week number, look at the most recent `week<N>-…` file names and dates in `docs/architecture/`, add the weeks elapsed since that file's date, and if that is not clearly determinable, ask the author which week to use.
- Status is always `Proposed`. The author changes it to `Accepted`; this workflow never does.
- Record decisions the author has already made as decided. Where the author has not chosen, put a recommendation in the decision and flag it plainly (in the chat summary and in Open Questions), so the author can see what is theirs versus proposed.
- Ground the Context section in the baseline from Step 2; do not edit older ADRs.
- Ask for the author's review of the ADR before the intent document depends on it. The intent doc may be written in the same session if the author says to proceed; it then cites the ADR as `Proposed`.

## Step 6 — Write the intent / requirements document

Write directly into `docs/intents/` with the Write tool, using `references/intent-template.md`.

- File name: `week<N>-<ISO date>-<kebab-case-slug>.md`, with the week number determined the same way as for the ADR.
- Status is always `Draft`.
- Fill every section. Requirements get stable IDs grouped by area, a priority (M/S/C) and an acceptance criterion that can be checked yes/no — vague requirements ("good logging") are rewritten until they are checkable.
- Mark all proposed numbers (thresholds, retention values, limits) clearly as proposals for the author to set, and list them in Open Questions.
- Include the traceability table from the author's original ask to requirement IDs, and the non-goals that were split off in Step 1.
- Do not specify implementation: no class layouts, no code. Requirements say what must be observable or true; the design says how.
- Link to the ADR (if one was drafted) and to the ADRs the work relies on; where a requirement depends on an unaccepted ADR, say so.

If the request turns out to be small, unambiguous and already covered by existing ADRs, say so instead of manufacturing a document — this workflow is for work large enough that writing down requirements and decisions first is worth it.

## Step 7 — Hand back concisely

Report the file paths created (ADR and intent doc) in a short summary: the shape of the requirements, the decisions the author still needs to make (thresholds, open questions, anything proposed rather than decided), and the suggested next step, usually `/wf-design` on the intent doc. Do not paste the documents back into chat.

## Scope notes

- Writes only under `docs/architecture/` (new ADR, never edits existing ones) and `docs/intents/`. Does not touch `docs/designs/`, source code, or ADR statuses.
- Does not commit or open PRs unless the author asks.
- Outputs are `Proposed` (ADR) and `Draft` (intent doc); promotion happens by the author's hand.
- Respect the author's existing decisions from the conversation; the purpose is to capture and sharpen them, not to re-litigate them.

## Additional Resources

- **`references/intent-template.md`** — the section skeleton for the requirements document.
- **`references/adr-template.md`** — the section skeleton for an ADR, matching the existing records in `docs/architecture/`.
