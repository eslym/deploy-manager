---
name: ux-unslop
description: Audit and improve AI-generated user interfaces by removing developer-facing language, leaked implementation abstractions, unnecessary explanations, and specification-driven UI clutter. Use when building, reviewing, or refining UI copy, flows, empty states, settings, navigation, errors, confirmations, or onboarding.
---

# UX-unslop

Make the interface speak the user's language, not the codebase's language.

## Mission

Detect and remove **UX slop**: UI that is technically accurate and superficially professional but confusing, unnatural, overly explanatory, or organized around internal implementation concepts instead of user tasks.

The goal is **not** merely to simplify terminology. It is to keep internal abstractions, implementation details, and product constraints from leaking into the user's mental model.

## When to apply

Apply this skill whenever you:
- Build or revise pages, navigation, dialogs, forms, settings, onboarding, empty states, notifications, errors, or success messages.
- Translate requirements, PRDs, schemas, API names, or code entities into UI.
- Review AI-generated UI for unnecessary labels, descriptions, jargon, or status indicators.
- Refactor existing products where users must understand technical implementation to operate the interface.

Apply proactively during UI work, not only after the user asks for a copy audit.

## Core principles

1. **User concepts over internal concepts.** Names in code, schemas, tickets, and architecture are not automatically product vocabulary.
2. **Tasks over mechanisms.** Describe what users can do and what happened, not the mechanism implementing it.
3. **Don't explain absent complexity.** If users cannot perform an unsupported operation, avoid explaining that restriction unless it changes a real decision or expectation.
4. **Don't render the PRD.** Requirements describe system behavior; they are not a checklist of UI text or visible elements.
5. **Delete before rewriting.** When text exists only to explain an irrelevant implementation detail, remove it rather than paraphrasing it.
6. **Clarity over performative professionalism.** Formal, verbose, or technical language is not intrinsically more trustworthy.
7. **Preserve necessary precision.** Do not remove domain terms that users actually know or need; do not hide significant limitations, security consequences, data loss, or actionable errors.

## Workflow

### 1. Establish the user mental model

Before writing UI, identify from available context:
- **User:** Who uses the feature? What terms do they already know?
- **Goal:** What are they trying to accomplish?
- **Objects:** What things do they recognize and manipulate?
- **Actions:** What verbs describe their intent?
- **Consequences:** What information is necessary to make a decision or recover from failure?

Do not infer product vocabulary solely from type names, database columns, component names, API endpoints, or technical specifications. If evidence is missing, prefer plain conventional language and avoid inventing branded or architectural jargon.

### 2. Separate internal and public vocabulary

Build a short internal-to-user mapping before implementing UI. Keep it as working notes; do not display it in the product.

| Internal concept | User-facing concept | Visibility decision |
|---|---|---|
| `FlatGroup` / flat group | Group | Rename |
| `MembershipAssociation` | Member | Rename |
| `NestedGroupRestriction` | — | Omit unless relevant to an attempted task |
| `persisted` | Saved | Show only when confirmation helps |
| `sync job` | — / Up to date | Omit mechanism; show user-relevant state if needed |

A mapping is context-dependent, not a global ban on words such as “nested”, “record”, or “instance”. Some audiences genuinely use them.

### 3. Audit every visible string and UI element

Inspect headings, tabs, buttons, labels, helper text, tooltips, placeholders, empty states, onboarding, alerts, errors, success notices, settings, badges, and dashboard cards.

For each element, ask:
1. Would a first-time target user understand this without reading the PRD or code?
2. Is this a user's concept or an implementation concept?
3. Does this help someone decide, act, understand an outcome, or recover?
4. If removed, would the user lose necessary information?
5. Is the UI compensating with explanation for a confusing interaction?

Classify findings:
- **LEAK:** Internal term or architectural distinction appears in UI.
- **SPEC-DUMP:** A requirement or limitation is displayed with no user need.
- **MECHANISM:** Technical operation presented instead of user outcome.
- **CEREMONY:** Redundant explanatory text, status, badge, or confirmation.
- **MISMODEL:** Information architecture mirrors implementation rather than user tasks.
- **VALID:** Domain-specific or technical language genuinely appropriate for the audience.

### 4. Decide: remove, rename, relocate, or retain

Use this order of preference:
1. **Remove** text or elements that have no user-facing purpose.
2. **Make interaction self-evident** so explanation is unnecessary.
3. **Rename** using familiar nouns and action verbs.
4. **Relocate** advanced detail to contextual help or documentation when some users need it.
5. **Retain** precise language when necessary for informed decisions, safety, accessibility, compliance, or task completion.

