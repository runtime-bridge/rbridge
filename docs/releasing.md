# Releasing

1. `make lint race cross licenses` on a clean checkout of `main`.
2. Run the compatibility suite on a machine with the runtimes signed in, then copy the results into `docs/compatibility.md`:

   ```sh
   RBRIDGE_IT_PROVIDERS=claude-code,opencode \
   RBRIDGE_IT_MODELS=claude-code=haiku,opencode=ollama/qwen3:8b \
   make compat
   ```

3. Move the `CHANGELOG.md` entry from "unreleased" to the version and date.
4. Tag and push: `git tag -s vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`. The release workflow builds the six platform archives with GoReleaser, writes `checksums.txt` and signs it with cosign (keyless, GitHub OIDC).
5. Check the release page, then run `scripts/install.sh vX.Y.Z` on a clean machine.

Homebrew and Scoop packages need two more repositories (`runtime-bridge/homebrew-tap`, `runtime-bridge/scoop-bucket`) and a token secret; they are not set up yet.

## SDK packages with rbridge inside

After an rbridge release, the SDKs can publish packages that carry it, so `pip install rbridge` or `npm install @rbridge/sdk` brings a per-project copy:

1. In each SDK repository, put the rbridge tag in `RBRIDGE_VERSION` and raise the package version.
2. Push a `v*` tag there. Each SDK's release workflow checks out rbridge at that tag, runs `make cross`, and builds the packages: seven wheels for Python (six platforms and a pure one), and for Node.js six platform packages plus the SDK that lists them as optional dependencies.
3. One-time setup: on PyPI, add `runtime-bridge/rbridge-sdk-python` (workflow `release.yml`, environment `pypi`) as a trusted publisher of the `rbridge` project. On npm, create the `rbridge` organisation and add an `NPM_TOKEN` secret to `runtime-bridge/rbridge-sdk-ts`.

Each SDK's CI already builds the packages on every push and installs the one for Linux and macOS into a fresh project, so a release only publishes what CI has run.
