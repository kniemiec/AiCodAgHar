# ADR Template

Match the existing records in `docs/architecture/` (see ADR-5, ADR-11 and ADR-12 for worked examples). Fill every section. The status is always `Proposed` when written by this workflow.

```markdown
# ADR-{{N}}: {{Title}}

| | |
|---|---|
| **Status** | Proposed |
| **Date** | {{today, ISO format}} |
| **Deciders** | Me |
| **Consulted** | {{roles, e.g. Security/InfoSec}} |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | {{"Extends" or "Supersedes" the named ADR(s) — say which, and that an extended ADR stays in force; list the ADRs it builds on, linked by relative path}} |

## Context

{{What forces the decision. Ground it in the code and docs as they are now, with the gaps found in the baseline analysis (cite files). State what earlier ADRs already decided and what they left open. Say what could not be verified.}}

## Decision Drivers

- {{Driver — the property the decision must serve.}}

## Options Considered

### {{A. Decision area}}

1. **{{Option}}.** {{What it is, and why it is rejected or chosen — the real trade-off, not a strawman.}}
2. **{{Option — chosen}}.** {{...}}

{{Repeat per independent decision area. If no alternative was seriously considered, say so instead of inventing one.}}

## Decision

{{Numbered subsections, one per decision. State what is decided, with enough precision that a design cannot reasonably contradict it, and leave detail that belongs to the design explicitly to the design. Mark clearly which decisions the author made and which are recommendations awaiting confirmation.}}

## Consequences

**Positive**
- {{...}}

**Negative / trade-offs**
- {{...}}

## Open Questions

- {{Anything undecided, unverifiable, or deferred, and where it will be resolved (design, requirements, later ADR).}}

## References

- {{Related ADRs, component guides and external sources, linked.}}
```
