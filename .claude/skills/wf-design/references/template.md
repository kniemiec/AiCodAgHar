# Design Doc Template

Fill every section below — don't leave a section as a placeholder. Replace `{{...}}` with real content; the surrounding structure and section names should stay as-is so design docs stay consistent across this repo's `docs/designs/active/` and `docs/designs/completed/`.

```markdown
# {{Title}} — Design

| | |
|---|---|
| **Status** | Draft |
| **Date** | {{today, ISO format}} |
| **Owner** | {{git user}} |
| **Purpose** | {{one or two sentences: the problem being closed and the shape of the fix}} |

## Goal

{{One paragraph: what will be different once this is built, stated concretely enough that "done" is unambiguous — name the actual function/file/endpoint that changes, not just the feature area.}}

## Context — Why This Shape

{{Why the current code doesn't already do this — cite the specific file:line or doc comment that shows the gap. Then explain any alternative approach that was considered and rejected, and why (the actual trade-off, not a strawman) — if no alternative was seriously considered, say so rather than inventing one.}}

## Design

### 1. {{First concrete change}}

{{File path(s), and either real interface/type signatures or a short code sketch. Reference existing naming/idiom conventions in the codebase by name (e.g., "mirrors CreateOrReuseIssue's idempotent-by-name pattern") rather than inventing a new one from scratch when an existing one fits.}}

### 2. {{Next concrete change}}

{{...}}

### N. {{What explicitly does *not* change}}

{{Name the adjacent components/files that stay untouched, and why that's safe — this is often as important as what changes.}}

## Testing Strategy

{{Concrete tests, named by the existing test file/pattern they'd extend (unit, integration, e2e — whatever this repo already distinguishes) — not a generic "add tests" line.}}

## Success Criteria

{{A short list of observable, checkable outcomes — each one should be answerable yes/no once implemented.}}

## Out of Scope

{{What's deliberately not addressed, and why — usually because it's a separate concern, a pre-existing assumption being kept as-is, or genuinely out of scope for this change's size.}}

## Named Risks / Follow-Ups

{{Anything asserted in this doc that couldn't be verified by reading code (e.g., a third-party service's runtime behavior) — flagged here explicitly, along with what would need to happen to confirm it, and a fallback if the assumption turns out false.}}

## Results

*(Appended here as a dated run entry once implemented.)*
```
