# Intent / Requirements Doc Template

Fill every section below — don't leave a section as a placeholder. Replace `{{...}}` with real content; keep the section names and order so intent docs stay consistent across `docs/intents/`. A section that truly does not apply is replaced by one line saying why, not deleted silently.

Priority key used throughout: **M** = must (first release), **S** = should (first release if feasible), **C** = could (later).

```markdown
# {{Title}} — Requirements

| | |
|---|---|
| **Status** | Draft |
| **Date** | {{today, ISO format}} |
| **Owner** | {{git user or "Me"}} |
| **Governing decision** | {{link to the ADR drafted or relied on, with its status; "none" if no ADR applies}} |
| **Next step** | A design document (`/wf-design`) derived from this one |

## 1. Goals and non-goals

### 1.1 Goals
{{G1…Gn: outcomes stated so "done" is checkable.}}

### 1.2 Non-goals
{{N1…Nn: what is deliberately excluded, including problems split off in discussion and parked for a later round, each with the reason.}}

## 2. Users and the questions they must be able to answer

| User | Must be able to answer / do |
|---|---|
| {{role}} | {{concrete questions or tasks}} |

## 3. Current state (baseline)

| Capability | Today (with `path:line` / migration / doc citation) | Gap |
|---|---|---|
| {{...}} | {{...}} | {{...}} |

{{Anything that could not be verified by reading is listed as an assumption below this table.}}

## 4. Definitions

{{Terms used by the requirements, defined precisely and tied to real names in the code where they exist.}}

## 5. Functional requirements

### 5.x {{Area}}

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| {{AREA-1}} | M/S/C | {{what must be true — not how}} | {{yes/no checkable outcome}} |

## 6. Cross-cutting requirements

{{Conventions that apply across areas: identifiers, naming, data shape, performance, retention, environments. Same ID / priority / acceptance form.}}

## 7. Non-functional requirements

{{Security and privacy, access control, operability and documentation obligations, compatibility with existing contracts and migrations. Same form.}}

## 8. Dependencies on other work

{{Other features, ADRs (including unaccepted ones), or decisions this depends on.}}

## 9. Boundary with out-of-scope work

{{What this document reserves for the parked work (extension points, identifiers), and what it deliberately does not specify.}}

## 10. Traceability to the original request

| Original ask | Requirements |
|---|---|
| {{the author's words}} | {{IDs}} |

## 11. Open questions

{{Numbered. Include every proposed number (thresholds, retention, limits) as a question for the author to set, and every undecided design-level choice.}}

## 12. Suggested delivery slicing (for the design to confirm)

{{Ordered slices that could be built and released independently.}}
```
