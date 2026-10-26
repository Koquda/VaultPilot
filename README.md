# VaultPilot

A local cloud-cost finance dashboard for startup infrastructure budgets. Import a billing CSV, inspect daily charges and credits, review spending spikes, and estimate month-end costs.

VaultPilot uses **Go, HTML, CSS, and SVG**. It has no JavaScript, TypeScript, Python, external packages, or third-party runtime services.

## Run locally

Install Go 1.22 or newer. From the repository directory, run:

```sh
go run ./cmd/vaultpilot
```

Open [VaultPilot on localhost](http://127.0.0.1:8080). The first run creates an explicitly labeled demo workspace in `data/workspace.json`. Stop the server with Ctrl+C. Restarting restores the saved snapshot, budgets, and spike review status.

To choose a different local port or data directory, run:

```sh
go run ./cmd/vaultpilot -addr 127.0.0.1:8081 -data ./my-workspace
```

The server accepts a loopback address only. One process can own a data directory at a time. This is a single-user local application, without accounts or shared hosting support.

## Import cloud billing data

1. Export daily costs from your cloud billing tool.
2. Convert the export to the five columns below. Headers can appear in any order.
3. Open **Imports**, select a CSV or paste its contents, and enter the inclusive coverage dates.
4. Confirm completeness only if the export includes all charges for its providers over that interval. Only confirmed missing days become zero-cost days.
5. Review the preview, then select **Replace snapshot**.

```csv
date,provider,service,cost,currency
2026-10-01,aws,EC2 Compute,74.250000,USD
2026-10-01,gcp,BigQuery,32.500000,USD
2026-10-02,aws,Service credit,-5.000000,USD
```

Providers are `aws`, `azure`, and `gcp`. Supported currencies are USD, EUR, and GBP, with one currency per file. Dates use `YYYY-MM-DD`. Costs accept up to six decimal places, without thousands separators. Credits are negative. Identical rows remain additive because separate legitimate charges can look identical.

Native AWS CUR, Azure Cost Details, and Google Cloud CSV headers are not parsed directly in this version. Choose a consistent daily cost basis when converting an export. Keep billing dates consistent, include credits once, and avoid mixing net, unblended, amortized, or converted-currency costs. No currency conversion occurs.

Imports have a 2 MB and 25,000-row limit. Invalid financial rows reject the entire file. A confirmed import replaces the current snapshot rather than appending overlapping charges. Matching budgets remain. Changed data clears stale spike review status. Back up important exports before replacing them.

## Understand the numbers

Spending and service totals include negative credits. The app stores exact integer millionths of a currency unit and rounds only for display. CSV downloads retain six decimal places. Formula-like service names receive an apostrophe prefix in exported CSVs for safer spreadsheet use.

A forecast needs confirmed continuous coverage from the beginning of the selected month and at least seven completed days. The current day is excluded. A projection is explicitly **as of the last covered complete day**, including for historical snapshots. It adds remaining calendar days at the last seven complete days' average positive charges to actual net costs. Existing credits count once. Future credits are not assumed. A fully covered month shows its actual cost. The scenario range reflects recent daily values and is not a confidence interval.

A spike compares positive charges for one provider and service with the previous seven complete days. It requires an increase greater than 50% and at least 20 units of the dataset currency. The candidate day never enters its own baseline. A new service can trigger against a zero baseline. Insufficient history does not establish that costs are normal. These are transparent rules, not a trained anomaly model.

Budgets match the selected month, provider scope, and currency. Provider budgets are independent of the all-provider budget. They are never added together. Data freshness comes from charge dates and declared coverage, not a live connection.

## Protect local data

State stays in the selected data directory and is excluded from Git. The server uses escaped HTML templates, bounded imports, CSRF-protected POST forms, Host and Origin checks, and atomic state replacement. Corrupt saved state causes a startup error and is preserved. To recover, stop the server, copy the data directory to a safe location, then start with a new directory or restore a known-good backup. This version does not encrypt the data file or provide remote authentication.

## Verify

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/vaultpilot
```

The Go packages separate billing rules, persistence, and HTTP presentation. Financial input and persisted state are validated before admission. Templates and CSS are embedded in the binary, so it can run outside the source directory after a build.

### Older Go on recent macOS

On this machine, Go 1.22.2's default linker produces a `missing LC_UUID` error. The verified local workaround uses the system C linker:

```sh
go run -ldflags=-linkmode=external ./cmd/vaultpilot
go test -race -ldflags=-linkmode=external ./...
```

This requires Apple's command-line developer tools. Use a newer compatible Go installation if the external linker is unavailable. The current file-lock implementation supports macOS and Linux.

For a standalone binary built with this older toolchain on Apple silicon, refresh its local signature after the external-link build:

```sh
go build -ldflags=-linkmode=external -o vaultpilot ./cmd/vaultpilot
codesign --force --sign - vaultpilot
./vaultpilot
```

The signing step applies to this older macOS toolchain workaround, not Linux builds. It uses an ad hoc local signature and requires no signing account.
