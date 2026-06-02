# aws-sso-sync

`aws-sso-sync` is a Go CLI that syncs AWS IAM Identity Center (AWS SSO) account roles into managed blocks inside one or more AWS config files.

It supports:
- Multiple JSON rule files in `~/.aws-sso-sync/configs/`
- Template-based profile naming
- Per-profile overrides using glob matching
- Safe managed blocks (`# BEGIN/END AWS-SSO-SYNC <name>`)
- Cached snapshots for offline formatting
- Change logs for added/removed profiles

## How It Works

1. Load enabled config files from `~/.aws-sso-sync/configs/*.json`.
2. Build or load a snapshot of available account/role profiles.
3. Render profile names and properties from your rules.
4. Replace only the managed block in each target file.
5. Append add/remove events to a JSONL log.

## Requirements

- Go 1.25+ (for building/running from source)
- AWS CLI v2 in `PATH` (required for live `sync` without `--source`)
- Valid AWS SSO login cache in `~/.aws/sso/cache` for each configured `startUrl` + `region`

## Install / Run

From this folder:

```bash
go run . help
```

Build a binary:

```bash
go build -o aws-sso-sync .
./aws-sso-sync help
```

Install from GitHub release assets (auto-detect OS/arch):

```bash
curl -fsSL https://raw.githubusercontent.com/Oatelaus/aws-sso-sync/main/scripts/install.sh | bash -s -- --repo Oatelaus/aws-sso-sync
```

Install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/Oatelaus/aws-sso-sync/main/scripts/install.sh | bash -s -- --repo Oatelaus/aws-sso-sync --version v1.2.3
```

Default install location is `$HOME/.local/bin`.

## GitHub Automation

This repository includes two GitHub Actions workflows:

- `.github/workflows/ci.yml`
  - Runs `go test ./...` on pushes to `main` and on pull requests.

- `.github/workflows/release.yml`
  - Triggers on tag pushes matching `v*` (for example `v1.0.0`).
  - Builds archives for:
    - linux amd64/arm64
    - darwin amd64/arm64
    - windows amd64/arm64
  - Publishes a GitHub Release with all archives and `checksums.txt`.

Create a release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The installer script expects release asset names in this format:

```text
aws-sso-sync_<version-without-v>_<os>_<arch>.<tar.gz|zip>
```

## Commands

```text
aws-sso-sync format
aws-sso-sync sync
aws-sso-sync sync --source <snapshot.json>
aws-sso-sync diff --source <snapshot.json>
aws-sso-sync list
aws-sso-sync logs
```

### `sync`

- `sync` (no source):
  - Ensures required SSO sessions are logged in
  - Fetches accounts and roles from AWS API
  - Caches snapshot to `~/.aws-sso-sync/cache.json`
  - Rewrites managed blocks in configured target files

- `sync --source <snapshot.json>`:
  - Skips AWS API fetch and loads a snapshot file directly
  - Updates cache and renders output from that source

### `format`

- Loads `~/.aws-sso-sync/cache.json`
- Re-renders target files without calling AWS APIs

### `diff --source <snapshot.json>`

- Compares cached snapshot vs another snapshot file
- Prints added/removed profiles

### `list`

- Lists discovered config files and whether each one is enabled

### `logs`

- Shows recent profile add/remove events from:
  - `~/.aws-sso-sync/logs.jsonl`

## Configuration

Config files are loaded from `~/.aws-sso-sync/configs/*.json`.

No config is created automatically. You must create one or more `.json` files yourself.

Example (`~/.aws-sso-sync/configs/default.json`):

```json
{
  "name": "zenobe",
  "enabled": true,
  "targetFile": "./example",
  "properties": {
    "Prefix": "zenobe"
  },
  "nameParts": [
    "{{ .Prefix  }}",
    "{{ .Environment }}",
    "{{ .AccountName | sanitize | lower }}",
    "{{ .RoleName | sanitize | lower }}"
  ],
  "separator": ".",
  "region": "eu-west-1",
  "startUrl": "https://zenobeenergy.awsapps.com/start",
  "overrides": [
    {
      "matchAlias": "zen-*",
      "properties": {
        "Prefix": "atlas",
        "Environment": "shared"
      }
    },
    {
      "matchAlias": "zen-*int*",
      "properties": {
        "Environment": "int"
      }
    }
  ]
}
```

### Rule Fields

- `name` (string): marker name used in managed block tokens
- `enabled` (bool): include/exclude rule file
- `targetFile` (string): output file to patch (default: `~/.aws/config`)
- `startUrl` (string): AWS SSO start URL
- `region` (string): AWS region for SSO APIs (default: `us-east-1`)
- `profileNameTemplate` (string): Go template for profile names
- `nameParts` (string[]): optional parts to render and join
- `separator` (string): joiner for `nameParts` (default: `-`)
- `properties` (map): template variables available during name rendering
- `includeAccounts` / `excludeAccounts` (string[])
- `includeRoles` / `excludeRoles` (string[])
- `overrides` (array): conditional adjustments per profile

### Overrides

Each override can match by glob pattern:
- `matchRole`
- `matchAccount`
- `matchAlias`

Then apply one or more changes:
- `profileNameTemplate`
- `property` or `properties`
- `startUrl`
- `region`

Matching uses shell-like globs (for example `prod-*`, `*Admin*`).

## Template Functions

Templates use Go `text/template` with these helper functions:

- `lower`
- `upper`
- `replace OLD NEW VALUE`
- `sanitize`

Available base variables include:

- `AccountID`
- `AccountName`
- `RoleName`
- `Alias`
- `StartURL`
- `Region`
- plus any keys from `properties`/override properties

## Managed Block Format

For each config `name`, the tool writes:

- `# BEGIN AWS-SSO-SYNC <name>`
- one or more `[sso-session ...]` blocks
- generated `[profile ...]` blocks
- `# END AWS-SSO-SYNC <name>`

Only content inside this tokenized block is owned by `aws-sso-sync`.

## Snapshot Format

You can provide snapshots with `--source`.

Example:

```json
{
  "fetchedAt": "2026-06-01T00:00:00Z",
  "profiles": [
    {
      "accountId": "111111111111",
      "accountName": "Sandbox",
      "roleName": "AdministratorAccess",
      "startUrl": "https://example.awsapps.com/start",
      "region": "us-east-1"
    }
  ]
}
```

A sample file is included at `sample-snapshot.json`.

## Logging and Cache

App data is stored in:
- `~/.aws-sso-sync/cache.json`
- `~/.aws-sso-sync/logs.jsonl`

`logs` prints up to the most recent 50 events.

## Typical Workflow

1. Create/update one or more `.json` files in `~/.aws-sso-sync/configs/`.
2. Run:

```bash
go run . sync
```

3. Review target output files.
4. Later, re-render from cache quickly:

```bash
go run . format
```

5. Compare cache to a fresh/exported snapshot:

```bash
go run . diff --source sample-snapshot.json
```

## Troubleshooting

- `aws cli not found in PATH`
  - Install AWS CLI v2 and confirm `aws --version`.

- `no valid SSO token ... run aws sso login`
  - Run SSO login for the matching `startUrl` and `region`.

- `load cached snapshot` errors on `format`
  - Run `sync` first to create `~/.aws-sso-sync/cache.json`.

- `no configuration files found in configs`
  - Add at least one `.json` rule file under `~/.aws-sso-sync/configs/`.
