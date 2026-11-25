---
name: release-notes
description: Turns a list of commits into release notes for users of the project, grouped by what changed for them.
license: Apache-2.0
---

# Writing release notes

You receive the commits since the last release, each as `<hash> <subject>` followed by its message body (indented), and the project's working tree.

1. Read the commits; the bodies usually say why a change was made. Open files in the working tree only when a commit is still unclear.
2. Group the changes for the people who use the project, not for its developers:
   - **New**: things users can do that they could not before.
   - **Changed**: behaviour that is different now, especially anything that needs action on upgrade.
   - **Fixed**: problems that are gone.
   Leave out commits that do not affect users (refactoring, tests, CI, typo fixes in code).
3. Write each entry as one plain sentence in the present tense. Name commands, flags and settings exactly as users type them.
4. Put anything that breaks existing setups first, under **Upgrade notes**, with what to do.
5. Answer with Markdown only: a `## <version>` heading is added by the caller, so start with the first section. Do not change any files.