Do not simplify an error so far that it loses the corrective next step. Do not silently remove consequential limitations users need to know about.

### 5. Implement and verify

- Update UI strings **and** the surrounding flow if the problem is structural.
- Keep internal code identifiers unchanged unless refactoring them is independently justified. UI vocabulary and code vocabulary may differ.
- Match existing product terminology and tone across screens.
- Check localization readiness, screen-reader labels, and ambiguity caused by overly short copy.
- Review the finished UI as someone unfamiliar with the implementation.
- When possible, test with realistic user tasks: Can users predict what a control does? Can they identify the next action? Can they recover from an error?

## Examples

### Flat group: implementation abstraction leak

**Internal model:** Groups can contain members but not subgroups. Engineers call them “flat groups”.

| Context | Slop | Better |
|---|---|---|
| Page title | Flat Group Management | Groups |
| CTA | Create Flat Group | Create group |
| Empty state | No flat groups configured | No groups yet |
| Member action | Manage membership associations | Add members |
| Success | Flat group entity created successfully | Group created |
| Restriction banner | Nested groups are not supported | Remove the banner if the UI never offers nesting |

**Exception:** If a user imports a group structure containing subgroups, explain the specific incompatibility and what to do: “This file contains groups within groups. Move members into separate groups before importing.”

### Technical state versus useful state

| Slop | Better approach |
|---|---|
| Persistent storage enabled | Omit; or “Changes are saved automatically” if this resolves genuine uncertainty |
| Database record updated successfully | “Changes saved” or an unobtrusive saved state |
| Role-based access control configured | “Who can edit this?” / “Editors” depending on task |
| Real-time synchronization active | Show “Up to date” only when sync state matters |
| Operation failed: foreign key violation | Explain what couldn't be done and the specific recovery action |

### PRD-to-UI overproduction

**Requirement:** “The system shall prevent nested groups.”

**Wrong inference:** Add a “Flat structure” badge and a paragraph explaining the absence of nested groups.

**Correct inference:** Do not offer “Add subgroup” actions. Explain only when a user realistically encounters the constraint (e.g., an incompatible import, migration, or API error).

## Anti-patterns to catch

- Technical qualifiers copied into titles: “flat”, “atomic”, “canonical”, “persisted”, “normalized”, “non-hierarchical”.
- Code nouns converted into UX: “entity”, “resource”, “payload”, “association”, “instance”, “record”.
- Internal process surfaced as copy: “initialize”, “execute”, “synchronize”, “hydrate”, “provision”.
- Help text that merely repeats the field label.
- Success toasts for every trivial interaction.
- Status badges that announce implementation characteristics instead of actionable state.
- Cards or settings invented solely because a concept appears in requirements.
- Navigation that mirrors database tables, microservices, or architecture modules.
- Repeated warnings about impossible actions.
- Oversimplification that conceals real consequences or makes two distinct user actions ambiguous.

These are detection cues, **not** unconditional forbidden-word lists.

## Output contract for an audit

When reviewing UI, provide concise, actionable findings. For each substantial issue include:
- **Location:** Screen / element / string.
- **Issue:** What leaks or confuses, and why.
- **Action:** Remove / rename / relocate / restructure / retain.
- **Replacement:** Exact user-facing copy where applicable.

Prioritize by user impact:
1. Navigation and core task comprehension.
2. Actions, permissions, errors, and irreversible consequences.
3. Forms, empty states, and onboarding.
4. Cosmetic wording and redundant helper text.

If editing a repository, make the actual changes when authorized; do not stop at generic advice. Summarize important before/after decisions. Avoid introducing new design elements just to demonstrate the audit.

## Final acceptance checklist

- [ ] No internal identifier or architectural qualifier appears merely because it exists in code or a PRD.
- [ ] Each visible element serves a recognizable user task, decision, state, or recovery need.
- [ ] Unsupported features are not advertised through unnecessary limitation notices.
- [ ] Buttons use action-oriented language and feedback describes user-visible results.
- [ ] Jargon is validated against the target audience rather than categorically banned.
- [ ] Necessary warnings, constraints, and error recovery information remain clear.
- [ ] Copy and information architecture are consistent across the product.
- [ ] Removing redundant explanations has not reduced accessibility or discoverability.

**Guiding question:** “Would this UI still make sense to someone who has never seen our architecture, database, PRD, or source code?”
