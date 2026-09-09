---
name: github-push
description: This skill should be used when the user asks to "push to GitHub", "push to gh", "publish commits", or "git push", or when a GitHub push fails because the shell cannot find credentials. It documents this host's persistent GitHub token and credential-helper workflow.
---

# Push commits to GitHub

Use the host's existing secret store and Git credential helper. Do not begin with
`gh auth login`: the token is persisted outside the container and interactive
login state is not.

Do not relaunch the devbox merely to repair this. The image and `~/.bashrc`
already source the shared secrets for interactive shells. Agent/tool shell
invocations are non-interactive and do not read `~/.bashrc`, so source
`secrets env --global` explicitly in the same invocation as `gh` and `git`.

## Standard workflow

1. Inspect the repository before pushing:

   ```bash
   git status --short --branch
   git remote -v
   git log -1 --oneline --decorate
   ```

2. Confirm that the requested changes are committed. Create a focused commit if
the user asked to commit or push uncommitted work; do not amend unrelated work.

3. Load the global secrets into the current command shell. Interactive agent
shells do not necessarily source them automatically:

   ```bash
   eval "$(secrets env --global)"
   ```

   This reads the shared store at
   `/home/andrew/Projects/.secrets/common.env` without printing secret values.

4. Validate GitHub authentication without exposing the token:

   ```bash
   gh auth status
   ```

   Expected output identifies the `amuldowney` account and says authentication
   is supplied by `GH_TOKEN`.

5. Push using the repository's configured HTTPS remote:

   ```bash
   git push origin "$(git branch --show-current)"
   ```

6. Verify synchronization:

   ```bash
   git status --short --branch
   git log -1 --oneline --decorate
   ```

Keep the `eval`, authentication check, and push in the same shell invocation;
exporting secrets in one tool call does not affect a later tool call.

## Credential-helper repair

The durable Git configuration normally contains a GitHub helper backed by the
GitHub CLI:

```text
credential.https://github.com.helper=
credential.https://github.com.helper=!/usr/local/bin/gh auth git-credential
```

Inspect it without showing credentials:

```bash
git config --global --show-origin --get-regexp 'credential|url\.'
```

If the GitHub helper is missing, configure it once. Resolve the installed
`gh` path rather than assuming a package location:

```bash
gh_path="$(command -v gh)"
git config --global --unset-all credential.https://github.com.helper 2>/dev/null || true
git config --global --add credential.https://github.com.helper "!$gh_path auth git-credential"
```

Then reload the secret environment and retry the standard workflow. Do not put
the token directly in a remote URL, command line, Git config, commit, or log.

## Failure handling

- `fatal: could not read Username for 'https://github.com'`: source
  `eval "$(secrets env --global)"` in the same shell, confirm `gh auth status`,
  and retry. Do not ask for a new token until the local secret store has been
  checked.
- `gh auth status` reports no account after sourcing secrets: verify that
  `secrets env --global` succeeds and that `GH_TOKEN`/`GITHUB_TOKEN` are listed
  by `secrets list` without printing values. Avoid `cat` or `echo` on the
  secret file.
- An SSH remote fails with `Permission denied (publickey)`: this host's stored
  GitHub authentication is for HTTPS. Inspect the remote and switch only with
  explicit intent; do not troubleshoot SSH by exposing or copying private keys.
- A push is rejected for non-fast-forward: stop and inspect the remote branch;
  do not force-push unless explicitly authorized.

## Safety

Never print `GH_TOKEN`, `GITHUB_TOKEN`, the contents of `common.env`, or the
output of `secrets env --global` by itself. Treat `gh auth status` as the safe
authentication check because it masks the token. Preserve the user's branch and
remote; only push after confirming the target repository and branch.
