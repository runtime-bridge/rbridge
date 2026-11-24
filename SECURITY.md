# Security

## Reporting a problem

Please use GitHub's private vulnerability reporting on this repository ("Security" tab, "Report a vulnerability"). Do not open a public issue for security problems. You will get an answer within a week.

## What rbridge protects, and what it does not

rbridge starts agent runtimes on your machine and stands between them and the software that gives them tasks. It:

- never reads, stores or forwards provider credentials; runtimes sign in through their own flows;
- starts runtimes with an allowlisted environment;
- serves file reads and writes for ACP agents only inside the task's workspace, with symlink and `..` checks;
- applies a policy profile to every permission request and asks the caller only when the profile says so;
- refuses skills whose files changed since they were added, and withholds skill scripts unless trust allows them;
- stops the whole runtime process tree on timeout, cancel and close.

It does not sandbox the runtime. A runtime that uses its own tools without asking permission is limited only by its own rules and the operating system. rbridge has no network restriction. Each session prints which controls are enforced by rbridge, which rely on the runtime asking, and which are not enforced; the audit log records the same.

A skill's format says nothing about its safety. Read a skill (`rbridge skills show`) before trusting it.
