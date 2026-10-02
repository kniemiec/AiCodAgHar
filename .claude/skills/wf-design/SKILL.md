---
name: wf-design
description: This skill should be used when the user runs `/wf-design`, asks to "design X", "create a design doc for X", "turn this idea into a design", or hands over a rough intent/description of a change and wants it turned into a concrete design document before any implementation starts. It investigates the codebase, asks clarifying questions only when the intent is genuinely ambiguous after that investigation, grounds every claim about current behavior in real file:line citations, and writes the result as a versioned design doc under docs/designs/active/ rather than just discussing the design in chat.
---

# Workflow: Intent → Design Doc

Turn a rough intent into a concrete, code-grounded design document, ready for implementation in a later, separate session. The deliverable is the doc itself — this workflow never implements anything, and never routes through plan mode / `ExitPlanMode`, because the doc *is* the plan.

## Input

The skill receives the user's intent as the text following `/wf-design`. If invoked with no intent text, ask directly (plain text, not a menu) what should be designed.

## Step 1 — Judge whether the intent is already clear enough

Default to "clear enough" — most intents don't need an interview. Real ambiguity looks like one of:
- Two or more materially different approaches would satisfy the intent, with a genuine trade-off between them (not just a stylistic choice).
- The scope/boundary is unstated (e.g., "add X" without saying which of several plausible places X belongs).
- Success criteria aren't implied by the intent or the codebase's own conventions.
- An architectural or trust/security judgment call is buried in the request that only the user can make (product priority, risk tolerance, which layer owns a responsibility).

If none of these apply, skip straight to Step 3.

## Step 2 — Investigate before asking anything

Resolve as much ambiguity as possible by reading the codebase before involving the user — most apparent ambiguities turn out to be answered by the code itself. Read the relevant source files and interfaces, existing ADRs under `docs/architecture/`, and any related docs already under `docs/designs/active/` or `docs/designs/completed/` that touch the same area, so the new design doesn't contradict a decision that's already been made. For a wide or unfamiliar search space, delegate this reading to a fork or an Explore agent rather than dumping large file contents into the main conversation — only the conclusions need to survive.

Only questions that remain genuinely unresolved after this investigation move to Step 3.

## Step 3 — Ask only the questions that are truly the user's call

Use `AskUserQuestion` for whatever ambiguity survived Step 2, batched into as few questions as possible, with a recommended option listed first when there is one. Reserve this for decisions that depend on the user's priorities or context, not on facts discoverable in the repo. When a question is really "here's a trade-off, which way do you want to go," it's fine to instead state a recommendation with its main trade-off in prose and let the user redirect, rather than always reaching for a formal question.

## Step 4 — Ground the design in the real code

Once the intent is unambiguous, read the actual files, interfaces, and conventions the design will touch. Every claim in the produced doc about *current* behavior must be traceable to a real `path:line` citation gathered this way — never assert how existing code behaves from memory or assumption. Where an assumption can't be verified by reading (e.g., a third-party service's runtime behavior), say so explicitly in the doc's risks section rather than presenting it as fact.

## Step 5 — Write the design doc

Write directly to `docs/designs/active/<kebab-case-slug>.md` with the Write tool. Use `references/template.md` as the section structure, filling every section — an empty or placeholder-only section is worse than a shorter but complete one. Match the concreteness level of a finished example in `docs/designs/completed/` or `docs/designs/active/` if one exists: real file paths, real function/type signatures, actual line numbers, not abstract descriptions of what the code "should" do.

If the intent turns out to be a small, unambiguous change with no real design decision in it (e.g., a rename, a one-line fix), say so instead of manufacturing a doc — this workflow is for changes where writing the design down first is actually worth doing.

## Step 6 — Hand back concisely

Report the file path and a short (2-4 sentence) summary of the design's shape and any open risks it flags. Don't paste the whole doc back into chat — the doc is the artifact.

## Scope notes

- Output status is always `Draft` in `docs/designs/active/` — this workflow never moves a doc to `docs/designs/completed/`; that happens later, once the design has actually been implemented.
- This workflow doesn't implement, doesn't open PRs, and doesn't touch `ExitPlanMode` — the written doc is the complete deliverable.

## Additional Resources

- **`references/template.md`** — the exact section skeleton to fill in when writing the design doc.
