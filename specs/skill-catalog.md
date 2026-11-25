# Skill catalog format

A skill catalog is a JSON document served over HTTPS that lists skills held in Git repositories. rbridge reads only the catalogs named in its configuration:

```yaml
skills:
  catalogs:
    - name: team
      url: https://skills.example.com/index.json
```

Plain `http` is accepted only for `localhost` and loopback addresses, and redirects must follow the same rule.

## Index

```json
{
  "version": 1,
  "skills": [
    {
      "id": "security-audit",
      "description": "Reviews a project for common security problems",
      "license": "Apache-2.0",
      "git": {
        "url": "https://github.com/example/skills.git",
        "commit": "4f1c0e7d9a2b3c4d5e6f708192a3b4c5d6e7f809",
        "subdir": "security-audit"
      },
      "digest": "sha256:6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b"
    }
  ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `version` | yes | Always `1` |
| `skills[].id` | yes | The skill's name: lowercase letters, digits and single hyphens, at most 64 characters. It must equal the `name` in its `SKILL.md` |
| `skills[].description`, `license` | no | Shown by `rbridge skills search` |
| `skills[].git.url` | yes | An `https` Git URL |
| `skills[].git.commit` | yes | A full commit hash (40 or 64 hex characters). Branches and tags are not accepted |
| `skills[].git.subdir` | no | The skill's folder inside the repository |
| `skills[].digest` | yes | rbridge's digest of the skill folder at that commit |

Entries that break these rules are ignored and reported. The index may be at most 4 MiB.

## Digest

The digest is the one rbridge records for every installed skill: SHA-256 over the skill's normalised file tree (sorted paths, file modes and contents). Publishers compute it with:

```sh
rbridge skills digest ./security-audit
```

When a user runs `rbridge skills add --catalog team:security-audit`, rbridge fetches the commit, computes the digest of the folder and installs the skill only if it matches. A skill installed from a catalog starts `unreviewed` like any other.
