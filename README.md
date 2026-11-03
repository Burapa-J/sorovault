#SoroVault

[![CI](https://github.com/soroworks/sorovault/actions/workflows/ci.yml/badge.svg)](https://github.com/soroworks/sorovault/actions/workflows/ci.yml)
[![Security](https://github.com/soroworks/sorovault/actions/workflows/security.yml/badge.svg)](https://github.com/soroworks/sorovault/actions/workflows/security.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/soroworks/sorovault/badge)](https://api.securityscorecards.dev/projects/github.com/soroworks/sorovault)
[![Release](https://img.shields.io/github/v/release/soroworks/sorovault)](https://github.com/soroworks/sorovault/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/soroworks/sorovault)](go.mod)
[![License](https://img.shields.io/github/license/soroworks/sorovault)](LICENSE)

A contract metadata and interface (ABI) registry for the Stellar/Soroban network.

SoroVault fetches a deployed contract's WASM, decodes the interface embedded in
it, stores it, and serves a searchable registry — so tools and humans can
discover what functions and types a contract exposes.

**Status:** MVP. The core is complete and tested; see [open ground](#open-ground)
for where contributions are most welcome.

---

# # Why

A deployed Soroban contract is a black box unless you have its interface.

The interface is right there in the uploaded WASM — Soroban embeds it as a
`contractspecv0` custom section — but getting at it means knowing which ledger
entries to read, how to walk a WASM binary, and how to decode a stream of XDR.
Every tool that wants to know what a contract does re-implements that.

SoroVault does it once and serves the result as clean JSON:

```console
$ curl -s localhost:8080/api/contracts/CDZZ…4PAN | jq '.interface.functions[].name'
"vote"
"has_voted"
"initialize"
"get_proposal"
"get_total_votes"
"get_vote_params"
"get_proposal_count"
```

It is the discovery layer for contract interfaces, and the natural companion to
a deployer (SoroForge) and a simulator (SoroProbe).

##What it does

- **Register** a contract by ID — fetch its WASM, decode the interface, store it
- **Search** the registry by contract ID or name
- **Serve** the decoded ABI as JSON: functions, arguments, return types, UDTs, events
- **Version** interfaces — when a contract is upgraded, the prior interface is kept
- **Find the contracts that share a WASM** — the question a vulnerability in a
  shared piece of code raises
- **Browse** it all in a small server-rendered UI, with a permalink for every
  function, type and event and a form that registers a contract while you browse

Records are namespaced by network, so the same contract ID on testnet and
mainnet are separate entries.

## Verified against

Contract spec handling evolves, so these were confirmed against live sources
rather than recalled. Verified **2026-08-07**:

| | Version | Note |
|---|---|---|
| Go | **1.25** | required by the SDK; `go.mod` pins `1.25.0` |
| [`github.com/stellar/go-stellar-sdk`](https://github.com/stellar/go-stellar-sdk) | **v0.7.1** | RPC client and XDR |
| Stellar RPC (testnet) | 27.1.1, protocol 27 | endpoint the decode path was exercised against |
| soroban-sdk (test fixture) | 25.3.0 | the contract in `internal/spec/testdata` |
| [SEP-48](https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0048.md) | 1.1.0 | contract interface specification |

> **On the SDK:** the original plan named `github.com/stellar/go`. That module is
> now formally deprecated — its `go.mod` carries
> `// Deprecated: Use github.com/stellar/go-stellar-sdk instead` — so SoroVault
> builds against the successor. This is also why the Go floor is 1.25 rather
> than the 1.22 originally targeted: the new SDK requires it.

### How the interface is actually retrieved

Worth writing down, because it is the part that is easy to get wrong:

1. **Contract instance.** Build a `LedgerKey` for `ContractData` with the
   contract's `ScAddress`, the key `ScvLedgerKeyContractInstance`, and
   `Persistent` durability. `getLedgerEntries` returns a `ContractDataEntry`
   whose `Val.Instance.Executable.WasmHash` names the module.
   - A Stellar Asset Contract has no `WasmHash` — it is built into the host, so
     there is no uploaded interface to decode. SoroVault recognizes the on-ledger
     `CONTRACT_EXECUTABLE_STELLAR_ASSET` discriminant and serves the documented
     built-in interface under the stable identifier
     `builtin:stellar-asset-contract:v1`. Its interface JSON is marked
     `builtin: true` and cites the [official SAC specification](https://developers.stellar.org/docs/tokens/stellar-asset-contract).
     Registering an SAC now returns its interface instead of the previous HTTP
     422 response; `wasm_hash` is omitted from the contract interface response
     and `interface_id` identifies the built-in definition.
2. **Contract code.** Build a `LedgerKey` for `ContractCode` keyed by that hash.
   `getLedgerEntries` returns a `ContractCodeEntry` holding the module bytes.
   The hash is the SHA-256 of the module, which is what makes it a usable
   version identifier.
3. **Spec section.** Walk the WASM's section framing for the custom section
   named `contractspecv0`.
4. **Decode.** Its body is a **bare stream of XDR-encoded `SCSpecEntry` values**
   — no header, no delimiter, no length prefix between entries. Read it by
   calling `xdr.Unmarshal` in a loop until the reader is drained.
5. **Metadata.** The optional `contractmetav0` section holds `SCMetaEntry`
   key/value pairs (`rsver`, `rssdkver`). Its absence is not an error.
6. **Environment metadata.** The optional `contractenvmetav0` section holds the
   `SCEnvMetaEntry` that records which host interface the contract was built
   against — effectively the minimum protocol it needs. Its absence is not an
   error either.

The SDK does not parse WASM custom sections, so `internal/wasm` implements the
small amount of the binary format that step 3 needs.


### Link previews and sitemaps

Set `PUBLIC_URL` to the externally reachable origin (for example,
`https://registry.example.org`) to enable the XML sitemap at `/sitemap.xml`
and canonical Open Graph URLs on contract pages. Contract pages include
an escaped description with the network, number of functions, and first
five function names. Sitemaps use `last_refreshed` for `<lastmod>`, and
registries with over 49,999 contracts use a sitemap index that links to
bounded pages of at most 50,000 URLs. When `PUBLIC_URL` is unset the
sitemap is unavailable (404).

## Quickstart

The fastest path is Docker, which brings up Postgres, applies migrations and
starts the server:

```console
$ git clone https://github.com/soroworks/sorovault && cd sorovault
$ make up
```

Then register a contract and browse it:

```console
$ curl -X POST localhost:8080/api/contracts \
    -H 'content-type: application/json' \
    -d '{"contract_id":"CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"}'
```

Open <http://localhost:8080> for the browse UI.

To run a published image instead of building from source, set
`SOROVAULT_IMAGE`; `make up` still builds locally because it passes `--build`:

```console
$ SOROVAULT_IMAGE=ghcr.io/soroworks/sorovault:1.2.3 docker compose up
```

### Verifying a release

Each release publishes `checksums.txt`, an SPDX SBOM for every archive, and a
build-provenance attestation. Verify the archive you downloaded and match it
to the SBOM before running it:

```console
$ gh attestation verify sorovault_1.2.3_linux_amd64.tar.gz \
    --repo soroworks/sorovault
$ sha256sum -c checksums.txt --ignore-missing
$ jq '.packages[] | {name, version}' sorovault_1.2.3_linux_amd64.tar.gz.sbom.json
```

Container images are published as `ghcr.io/soroworks/sorovault:<version>` and
`:latest` for `linux/amd64` and `linux/arm64`.

### Kubernetes with Helm

The chart at [`deploy/helm/sorovault`](deploy/helm/sorovault) deploys the API
and runs database migrations before install and upgrade. It expects an
existing PostgreSQL database; it does not install or manage PostgreSQL. Create
a Secret containing the connection URL, then install the chart:

```console
$ kubectl create secret generic sorovault-db \\
  --from-literal=DATABASE_URL='postgres://sorovault:password@postgres:5432/sorovault?sslmode=disable'
$ helm upgrade --install sorovault ./deploy/helm/sorovault \\
  --set database.existingSecret=sorovault-db
```

The secret name and key, image, service, and SoroVault environment settings can
be overridden in a values file. `RPC_URL`, `NETWORK_PASSPHRASE`, `HTTP_ADDR`,
`LOG_LEVEL`, `RPC_TIMEOUT`, and `REFRESH_INTERVAL` are exposed under `config`;
use `extraEnv` for other environment variables. The server and migration Job
run non-root with read-only root filesystems.

For production deployment — sizing, backups, upgrades and monitoring — see
[docs/operations.md](docs/operations.md).

### Running locally

For single-user or local deployments, point `DATABASE_URL` at a SQLite file — no external database server or C toolchain is needed (`modernc.org/sqlite` is pure Go):

```console
$ export DATABASE_URL=sqlite:///tmp/registry.db
$ go run ./cmd/sorovault migrate
$ go run ./cmd/sorovault add CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN
$ go run ./cmd/sorovault serve
```

Or run against Postgres via Docker Compose:

```console
$ cp .env.example .env          # adjust as needed
$ set -a && source .env && set +a

$ docker compose up -d db       # or point DATABASE_URL at your own Postgres
$ go run ./cmd/sorovault migrate
$ go run ./cmd/sorovault add CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN
$ go run ./cmd/sorovault serve
```

## Configuration

All configuration is environment variables. Only `DATABASE_URL` (or the path in `DATABASE_URL_FILE`, read from the filesystem) is required.

### A YAML file as the base layer

As the settings list grows, a reviewed file is easier to manage than a wall
of exports. Pass `--config sorovault.yaml` (the `CONFIG_PATH` env var works
too) and the file's values sit under the environment: the environment
overrides the file setting by setting — an operator who exported a
variable did so deliberately, usually in a container — and defaults still
apply below both. The file's keys are these same env-var names,
case-insensitively, so `http_addr:` and `HTTP_ADDR:` are the same key, and
the types YAML gives the document (`read_only: true`, `rpc_timeout: 30s`)
read as the settings they look like. A key that is not one of the names
in the table above is an error naming the key, never a setting silently
ignored, so a spelling slip cannot "work" by doing nothing.

```yaml
http_addr: ":8080"
rpc_timeout: 30s
refresh_interval: 6h
read_only: true
cors_origins: "https://dashboard.example.com"
```

Run `sorovault config show` to print the effective configuration — file,
environment and defaults merged — with the secrets redacted: the
connection string keeps its host and loses its password, tokens report
only how many are configured, and the webhook secret is not even shaped
back out of what it became. `--json` prints the same rows for scripts.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | — | Postgres connection string (`postgres://…`) or SQLite file URL (`sqlite:///path/registry.db`). **Required** (or set `DATABASE_URL_FILE` to read it from a file). |
| `MAX_CONCURRENT_REGISTRATIONS` | `4` | how many registrations and refreshes run at once |
| `DATABASE_MAX_CONNS` | pgx's own (the greater of 4 or the CPU count) | most connections the pool holds open |
| `DATABASE_MIN_CONNS` | `0` | connections the pool keeps open between requests |
| `RPC_URL` | `https://soroban-testnet.stellar.org` | Stellar RPC endpoint |
| `NETWORK_PASSPHRASE` | `Test SDF Network ; September 2015` | the network you expect `RPC_URL` to serve |
| `HTTP_ADDR` | `:8080` | listen address |
| `TLS_CERT_FILE` | — | PEM certificate path; set with `TLS_KEY_FILE` to serve HTTPS (`--tls-cert` overrides) |
| `TLS_KEY_FILE` | — | PEM private key path; set with `TLS_CERT_FILE` (`--tls-key` overrides) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | `text` for people, `json` for one parseable object per line |
| `RPC_TIMEOUT` | `30s` | bound on a single contract fetch |
| `REFRESH_INTERVAL` | `0` (off) | re-check registered contracts this often while serving; minimum `1m` |
| `REFRESH_WEBHOOK_URL` | — | post a JSON message here whenever a refresh finds an upgraded contract |
| `REFRESH_WEBHOOK_SECRET` | — | sign webhook payloads with HMAC-SHA256 |
| `REFRESH_EVENTS` | `false` | follow the network's upgrade events and refresh a contract as soon as it is upgraded; needs `REFRESH_INTERVAL` |
| `REFRESH_EVENTS_INTERVAL` | `10s` | how often new upgrade events are read; minimum `2s` |
| `SIGNING_KEY` | — | sign interface responses with this Ed25519 key (base64 32-byte seed; or `SIGNING_KEY_FILE`). `sorovault keygen` makes one |
| `SIGNING_RETIRED_KEYS` | — | comma-separated base64 public keys of earlier signing keys, kept at `GET /api/keys` so old signatures stay verifiable |
| `RATE_LIMIT_PER_MINUTE` | `30` | registrations and refreshes allowed per client per minute |
| `RATE_LIMIT_BURST` | `10` | how many of those may arrive at once |
| `TRUST_X_FORWARDED_FOR` | `false` | use the first `X-Forwarded-For` address as the client; only behind a trusted proxy |
| `CORS_ORIGINS` | — | comma-separated exact browser origins, or `*` for read-only routes only |
| `READ_ONLY` | `false` | refuse every write endpoint with 405; reads and automatic refreshes stay on (`--read-only` overrides) |
| `API_TOKENS` | — | comma-separated bearer tokens the write endpoints accept; unset keeps writes public |
| `CONFIG_PATH` | — | YAML config file whose values sit under the environment (`--config` names it explicitly) |

`NETWORK_PASSPHRASE` is a guard, not a source of truth. SoroVault reads the real
passphrase from the RPC endpoint and refuses to proceed if the two disagree, so
a stale `RPC_URL` cannot quietly file mainnet contracts under `testnet`.

### Automatic refresh

Set `REFRESH_INTERVAL` (for example `6h`) and `sorovault serve` re-checks, on
that schedule, every contract on its network that has not been refreshed
within the interval. An upgraded contract gets a new interface version
exactly as `POST /api/contracts/{id}/refresh` would record it; an unchanged
one costs a single `getLedgerEntries` call and is not re-downloaded. Refreshes
are spread at even offsets across the interval to avoid an RPC traffic burst.

A rate-limited RPC response pauses the sweep and retries that contract with
exponential backoff, capped at 16 seconds. After five retries, the sweep stops
and leaves remaining contracts stale for the next scheduled sweep; the rate
limit is logged once rather than once per contract.

A contract that fails to refresh — expired, or an RPC error — is logged and
skipped, and the sweep carries on. One sweep runs at startup and then one per
interval; a sweep in progress at shutdown is cancelled before the database is
closed.

### Refreshing on upgrade events

Polling finds an upgrade up to a whole interval late, and re-checks every
contract to find the few that changed. When a contract replaces its code, the
network emits a `system` event with the topics `executable_update`, the old
executable and the new one. Set `REFRESH_EVENTS=true` and `serve` follows those
events through `getEvents`, and refreshes just the registered contracts they
name, within `REFRESH_EVENTS_INTERVAL` of the upgrade. Events about contracts
nobody registered cost no RPC work, and a contract upgraded several times
between two reads is refreshed once.

Polling does not go away: `REFRESH_INTERVAL` is required alongside, and keeps
sweeping as a slow safety net for whatever an event might miss (a refresh that
failed, for one).

The position read up to is saved in the database after each page of events, so
a restart resumes where it stopped. Two things leave no usable position, and
both are answered with a full sweep instead of a guess:

- the first run, which has no saved position; and
- a saved position older than the RPC's retention (the RPC keeps about a week
  of ledgers), because upgrades in the gap can no longer be read.

In both, the position is moved to the newest ledger *before* the sweep starts,
so an upgrade that lands during the sweep is read by the next poll.

### Read-only mode

Set `READ_ONLY=true` (or pass `--read-only`) and serve refuses every write
endpoint with **405** — registrations, refreshes, source and deprecation
edits, subscriptions. The routes stay mounted, so a writer is told the
method is refused rather than sent a 404 that reads as an unknown resource;
`GET /api/stats` reports the mode under `read_only`. Reads keep working,
and the automatic refresher keeps the mirror current on its `REFRESH_INTERVAL`
schedule, since a sweep is not subject to the HTTP restriction. The browse UI
hides its register form. A common pairing: `REFRESH_INTERVAL=6h READ_ONLY=true`
publishes a registry that stays current but takes nothing new from the public.

### Restricting writes to token holders

Set `API_TOKENS` to one or more comma-separated tokens and every write
endpoint — registering (single and batch), cataloguing WASM, refreshes, source
and deprecation edits, subscriptions — requires `Authorization: Bearer
<token>`. Reads stay public: a registry's value is that anyone can browse it,
and the mode still honors `READ_ONLY` if both are set (405 wins, since a
read-only server refuses writes anyway). Each token must be at least 16
characters (they are secrets: `openssl rand -hex 32` or equivalent) and no
two may repeat; the server refuses to start otherwise. Configure several
tokens, hand one to each tool that needs to write, and rotate by adding a
new token before removing the old.

Tokens are compared in constant time and never logged or echoed. Note the
boundary honestly: the browse UI has no way to carry a bearer token, so its
register form is disabled while tokens are configured — use the JSON API for
writes. `GET /api/stats` stays public and is unaffected.

### Telling the world about an upgrade

Set `REFRESH_WEBHOOK_URL` and every contract whose interface changes during a
sweep is posted to it, one message per contract:

```json
{
  "contract_id": "CDZZ…4PAN",
  "network": "testnet",
  "old_wasm_hash": "c618da…0781",
  "new_wasm_hash": "9f2b71…4c3a",
  "interface_url": "/api/contracts/CDZZ…4PAN?network=testnet&wasm_hash=9f2b71…4c3a",
  "changed_at": "2026-08-07T12:00:00Z"
}
```

`interface_url` is a path, not an absolute URL: the instance does not know
the address it is served under, so it says where the interface is and lets
the receiver put its own prefix on it.

Delivery is best effort. The webhook is posted to once, with a five-second
timeout and no retries: a receiver that is down must not be able to stall the
sweep, and the sweep's job is to keep the registry up to date rather than to
keep a receiver informed. A delivery that fails is logged.

Set `REFRESH_WEBHOOK_SECRET` to sign the payloads. The signature is the
HMAC-SHA256 of the raw request body, keyed with the secret, hex-encoded and
sent in the `X-SoroVault-Signature` header:

```
X-SoroVault-Signature: sha256=6f3a…
```

A receiver recomputes it over the body it received and compares the two in
constant time. A mismatch means the payload was tampered with, or did not
come from SoroVault at all.

### Who deployed a contract

On registration SoroVault works out which account created the contract and in
which ledger, and stores both. They appear in the API (`origin` in
`GET /api/contracts/{id}`, `creator` and `created_ledger` in listings) and on
the contract page. When they cannot be determined the API says so
(`"origin": {"known": false}`) and the page says "Unknown"; a missing creator
is never shown as an empty one.

The creator is found from the RPC's own history: the instance entry says which
ledger last modified it, and if that ledger holds a transaction that *created*
the entry, that transaction's source account (or the operation's, when it has
its own) is the creator. This has two limits, both deliberate:

- The RPC keeps about a week of ledgers, so a contract older than that cannot
  be traced.
- A contract modified since it was created — upgraded, or with instance
  storage written — has a "last modified" ledger that is no longer its
  creation. SoroVault reports it as unknown rather than guess, because
  publisher verification (below) trusts this value.

Horizon, which keeps full history, could answer both cases, but it has no
lookup by contract; finding a creation there means scanning operations. The
lookup sits behind `stellar.OriginFinder`, so a Horizon-backed finder can be
added without touching the registry.

`sorovault backfill-origins` fills in contracts registered before this existed,
or whose lookup failed. It only looks at contracts still without a creator, so
it is safe to run repeatedly.

### Verified publishers

Anyone can register a contract and attach a source link, so a listing alone
does not say who stands behind it. A publisher who controls the account that
deployed a contract can prove it by signing a claim:

```console
$ export SOROVAULT_PUBLISHER_SECRET=S…     # the deploying account's secret seed
$ sorovault claim sign CDZZ…4PAN --name "ZK Vote" \
    --source-url https://github.com/example/zkvote \
  | curl -X POST -H 'content-type: application/json' -d @- \
    https://registry.example/api/contracts/CDZZ…4PAN/verify
```

`claim sign` signs locally and prints the request body; the seed is read from
the environment (never a flag) and is never sent anywhere. The signed message is
`sorovault-claim-v1\n` followed by the [canonical JSON](#signed-attestations) of
`{v, network, contract_id, name, source_url, source_commit?, wasm_hash?,
issued_at}`, signed with the account's Ed25519 key, so any Stellar SDK can
produce it.

The registry accepts a claim when all of these hold, and otherwise refuses it
with the reason:

| Check | Refusal |
|---|---|
| well formed, and for this registry's network | `400` |
| the signature verifies against the signing account | `400` |
| issued within the last 15 minutes (and not in the future) | `422` stale |
| if it pins a `wasm_hash`, the contract still runs that version | `422` stale |
| the deployer is known | `422` |
| the signer is the account that created the contract | `403` |

The signature is checked before the registry is consulted, so nobody can probe
what is registered without a valid signature. The 15-minute window is what stops
a captured claim being replayed later.

On success the contract is marked verified and takes the claim's name and source.
The API reports `verified: {publisher, verified_at, wasm_hash, current}`, where
`current` goes false if the contract is upgraded afterwards (the publisher
vouched for an earlier version), and the UI shows a badge.

The deployer is the only relationship accepted. A contract's "admin" is a
convention of each contract, not something the network records, so honouring it
would mean guessing at storage layouts. A publisher whose deploying account
differs from the one they sign with can sign with the deployer.

### Signed attestations

A client that trusts SoroVault's decoded interfaces has to trust the transport
and the server. With a signing key, every interface response says which
registry produced it, in a way that survives caches and mirrors.

```console
$ sorovault keygen > signing.key          # seed on stdout; key id and public key on stderr
$ SIGNING_KEY_FILE=signing.key sorovault serve
```

`GET /api/contracts/{id}` and `GET /api/wasm/{hash}` then carry two headers:

```
X-SoroVault-Attestation: <base64 Ed25519 signature>
X-SoroVault-Key-Id: 65b60673d6ed884b
```

and `GET /api/keys` publishes the public keys to verify against (the current key
first, then any in `SIGNING_RETIRED_KEYS`). To rotate: generate a new key, move
the old public key into `SIGNING_RETIRED_KEYS`, and set `SIGNING_KEY` to the new
seed; old signatures keep verifying and new responses use the new key.

The signature covers the string `sorovault-attestation-v1\n` followed by the
canonical JSON of

```json
{"contract_id": "C…", "interface": { … }, "network": "testnet", "v": 1, "wasm_hash": "…"}
```

built from the response body the client already has (`contract_id` is left out
for `/api/wasm/{hash}`), so verifying needs no further request. The canonical
form is [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) for the integer-only
documents SoroVault serves: no whitespace, object keys sorted by UTF-16 code
unit, strings escaping only `"`, `\` and control characters (`<`, `&` and
U+2028 are written as themselves), integers as digits. The domain prefix means
a signature made for an attestation can never be replayed as a publisher claim.

Verifiers: Go has `attest.VerifyResponse`; TypeScript has
[`clients/typescript/verify.ts`](clients/typescript/verify.ts) (WebCrypto, no
dependencies). Both are tested against the same fixed vectors in
[`internal/attest/testdata/vectors.json`](internal/attest/testdata/vectors.json),
which include the awkward cases (HTML characters, U+2028, astral characters, the
full `u32` range), so the two implementations cannot silently drift. Pin the
public key from a channel you trust: verifying against a key fetched from the
same mirror proves nothing.

An attestation says a registry decoded *this interface from this WASM hash*; it
does not say the WASM is safe or that the publisher is who they claim — that is
what verification above is for.

### Contracts that left the network

A contract instance expires once it has not been used for a while. When a
refresh cannot find a contract, the registry records when it first noticed —
`missing_since` — rather than failing the refresh and losing what it knew:

```json
{
  "contract_id": "CDZZ…4PAN",
  "network": "testnet",
  "current_wasm_hash": "c618da…0781",
  "missing_since": "2026-08-07T12:00:00Z"
}
```

The interface is still served — it is the best description of a contract that
is no longer there — and the browse UI marks the contract as missing. A
contract that comes back is marked live again, and the mark is cleared. A
sweep counts a missing contract apart from a failure, and says so once, when
it happens, rather than on every sweep.

### Bounded registrations

Registering a contract downloads its WASM module and decodes it in memory,
so the number of registrations running at once is what keeps peak memory
finite. `MAX_CONCURRENT_REGISTRATIONS` (default `4`) bounds it, across every
client: the API, the CLI and the background refresher all draw on the same
budget, because they all download modules.

A request that cannot be given a slot waits a couple of seconds for one and
is then refused with **503** and a `Retry-After` header, rather than queueing
without limit:

```console
$ curl -si -X POST localhost:8080/api/contracts \
    -H 'content-type: application/json' \
    -d '{"contract_id":"CDZZ…4PAN"}'
HTTP/1.1 503 Service Unavailable
Retry-After: 2
…
{"error":"registry: too many registrations in flight: 4 already running"}
```

The wait is short on purpose: a client that is told to come back shortly is
better served than one left hanging behind a queue the server cannot drain.
The refresher treats a busy registry the same way — it skips the contract and
picks it up on the next sweep, since a contract that was not refreshed stays
stale. The browse UI's registration form draws on the same budget and shows
the same **503**, with the `Retry-After` header, inline on the page.

### Connection pool

SoroVault talks to Postgres through pgx's pool. Two variables size it:

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_MAX_CONNS` | pgx's own (the greater of 4 or the CPU count) | the most connections the pool holds open |
| `DATABASE_MIN_CONNS` | `0` | the connections the pool keeps open between requests |

Both are optional. Leaving them unset changes nothing: pgx sizes the pool,
as it always has — the greater of 4 or the CPU count for the maximum, and
none for the minimum. A value that is not a whole number, or that falls
outside the accepted range — `1` to `10000` for the maximum, `0` to `10000`
for the minimum — is a startup error rather than a silent fallback, and so
is a minimum above the maximum.

**Precedence.** pgx also reads `pool_max_conns` and `pool_min_conns` from
the connection string:

```text
postgres://…/sorovault?sslmode=disable&pool_max_conns=8
```

The environment wins. `DATABASE_MAX_CONNS`, when it is set, replaces
whatever `pool_max_conns` says, and `DATABASE_MIN_CONNS` replaces
`pool_min_conns`. When the environment variable is unset the connection
string's parameter applies, and when that is absent too, pgx's own default
does. A connection string is often shared with other tools, so this keeps
one deployment's settings from moving another's.

**Sizing.** A Postgres connection is a server-side process: each one costs
a few megabytes of memory on the server and a file descriptor on both ends.
A sensible maximum is therefore well below the database's own
`max_connections` (100 by default), leaving room for the migrations, the
`psql` sessions and the monitoring that share the same server.

SoroVault is mostly read-only — API reads are short queries, and a refresh
sweep is the only sustained traffic — so the default pool is usually
enough. Raise `DATABASE_MAX_CONNS` when requests queue behind a busy pool
(`pool timeout` errors, or latency that grows with load), not because the
number sounds better. Lower it on a small managed database, where the
connection limit is often lower than Postgres's own default.

Connections are opened on demand, so a large maximum costs nothing until
the traffic arrives. `DATABASE_MIN_CONNS` keeps that many open between
requests, which saves the handshake on the first request after an idle
period; it is worth setting only when that latency matters and the database
can afford the connections.

The `migrate` command does not use this pool: it opens its own connection
through golang-migrate, and migrations are short-lived either way.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/contracts` | list contracts — `?q=`, `?function=`, `?type=`, `?network=`, `?limit=`, `?cursor=` (`?offset=` still works but is deprecated; a cursor walk sees contracts registered mid-walk exactly once, an offset page repeats and skips them) |
| `POST` | `/api/contracts` | register a contract — `{"contract_id": "C…"}` |
| `POST` | `/api/contracts:batch` | register up to 20 contracts in one request — `{"contract_ids": ["C…", "C…"]}` |
| `POST` | `/api/wasm/{hash}` | catalog uploaded code before a contract runs it |
| `GET` | `/api/wasm/{hash}` | read a catalogued uploaded-code interface — `?network=` |
| `GET` | `/api/contracts/{id}` | the decoded ABI — `?network=`, `?wasm_hash=`, `?download=1` |
| `GET` | `/api/wasm/{hash}/contracts` | every contract running a given WASM — `?network=`, `?ever=`, `?limit=`, `?offset=` |
| `GET` | `/api/contracts/{id}` | the decoded ABI — `?network=`, `?wasm_hash=` |
| `PUT` | `/api/contracts/{id}/source` | attach a `source_url` and optional `source_commit` |
| `POST` | `/api/contracts/{id}/verify` | verify the publisher with a claim signed by the deploying account |
| `GET` | `/api/keys` | public keys that verify interface signatures |
| `PUT` | `/api/contracts/{id}/deprecation` | mark the contract deprecated — optional `successor` (registered on the same network, not the contract itself) and `note` |
| `DELETE` | `/api/contracts/{id}/deprecation` | lift the deprecation mark |
| `GET` | `/api/contracts/{id}/functions/{fn}` | one function in detail |
| `GET` | `/api/contracts/{id}/functions/{fn}/example.json` | one example argument list for the function — `?wasm_hash=` |
| `GET` | `/api/contracts/{id}/events` | the contract's event definitions — `?network=`, `?wasm_hash=` |
| `GET` | `/api/contracts/{id}/client.ts` | typed TypeScript client — `?wasm_hash=`, `?download=1` |
| `GET` | `/api/openapi.json` | OpenAPI 3.1 description of this API |
| `GET` | `/api/contracts/{id}/versions` | every stored interface version |
| `POST` | `/api/contracts/{id}/refresh` | re-check against the network |
| `GET` | `/api/stats` | contracts per network, stored versions, latest registration, last sweep |
| `GET` | `/api/upgrades` | recent interface upgrades, newest first — `?network=`, `?limit=`, `?offset=` |
| `GET` | `/api/upgrades.atom` | the same upgrades as an Atom 1.0 feed |
| `GET` | `/healthz` | liveness |
| `GET` | `/readyz` | readiness: checks Postgres and RPC |

Worked examples for each route, with requests and abbreviated responses, are in
[docs/api-recipes.md](docs/api-recipes.md).

`POST /api/contracts` returns **201** when the contract is new and **200** when
it was already registered, so it is safe to retry. Passing `?wasm_hash=` to a
read selects a superseded version instead of the current one.

`GET /api/contracts/{id}/functions/{fn}/example.json` returns an example
argument list for a call: one deterministic placeholder of each argument's
type, keyed as the function's JSON Schema spells them, so it validates against
that schema and can be pasted into a client and edited:

```console
$ curl -s localhost:8080/api/contracts/CDZZ…4PAN/functions/has_voted/example.json
{"nullifier":"0000000000000000000000000000000000000000000000000000000000000000"}
```

`sorovault codegen --lang examples` writes the same examples for every function
at once, one entry per function name.

**Batches.** `POST /api/contracts:batch` registers up to 20 contracts in one
request, which is how a tool seeds a whole deployment without making twenty
requests. The contracts are processed one after another, so the batch draws on
the registry's concurrency budget once per contract rather than all at once,
and one failure does not stop the rest. Every contract gets a result of its
own:

```json
{
  "results": [
    { "contract_id": "CDZZ…4PAN", "status": "created", "created": true, "changed": true, "contract": {…}, "interface": {…} },
    { "contract_id": "CDZZ…2V", "status": "error", "created": false, "changed": false,
      "error": "stellar: contract not found on network: CDZZ…2V", "code": 404 }
  ],
  "created": 1, "unchanged": 0, "changed": 0, "failed": 1
}
```

`status` is `created`, `unchanged`, `changed` (already registered, but
upgraded since it was last fetched) or `error`. A failure carries the status
code the contract would have had on a request of its own. The request itself
is bounded at 20 contract IDs, which is about 40 RPC round trips: enough to
seed a deployment, few enough to finish inside the server's request timeout.

**Search.** `?q=` is a case-insensitive substring match against the contract
ID, its name, and the names of every function, type and event in the
contract's **current** interface — so `?q=transfer` finds every token-like
contract in the registry. When a symbol matched, the listing says which:

```json
{ "contract_id": "CDZZ…4PAN", "network": "testnet", "matches": ["has_voted", "vote"] }
```

A query that matches no name is also matched against the doc comments of the
contract's functions and user-defined types (not events): there, every word
of the query must appear as a word of some doc, so `?q=swap rewards` asks
for both. Contracts whose names matched rank ahead of contracts only their
documentation matched.

A function removed by an upgrade stops matching; older versions stay
retrievable by hash but are not searched.

On Postgres, doc-comment search is backed by a GIN `tsvector` index (`to_tsvector('simple', …)`) and symbol/exact-name filters run in SQL over `ILIKE` and `jsonb` predicates. On SQLite (`DATABASE_URL=sqlite:///path/registry.db`), `specs` stores the newline-joined symbol list, doc text, and ABI JSON as `TEXT` without requiring FTS5 or C extensions, and evaluates the same case-insensitive symbol substring match, lowercased alphanumeric prefix doc-word match, and exact function/type/standard filters in process after narrowing by network. Both backends pass the same store conformance and search fuzz suites.

**Exact names.** `?q=` is a substring, so `?q=transfer` also finds
`transfer_from` and `transferable`. When a tool wants the contracts that
implement an interface, it wants the names to match exactly:

```
GET /api/contracts?function=transfer&function=balance
GET /api/contracts?type=Balance
```

`?function=` matches a function of that exact name, `?type=` a user-defined
type (struct, union, enum or error enum) of that exact name. Both are
repeatable, and every name has to match: two `?function=` parameters ask for
a contract with both functions. The names are matched case-sensitively, since
an interface is spelled a particular way. The filters combine with `?q=` and
`?network=`, and all of them have to match.

Status codes worth knowing: **422** means the contract exists but has no
interface to serve (a Stellar asset contract, a module with no spec section,
or one over SoroVault's decode limits), as distinct from **404**, which means
it is not there at all.

**Statistics and the upgrade feed.** `GET /api/stats` is the registry's size
at a glance: contract and stored-version counts per network, the most
recently registered contract, and the last automatic refresh sweep when
refresh is enabled. `GET /api/upgrades` is a paged feed of interface
changes — the contract, its network, the old and new WASM hashes, and when
the new version was observed — with the same events available as an Atom 1.0
feed at `GET /api/upgrades.atom`. A contract's first registration is not an
upgrade and is excluded from both.

## Browse UI

`serve` mounts a small server-rendered UI at `/`. The contract list is
searchable as you type; htmx swaps the table, and the same form works with
scripting off. Every contract page gives each function, type and event a
permalink — `#fn-transfer`, `#type-Amount`, `#event-Transfer` — with a visible
link beside its heading, so a discussion can point at one entry point instead
of the top of the page. The kind is part of the anchor, so a function and a
struct that share a name do not collide.

The index page also carries a registration form, so a contract can be added
while browsing rather than through the CLI or a curl. It posts to the UI
rather than the JSON API, which lets a failure (invalid ID, not found, no
interface, a busy registry) be explained in place with the same meaning as
the API's status codes. It is a plain HTML form that htmx upgrades, and the
write is accepted only when the browser names this origin in `Origin` (or
`Referer`): the UI has no session for a CSRF token to bind to, so a
same-origin check is the control that actually protects the write.

**Contracts that share a WASM.** Many contracts run the same code: every
instance of a token template, every vault a factory deployed. That is the
question a vulnerability in a shared WASM raises, and
`GET /api/wasm/{hash}/contracts` answers it:

```console
GET /api/wasm/c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781/contracts
```

```json
{
  "wasm_hash": "c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781",
  "network": "testnet",
  "ever": false,
  "contracts": [
    { "contract_id": "CDZZ…4PAN", "network": "testnet", "current_wasm_hash": "c618dae…", "current": true, … }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

`?ever=true` widens the answer to the contracts that ran the code in the past
and have since been upgraded; each result says whether the contract still runs
it. The contract page links its WASM hash to this listing.

`?download=1` serves a contract's interface as a file rather than a document
the browser renders — `<contract>.<short-hash>.abi.json` — which is what the
browse page's download link uses. The file holds the same document the plain
`GET` returns: the interface, under `interface`, with the contract's metadata
beside it.

## CLI

```console
sorovault add <contract_id>...      # fetch, decode and register (--from-file)
sorovault remove <contract_id>      # delete a contract and its versions (--yes)
sorovault refresh <contract_id>...  # re-check for an upgraded interface (--from-file)
sorovault refresh --all             # one sweep over every contract (--json)
sorovault list                      # -q, --function, --type, --network, --limit, --offset
sorovault get <contract_id>         # --function, --wasm-hash, --json
sorovault versions <contract_id>    # --network, --json
sorovault codegen <contract_id>     # --lang ts|md|examples; -o, --wasm-hash, --package, --zod
sorovault codegen <contract_id>     # --lang ts|md|examples; -o, --wasm-hash, --package
sorovault check-client <client.ts>  # --contract, --api, --network; exits non-zero when stale
sorovault export [file]             # versioned JSON backup (-o)
sorovault import [file]             # restore a backup (--merge)
sorovault serve                     # JSON API + browse UI
sorovault backfill-origins          # find the creator of contracts that lack one (--json)
sorovault claim sign <contract_id>  # sign a publisher claim (--name, --source-url, ...)
sorovault keygen                    # make an attestation signing key
sorovault migrate                   # apply schema migrations
sorovault migrate status            # --json
sorovault doctor                    # check a deployment's health
sorovault config show               # print the effective configuration, secrets redacted (--json)
```

`add` and `refresh` accept several contract IDs and report per-contract
failures without abandoning the rest. Both accept `--from-file path` to read
additional IDs, one per line; blank lines and lines beginning with `#` are
ignored, and `--from-file -` reads standard input. `list` takes `--function` and `--type`
for the same exact-name filters the API offers. Every read command takes
`--json`, which emits exactly what the API serves:

```console
$ sorovault get CDZZ…4PAN --function has_voted
fn has_voted(nullifier: BytesN<32>) -> bool

inputs:
  nullifier  BytesN<32>

returns: bool
```

`sorovault versions` lists a contract's stored interface history, newest
first, so the hash `get --wasm-hash` wants is one command away:

```console
$ sorovault versions CDZZ…4PAN
WASM HASH                                                         DECODED           CURRENT
c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781  2026-08-07 09:14  yes
```

### `sorovault doctor`

When a deployment misbehaves, the same four questions come up: can it reach
Postgres, are the migrations up to date, does the RPC endpoint answer, and
is it serving the network this instance expects. `doctor` answers all four in
one go:

```console
$ sorovault doctor
PASS  database    the database answers
PASS  migrations  the database is at version 2, the latest is 2
PASS  rpc         the endpoint is reachable, serving testnet
PASS  passphrase  the endpoint serves the configured network
```

Every check runs, so one failure does not hide the others, and the command
exits non-zero if any of them failed. `--json` emits the same report for a
script to read:

```console
$ sorovault doctor --json
{
  "ok": false,
  "checks": [
    { "name": "database", "ok": true, "detail": "the database answers" },
    { "name": "migrations", "ok": false, "error": "the database is at version 1, but version 2 is available; run `sorovault migrate`" },
    { "name": "rpc", "ok": true, "detail": "the endpoint is reachable, serving testnet" },
    { "name": "passphrase", "ok": true, "detail": "the endpoint serves the configured network" }
  ]
}
```

The command is strictly read-only: it opens a connection and reads, and
never writes to the database, applies a migration, or changes anything on
the network. It reads the same configuration as everything else, so a
deployment missing its `DATABASE_URL` is reported as a configuration error
rather than a passing check.

### `sorovault migrate status`

Before an upgrade, an operator wants to know what is applied and what is
pending — without applying anything. `migrate status` says exactly that, and
nothing more:

```console
$ sorovault migrate status
current version: 2
latest version:  4

pending migrations:
  VERSION  NAME
  3        wasm_specs
  4        contract_source
```

The version the database has applied, and whether it is dirty, come from
golang-migrate — the same source `sorovault migrate` applies, so the two
cannot disagree. The latest version, and the migrations still to come, are
the ones embedded in this binary. No migration is applied: a status run
reports and stops there. It does read the database through golang-migrate,
which creates the migrator's own version table if the database has not been
migrated yet — nothing else is written. A database role that may only read
will therefore fail the command; `sorovault doctor` reads the same state
without writing anything.

The command exits non-zero when migrations are pending, so it can gate a
deployment. A database marked **dirty** — a migration that failed partway, so
the schema is in an unknown state — is reported as such, with what to do
about it, and also exits non-zero.

`--json` emits the same report as JSON, for a gate that reads it rather than
a person. The `message` says what the report means and what to do about it,
so a reader of the JSON is told what a reader of the text report is:

```console
$ sorovault migrate status --json
{
  "current_version": 2,
  "latest_version": 4,
  "dirty": false,
  "pending": [
    { "version": 3, "name": "wasm_specs" },
    { "version": 4, "name": "contract_source" }
  ],
  "message": "2 migration(s) pending; run `sorovault migrate` to apply them"
}
```
### Shell completion

`sorovault completion bash|zsh|fish|powershell` prints a completion script (this
is Cobra's built-in command; run it once per shell). Then `Tab` completes the
arguments that are otherwise awkward to type:

```console
$ sorovault get <Tab>
CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN  zkvote
CBQZ…

$ sorovault get CDZZ --network <Tab>
testnet  futurenet  mainnet
```

`get`, `versions`, `refresh` and `codegen` complete contract IDs from the
registry, and `--network` completes the network names. Named contracts carry
their name as a description, so the candidate is readable rather than a column
of 56-character identifiers.

Two rules shape the behaviour, because completion runs inside the user's shell
prompt on every `Tab`:

- **It never fails noisily.** A database that is down produces no candidates
  and no error, rather than text pasted into the command line.
- **It is bounded.** The lookup is capped at a fraction of a second and at 200
  candidates, so a slow database or a large registry cannot hold the prompt.

Completion reads the registry only; it never contacts the network. `refresh`
takes contract IDs or `--all`, so it offers no IDs once `--all` is set.

## Typed clients

SoroVault generates a typed TypeScript client for any registered contract —
from the stored interface, so it needs no network access, and for any
version by `?wasm_hash=`:

```console
$ sorovault codegen CDZZ…4PAN -o zkvote.ts
$ curl -s localhost:8080/api/contracts/CDZZ…4PAN/client.ts > zkvote.ts
```

The file declares every struct, union and enum, an `Errors` code table, and a
`Client` interface with one method per contract function. `connect()` hands
back `@stellar/stellar-sdk`'s `contract.Client` narrowed to that interface:

```ts
import { connect } from "./zkvote";

const client = await connect({
  rpcUrl: "https://soroban-testnet.stellar.org",
  networkPassphrase: "Test SDF Network ; September 2015",
});
const tx = await client.get_proposal({ index: 0 });   // args and result are typed
console.log(tx.result);                               // sdk.Result<Proposal>
```

Type mappings follow the Stellar CLI's own bindings (`u64`→`bigint`,
`BytesN<32>`→`Buffer`, unions as `{ tag, values }`), so values pass through
the SDK unchanged. It requires `@stellar/stellar-sdk` 14 or later and
`buffer`.

### Runtime validation

TypeScript types disappear at compile time, but a frontend taking user input
before building a transaction can validate against the same shapes at
runtime with `--zod` (or `client.ts?zod=1` over the API). The generated file
gains a schemas section — one Zod schema per struct, union and enum, plus
one per function's argument list — with the widths enforced exactly:
integer bounds at each kind's limits and exact byte lengths for
`BytesN<N>`:

```ts
import { voteArgsSchema } from "./zkvote";

const args = voteArgsSchema.parse(JSON.parse(input)); // throws before anything ships
const tx = await client.vote(args);
```

A schema for a user-defined type references the others through `z.lazy`, so
recursive interfaces stay legal, and the section compiles standalone: with
`--zod` the file imports `zod` itself, and a package generated with
`--package --zod` pins `zod` as a peer dependency at the same version CI
type-checks the golden validators against.

### Staleness checks

Every generated file embeds the interface version it was built from: the
`wasmHash` constant next to `contractId` and `networkName`, also visible in
the file header. When the contract upgrades, a client generated for the old
interface keeps connecting but calls `vote` against functions that may no
longer exist. `assertSameWasm(liveHash)` compares the embedded constant with
the live hash — `GET /api/contracts/{id}` returns `wasm_hash` — warns on a
mismatch, and returns whether the client still matches:

```ts
import { assertSameWasm, connect } from "./zkvote";

const meta = await fetch("https://sorovault.example/api/contracts/CDZZ…4PAN").then(r => r.json());
if (!assertSameWasm(meta.wasm_hash)) {
  // The contract was upgraded: regenerate, or keep the behavior your
  // app was built against with the pinned client ?wasm_hash= serves.
}
```

Checking is opt-in: nothing happens unless the application calls it, so
offline use stays untouched, and a project that commits its client keeps it
byte-identical until the interface actually changes.

### Checking a committed client for staleness

Generated clients embed the WASM hash they were built for as the `wasmHash`
constant next to `contractId`. For a project that commits its client,
`sorovault check-client` compares that file with SoroVault's live hash and
with the client the server would generate now, and names the functions
added, removed or changed when the interface moved:

```console
$ sorovault check-client zkvote.ts --contract CDZZ…4PAN --api https://sorovault.example
zkvote.ts: added to the interface
tally: added to the interface
zkvote.ts is stale; regenerate it and commit the result
```

It exits non-zero when anything differs, so a CI job can fail politely with
a reason instead of failing deep inside a call.

### Packages

`--package` writes a complete npm package instead of a bare file, so a client
can be published to a registry or installed from a tarball:

```console
$ sorovault codegen CDZZ…4PAN --package --name @acme/zkvote -o zkvote-client
wrote zkvote-client (4 files)
```

The directory holds `src/index.ts`, a `package.json` that pins
`@stellar/stellar-sdk` and `buffer` as peer dependencies, a `tsconfig.json`
that builds to `dist/`, and a short README. `npm install && npm run build` in
it produces the importable, typed package.

The version defaults to `0.0.0-<WASM hash prefix>`, which names the interface
unambiguously and sorts below any release the publisher gives it; pass
`--package-version` to publish under a version of your own. The SDK version is
read from the same file CI type-checks against, so generated packages and the
golden client can never disagree.

CI compiles the generated client for the real testnet fixture with
`tsc --strict` against a pinned SDK, and builds the generated zkvote package
with `npm install && npm run build`, so a change to the generator that would
produce broken TypeScript fails the build.

## Markdown documentation

Generate a standalone Markdown reference from a registered contract's ABI,
including function signatures and comments, user-defined types, events, and
the selected WASM version:

```console
$ sorovault codegen CDZZ…4PAN --lang md -o CONTRACT.md
```

The browse UI renders the same documentation as Markdown, because that is
how contract authors write it — the testnet fixture's `vote` function has a
table and a bullet list. The rendering is sanitised, since documentation
comes from an untrusted chain: raw HTML in a doc never reaches the page, and
every link is marked `rel="nofollow noopener"`.

The browse UI also offers a copy button next to the values people paste into
other tools — contract IDs, WASM hashes and function signatures. The buttons
are revealed by a small script, so without JavaScript the values stay plain
selectable text.

## Uploaded code and source links

`POST /api/wasm/{hash}` catalogs uploaded code by its 64-character SHA-256
hash, even before a contract instance uses it. Its decoded interface is then
available from `GET /api/wasm/{hash}`. To see what a module would expose
before it is ever deployed — a doc comment that should have rendered, a type
that should have changed — `POST /api/decode` with the file as the request
body (raw or as a `multipart/form-data` file) answers with the ABI JSON and
stores nothing; `?format=ts` returns the generated TypeScript client
instead. The body is limited to 512 KiB, four times the 128 KiB the network
allows as contract code.

Attach a contract repository URL and an optional 40- or 64-character commit
hash with `PUT /api/contracts/{id}/source`; the browse page displays the
repository and commit with the interface.

**Deprecation.** When a team migrates off a contract, the registry does not
stop listing it — deployments keep pointing at it. Warn the people who find
it: `PUT /api/contracts/{id}/deprecation` marks the contract deprecated,
optionally naming a `successor` contract and a `note` on how to move over.
The successor must be registered on the same network, and must be a contract
other than the one being deprecated. The browse page shows a banner linking
the successor, the contract responses carry `deprecated_since`, `successor`
and `deprecation_note`, and `GET /api/contracts/{id}/client.ts` opens its
generated code with a `@deprecated` note. A refresh never lifts the mark;
`DELETE /api/contracts/{id}/deprecation` does.


## The ABI JSON

This is the project's public contract — the shape other tooling should build
against. `abi_version` is bumped if it ever changes incompatibly.

```jsonc
{
  "abi_version": "1",
  "meta": { "rsver": "1.93.0-nightly", "rssdkver": "25.3.0#dcbea445…" },
  "env_meta": { "interface_version": "25", "protocol": 25 },

  "functions": [
    {
      "name": "vote",
      "doc": "Cast a vote using a ZK Firma Digital Groth16 proof.…",
      "inputs": [
        { "name": "voter",       "type": { "kind": "address", "display": "Address" } },
        { "name": "pub_signals", "type": { "kind": "vec", "display": "Vec<BytesN<32>>",
                                           "element": { "kind": "bytes_n", "display": "BytesN<32>", "n": 32 } } }
      ],
      "outputs": [
        { "kind": "result", "display": "Result<(), Error>",
          "ok":    { "kind": "void",  "display": "()" },
          "error": { "kind": "error", "display": "Error" } }
      ]
    }
  ],

  "types": {
    "structs":     [ { "name": "Proposal", "fields": [ … ], "is_tuple": false } ],
    "unions":      [ { "name": "DataKey",  "cases":  [ { "name": "Admin", "values": [] } ] } ],
    "enums":       [ { "name": "Status",   "cases":  [ { "name": "Active", "value": 0 } ] } ],
    "error_enums": [ { "name": "Error",    "cases":  [ { "name": "AlreadyVoted", "value": 5 } ] } ]
  },

  "events": [
    {
      "name": "Voted",
      "prefix_topics": ["voted"],
      "params": [
        { "name": "voter",          "type": { "kind": "address", "display": "Address" }, "location": "topic-list" },
        { "name": "proposal_index", "type": { "kind": "u32",     "display": "u32"     }, "location": "data" }
      ],
      "data_format": "map"
    }
  ]
}
```

### Types

Every type is an object with a `kind` and a `display`. `display` is the
human-readable rendering of the whole type (`"Option<Vec<u32>>"`) so that UIs
and documentation generators do not each have to re-implement the traversal.

**Scalars** — `val`, `bool`, `void`, `error`, `u32`, `i32`, `u64`, `i64`,
`timepoint`, `duration`, `u128`, `i128`, `u256`, `i256`, `bytes`, `string`,
`symbol`, `address`, `muxed_address`. These carry no extra fields.

**Containers** — each adds the fields it needs:

| `kind` | Extra fields |
|---|---|
| `option` | `inner` |
| `vec` | `element` |
| `map` | `key`, `value` |
| `result` | `ok`, `error` |
| `tuple` | `elements` (array) |
| `bytes_n` | `n` |
| `udt` | `name` — refers to an entry in `types` |

A few things worth relying on:

- `outputs` is an array, but Soroban allows at most one entry. **Empty means the
  function returns nothing.**
- Empty collections are `[]`, never `null`, so you can iterate without a check.
- `is_tuple` on a struct marks a Rust tuple struct, which Soroban encodes as a
  struct whose fields are named `"0"`, `"1"`, … — flagged so consumers need not
  re-derive it.
- `meta` is omitted entirely when a contract carries no metadata section.
- `env_meta` is omitted entirely when a contract carries no environment
  metadata section. It says which host interface the contract was built
  against, which is the minimum protocol it needs: a host runs a contract
  built for the same or an older protocol version. `interface_version` is
  that version written as a version number; `protocol` and `pre_release` are
  the two numbers it is made of. The section used to carry them as one
  64-bit value — the protocol number in the high half, the pre-release
  number in the low half — and the XDR now spells them out separately. The
  bytes mean the same thing either way, so a section written by an older
  toolchain still decodes.

`abi_version` is **1**. The `env_meta` field was added without a bump,
because adding an optional field is not a breaking change.

## How it fits together

```
cmd/sorovault      cobra CLI; also serves the API and UI
internal/config    environment configuration
internal/model     the ABI model — no dependencies, the public JSON contract
internal/wasm      WASM custom-section reader
internal/spec      contract spec decoding: XDR -> model
internal/stellar   RPC client and contract fetch
internal/store     Postgres registry, migrations, and an in-memory store
internal/registry  register/refresh orchestration
internal/refresher scheduled re-checks of registered contracts
internal/codegen   Markdown documentation and typed client generation from a stored ABI
internal/api       chi JSON handlers
internal/web       html/template + htmx browse UI; goldmark + bluemonday for the docs
```

Three seams are interfaces, and each one is the natural place to extend the
project: **`stellar.Fetcher`** (where contract code comes from),
**`spec.Decoder`** (how an interface is derived), and **`store.Store`** (where
the registry lives). Nothing above a seam depends on what is below it, which is
why the whole stack can be tested without a network or a database.

### Data model

```
contracts (network, contract_id)  PK
  current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed,
  missing_since

specs     (network, contract_id, wasm_hash)  UNIQUE
  spec jsonb, decoded_at, symbols

wasm_specs (network, wasm_hash)  PK
  spec jsonb, decoded_at
```

Refreshing an upgraded contract **appends** a `specs` row and repoints
`current_wasm_hash`. Prior versions are never overwritten, so an interface that
was live at some point stays retrievable by its hash.

## Development

```console
$ make test        # no database, no network needed
$ make test-all    # also runs the store suite against a throwaway Postgres
$ make lint        # golangci-lint, pinned to the version CI uses
$ make build
$ make help        # everything else
```

`go test ./...` **never touches the network.** The decode path is exercised
against a real testnet contract checked in at
[`internal/spec/testdata/zkvote.wasm`](internal/spec/testdata/) — its provenance
and a hash check are documented alongside it. Layers above `internal/stellar`
run against a fake network (`internal/stellar/stellartest`).

The Postgres store suite skips unless `TEST_DATABASE_URL` is set; `make
test-all` and CI both set it. Both store implementations run the **same
conformance suite**, so the in-memory one other packages test against cannot
drift from the real one.

## Open ground

Contributions welcome. Deliberately **not** built yet:

- **More codegen targets.** TypeScript clients ship today (see
  [Typed clients](#typed-clients)); Go, Python and Rust generators would
  slot in beside `internal/codegen/typescript.go`, reusing its golden-file
  and compile-in-CI test pattern.
- **Resolving contract names.** The schema carries a `name`, but nothing
  populates it yet.
Explicitly out of scope: authentication, and any write operation against
contracts themselves. SoroVault reads chain state; it never sends a transaction.

## License

Apache-2.0. See [LICENSE](LICENSE).


## Token standard detection

SoroVault classifies a contract against versioned interface definitions after decoding its interface. The first matcher is SEP-41 Soroban Token Interface [v0.5.2](https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0041.md), a draft version pinned by the matcher. Matching requires every required function name, ordered argument type, and return type to match exactly; names alone are not enough. The API accepts repeatable `implements` filters, for example `GET /api/contracts?implements=sep41`, and the browse list shows a badge for detected standards.

Existing registry rows are classified on their next successful registration or refresh. The migration does not infer standards from old JSON rows, so an older contract will not appear in a standard-filtered listing until refreshed.
