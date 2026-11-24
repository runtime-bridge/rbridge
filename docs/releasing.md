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

Homebrew and Scoop packages need two more repositories (`rbridge/homebrew-tap`, `rbridge/scoop-bucket`) and a token secret; they are not set up yet.
