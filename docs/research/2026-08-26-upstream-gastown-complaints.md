# Upstream Gas Town: recurring user complaints

**Date:** 2026-08-26  
**Sources:** public first-party GitHub issues and discussions in
[`gastownhall/gastown`](https://github.com/gastownhall/gastown). No secondary
sources were used.

## Short answer

The recurring complaint is not that the idea lacks appeal; it is that the
orchestrator is difficult to trust and operate. Users most often report that
work/state can be assigned, stored, observed, or recovered inconsistently;
that creates fear of lost, duplicated, or destructive work. A smaller but
clear discussion theme is that setup and the intended delegation model are
hard to learn.

## Evidence

This is a **report-frequency sample**, not a user-satisfaction survey. I
primary-theme-coded the 50 newest open issues and 50 most-recently closed
issues retrieved on this date. Excluding seven probes, proposals, or
feature-only items left 93 concrete problem reports; each has one theme below.
Issue volume is concentrated among several audit-style reporters, so the
counts describe recurring failure modes—not the prevalence of opinions among
independent users.

| Recurring theme | Reports | What people are saying |
| --- | ---: | --- |
| Orchestration/lifecycle/recovery failures | 37 (40%) | Work can be stranded, closed too early, or repeated: dog completion is unreachable ([#4738](https://github.com/gastownhall/gastown/issues/4738)); submit closes work before merge ([#4699](https://github.com/gastownhall/gastown/issues/4699)); recovery can redo merged work ([#4698](https://github.com/gastownhall/gastown/issues/4698)). |
| Beads/Dolt/rig state and compatibility | 31 (33%) | Storage schema and scope differ across tools: the shipped `bd` cannot read a schema GT writes ([#4770](https://github.com/gastownhall/gastown/issues/4770)); rig entities can land in the town database ([#4733](https://github.com/gastownhall/gastown/issues/4733)). |
| Misleading status or hidden failure | 12 (13%) | Commands can report a healthy/successful state when they are not: failed hooks look empty ([#4632](https://github.com/gastownhall/gastown/issues/4632)); a session says it started despite exit 1 ([#4671](https://github.com/gastownhall/gastown/issues/4671)); the dashboard says “Live” but does not refresh ([#4712](https://github.com/gastownhall/gastown/issues/4712)). |
| Unsafe side effects or data loss | 9 (10%) | Automation can change or discard the wrong thing: checkpointing can commit conflict markers ([#4672](https://github.com/gastownhall/gastown/issues/4672)); cleanup can delete shared-layout rig databases ([#4604](https://github.com/gastownhall/gastown/issues/4604)); a detached `HEAD` can repoint `origin/HEAD` ([#4629](https://github.com/gastownhall/gastown/issues/4629)). |
| Docs/config/onboarding friction | 4 (4%) | Help can point at obsolete paths ([#4736](https://github.com/gastownhall/gastown/issues/4736)); a fresh install's documented repair may not work ([#4651](https://github.com/gastownhall/gastown/issues/4651)). |

The explicit end-user discussion posts reinforce a distinct **learnability**
complaint: three describe Gas Town as hard to make “just work” or as a normal
single-agent session with extra steps ([#4435](https://github.com/gastownhall/gastown/discussions/4435),
[#874](https://github.com/gastownhall/gastown/discussions/874),
[#1078](https://github.com/gastownhall/gastown/discussions/1078)). Adjacent
concerns are scope/overhead for solo work ([#624](https://github.com/gastownhall/gastown/discussions/624)),
Git and local-only-repository setup ([#266](https://github.com/gastownhall/gastown/discussions/266),
[#883](https://github.com/gastownhall/gastown/discussions/883)), and running
agents with broad filesystem authority ([#464](https://github.com/gastownhall/gastown/discussions/464)).

## Interpretation

In plain language: people want fewer moving parts, clearer “what is happening
now?” answers, and stronger guardrails before Gas Town mutates data or Git.
The most leverage is likely in dependable state/recovery and a guided,
small-project onboarding path—not more orchestration features.
