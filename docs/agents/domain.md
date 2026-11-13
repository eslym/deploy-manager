# Domain docs

## Before exploring

- Read root `GLOSSARY.md`.
- Read relevant decisions in `docs/adr/`.
- Follow `docs/design.md` as the confirmed requirements authority,
  with the implementation-stage direction recorded in root `AGENTS.md`.

If the glossary or ADR directory is absent, proceed silently.
The domain-modeling skill creates domain docs lazily as terminology
and decisions are resolved.

## Layout

This is a single-context repository:

- `GLOSSARY.md`: shared domain vocabulary.
- `docs/adr/`: shared architecture decision records.

## Vocabulary

Use glossary terms in issue titles, proposals, hypotheses, and test
names. Respect explicitly avoided synonyms. Flag real terminology
gaps for domain-modeling.

## Decision conflicts

Explicitly identify any proposal that contradicts an existing ADR,
including the ADR reference and why reopening it may be worthwhile.
