# gh user-attachment

A `gh` extension that resolves private GitHub Enterprise user attachment URLs such as
`https://<host>/user-attachments/assets/<guid>` and downloads the attached file using
`GH_TOKEN` or `GITHUB_TOKEN`.

The extension works by:

1. Accepting either the GUID or a full user-attachment URL.
2. Searching the configured repository for the asset ID.
3. Fetching the matching issue or comment HTML from GitHub's REST API.
4. Extracting the short-lived signed object URL.
5. Downloading the object without forwarding the GitHub token to object storage.

## Why this exists

GitHub user-attachment URLs are not directly usable from an automated or CI environment.
The browser URL is a repository-scoped attachment reference, not an object URL you can
access with a raw `curl` or a generic browser session. The extension resolves the asset
through the repository metadata and then downloads the temporary signed object URL.

## Install

Install from GitHub.com:

```bash
gh extension install https://github.com/digimangos/gh-user-attachment
```

This requires a published release with precompiled binaries. If no release is
available yet, build and install from a clone:

```bash
git clone https://github.com/digimangos/gh-user-attachment.git
cd gh-user-attachment
go build -o gh-user-attachment .
gh extension install .
```

If you host your own copy on GitHub Enterprise, replace `owner` with its owner:

```bash
gh extension install https://github.example.com/owner/gh-user-attachment
```

## Usage

```bash
export GH_HOST=github.example.com
export GH_TOKEN=...

gh user-attachment \
  --repo owner/repository \
  --host github.example.com \
  --api-url https://api.github.example.com \
  --output /tmp/copilot-user-attachment \
  https://github.example.com/user-attachments/assets/00000000-0000-4000-8000-000000000000
```

You can also pass just the GUID:

```bash
gh user-attachment --repo owner/repository 00000000-0000-4000-8000-000000000000
```

Without `--output`, the attachment is saved in the current directory as
`<GUID>.<extension>`, for example
`00000000-0000-4000-8000-000000000000.png`. The extension is derived from the
final HTTP response's `Content-Type` after following redirects, not from the
source URL. Media types are parsed case-insensitively and parameters such as
`charset` are removed. Aliases such as `image/jpg` and `document/pdf` are
normalised to `image/jpeg` and `application/pdf`. JPEG uses `.jpg`; PNG, GIF,
WebP and PDF use `.png`, `.gif`, `.webp` and `.pdf`. Common documents, archives,
text files, audio and video are also supported using the MIME-to-extension
mapping. Generic `application/octet-stream` downloads use `.bin`. HTML and XHTML
responses are rejected to avoid saving sign-in pages. An unrecognised content
type requires an explicit `--output` filename.

The success message reports the absolute path, content type and downloaded size.
An explicit `--output` path is used exactly as supplied, without appending or
changing its extension. Downloads are no longer written as binary data to stdout
when `--output` is omitted.

## Copilot usage

This extension is intended to be called by Copilot in a repository with the following pattern:

```bash
repo="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
gh user-attachment \
  --repo "$repo" \
  --output /tmp/copilot-user-attachment \
  "<complete-user-attachments-url>"
```

Use the downloaded local file for image inspection, OCR, or further analysis.

## Release packaging

Pushing a semantic version tag such as `v0.2.0` runs the official
`cli/gh-extension-precompile` action. It publishes binaries for supported
operating systems and architectures using the asset naming convention expected
by GitHub CLI.

Once a release is published, install a specific build with `--pin`:

```bash
gh extension install https://github.com/digimangos/gh-user-attachment --pin v0.2.0
```

Upgrade to another explicit build:

```bash
gh extension upgrade user-attachment --pin v0.3.0
```
