---
title: AWS Manager - Plan
type: feat
date: 2026-11-23
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-brainstorm
execution: code
---

# AWS Manager - Plan

## Goal Capsule

- Objective: someone who already uses the AWS CLI on this computer can open their AWS accounts in Hostbay, see their EC2 instances and RDS databases per region, and reach them (a shell, an SSH host, a database host) without leaving the app or copying endpoints by hand.
- Product authority: this plan, then `CLAUDE.md` (public repository rules, commit rules). The request was one line ("can we add aws manager"); the scope below is this plan's pick, recorded as assumptions.
- Means: a stand-in session on this computer like the local Containers tab, AWS API calls signed in Go, and the AWS CLI in a terminal tab for SSO sign-in and SSM (KTD1 to KTD4).
- Open blockers: none.
- Stop conditions: anything that would need Hostbay to store AWS keys, add a new Go module, or let the page pass arguments to the CLI is skipped and reported instead.
- Execution profile: work on `main` in a checkout shared with another session (DBeaver import); stage only this plan's files, one commit per unit or pair of units, never push.

## Product Contract

### Summary

An AWS tab, opened from the start page like the local Containers tab, lists the profiles in this computer's AWS config. For the chosen profile and region it shows the account, the EC2 instances and the RDS databases. An instance opens an SSM shell or becomes a saved SSH host (an RDP host for a Windows instance); a database becomes a saved database host. Instances can be started, stopped and rebooted after a confirmation.

### Problem Frame

Hostbay already reaches servers, databases and Kubernetes clusters, but a user on AWS has to look up an instance's address or an RDS endpoint in the console or the CLI and type it into a host form. Instances without a public address, or with no SSH key, are reached through SSM Session Manager, which today means a separate terminal.

### Key Decisions

- **AWS is a session on this computer, not a saved host.** Profiles live in `~/.aws`, which is per machine; a saved host would sync a profile name to computers that lack it. Governs R1, R2.
- **Hostbay keeps no AWS secrets.** Credentials come from the AWS config the CLI already uses (keys, SSO cache, assume-role, credential process) and never reach the page. Governs R3, R4.
- **The AWS CLI does what needs it.** SSO sign-in and SSM shells run the user's `aws` binary in a terminal tab; listing and instance actions call the AWS APIs from Go. Governs R4, R9.
- **Reaching a resource reuses existing host forms.** "Save as SSH host" and "Save as database host" open the normal host editor prefilled, so keys, passwords, VPN and trust checks work as they do for any host. Governs R10, R12.

### Requirements

**Accounts**

- R1. The start page has an AWS button that opens one AWS tab; a second click focuses the open tab.
- R2. The tab lists every profile in the AWS config and credentials files (honouring `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE`) and opens on the default profile, or the first one. With no profiles it says where it looked and that Hostbay reads named profiles only.
- R3. For the chosen profile the tab shows the account ID and caller ARN, and a region picker that starts on the profile's region (us-east-1 when unset) and offers the regions enabled for the account.
- R4. When the profile's credentials cannot be loaded, the tab shows why; for a profile whose SSO sign-in (its own or its `source_profile` root's) is missing or expired it offers "Sign in", which runs `aws sso login` for the profile holding the SSO settings in a terminal tab.

**EC2**

- R5. The tab lists the region's EC2 instances with name tag, instance ID, state, type, private and public address and key pair name, and a text filter.
- R6. A running instance offers "Shell (SSM)", which opens a terminal tab running an SSM session through the AWS CLI.
- R7. When the AWS CLI or the Session Manager plugin is missing, the action says which one and how to install it, instead of failing silently.
- R8. Start (on a stopped instance), stop and reboot (on a running one) ask for confirmation naming the account, region and instance, then refresh the list. Terminate is not offered.
- R9. The page never passes a command line: Go validates the profile name, region and instance ID and runs the CLI with an argument list.
- R10. "Save as SSH host" ("Save as RDP host" for a Windows instance) opens the host editor prefilled: the name tag (or ID) as the name, the public address (else the private one) as host name, port 22, and an `aws` tag; a Windows instance prefills an RDP host instead.

**RDS**

- R11. The tab lists the region's RDS instances with identifier, engine, status, endpoint, port and master user name.
- R12. "Save as database host" opens the host editor prefilled with the matching Hostbay kind, endpoint (an Aurora member's cluster writer endpoint), port, master user name and database name; engines Hostbay cannot open (no matching kind) show the endpoint but no save action.

### Acceptance Examples

- AE1. Covers R4. Given an SSO profile whose cached token has expired, when the tab loads it, then it shows the expiry and a "Sign in" button; after `aws sso login` succeeds in the terminal tab, "Refresh" loads the account.
- AE2. Covers R12. Given an `aurora-postgresql` cluster member at `db1.abc.us-east-1.rds.example.test:5432`, when the user picks "Save as database host", then the editor opens as a PostgreSQL host with its cluster's writer endpoint and port 5432, and nothing is saved until the user saves the form.
- AE3. Covers R6, R7. Given no `session-manager-plugin` on this computer, when the user picks "Shell (SSM)", then the terminal shows the CLI's own message and Hostbay's hint names the plugin.
- AE4. Covers R9. Given a profile name starting with `-`, when any AWS call names it, then Go refuses it before running anything.

### Scope Boundaries

Deferred for later:

- Access keys saved in Hostbay (profiles only for now).
- Terminate, create, or modify instances; security groups; snapshots.
- EKS (import as a Kubernetes host), S3 browsing beyond the existing S3 host kind, ECS, Lambda, CloudWatch, cost.
- EC2 Instance Connect, port forwarding through SSM, a native SSM client without the plugin.
- Writing to `~/.aws` (adding or editing profiles).
- Assume-role profiles that need an MFA code: they are refused with a plain message.
- Profiles that sign in with `aws login` (`login_session`): they show the SDK's error and no Sign in action.

### Dependencies / Assumptions

- The AWS CLI v2 is installed for SSO sign-in and SSM shells; listing works without it for non-SSO profiles and for SSO profiles with a valid cached token.
- SSM shells also need the Session Manager plugin, which this development machine lacks, so R6 is checked only up to the CLI's error.
- AWS API traffic goes over this computer's normal network, not through a host's proxy or VPN profile.

Product Contract preservation: changed: R10 — a Windows instance prefills an RDP host, since SSH rarely reaches one; R2, R4, R8, R12 and AE2 sharpened by document review (empty profiles message, `source_profile` SSO roots, per-state actions, Aurora cluster endpoint); no scope added beyond those.

## Planning Contract

### Key Technical Decisions

- KTD1. **The AWS tab is a stand-in session with no backend session.** The tab's session ID is minted in the page and its host is a stand-in, like `openFiles` in `src/state/SessionsProvider.tsx`; focusing an open tab follows `openContainers`. Every backend call names the profile and region, so closing the tab releases nothing. Governs R1, R2.
- KTD2. **Credentials come from `aws-sdk-go-v2/config` with the profile named.** The module is already in `go.mod` as an indirect dependency, so this adds no module. Each profile and region's credentials setup is kept in Go for up to five minutes, and dropped when either AWS file changes. A failed load is never kept, and the credentials themselves stay in the SDK's provider. (Changed after review: loading fresh on every call ran a prompting `credential_process` three or more times per refresh. The service still holds no session and every call still names its profile and region.) Governs R3, R4.
- KTD3. **EC2, RDS and STS are called through their Query APIs, signed with the SDK's `aws/signer/v4`, and decoded with `encoding/xml`.** `service/ec2` and `service/rds` are not in the module graph; STS stays on the same path so one `query` and one test endpoint seam serve all three. The repo already prefers small clients to SDK services for S3 (`internal/s3sig`). Six actions do not justify two large modules. Governs R3, R5, R8, R11.
- KTD4. **SSO sign-in and SSM shells run the `aws` binary through `terminal.OpenCommand`, as `AdminService.ContainerTerminal` runs an engine.** The binary is found on PATH and then in the usual install folders, because a Finder-launched app has a short PATH (`container.Find`). The child's PATH has those folders first, so `session-manager-plugin` resolves beside it. When the service starts it also appends any missing install folders to the app's own PATH, so a `credential_process` command the SDK runs resolves when Hostbay is launched from the Finder. Governs R4, R6, R7, R9.
- KTD5. **Inputs are checked in Go at the service boundary.** The profile name must exist in the config files and must not start with `-`. The region must match `^[a-z]{2}(-[a-z]+)+-\d$`. The instance ID must match `^i-[0-9a-f]{8,17}$`. The instance action must be `start`, `stop` or `reboot`. Governs R8, R9.
- KTD6. **The engine-to-kind map lives in Go.** Each RDS row carries the Hostbay kind, or none, so the page holds no engine rules. `postgres` and `aurora-postgresql` map to postgres. `mysql`, `mariadb`, `aurora` and `aurora-mysql` map to mysql. `sqlserver-*` maps to mssql and `oracle-*` to oracle. The `custom-` variants of the last two map the same way. Governs R12.
- KTD7. **An expired or missing SSO sign-in is a new error code, `AWSSignIn`, decided from the SSO token cache, not the error type.** When credentials fail and the profile's SSO root has no cached token file (`ssocreds.StandardCachedTokenFilepath`, keyed by `sso_session` name, else `sso_start_url`) or one past its `expiresAt`, the error is `AWSSignIn` with the root profile and `expiresAt` in details. A `processcreds.ProviderError` becomes a fixed message naming the profile, because its text can repeat the process output, secret key included. `AssumeRoleTokenProviderNotSetError` becomes a message that the profile needs an MFA code Hostbay does not ask for. Any other failure shows the SDK's message. Governs R4.
- KTD8. **The host editor opens on a draft.** `HostManager` gains `newHostFrom(draft)`, and the tab calls it through `useHostManager()`. Saving still goes through `SaveHost`. Governs R10, R12.

### High-Level Technical Design

Directional only:

```text
AwsTab (page) --profile, region--> AwsService (internal/app/aws.go)
    AwsProfiles          -> awsapi.Profiles(config files)
    AwsAccount           -> STS GetCallerIdentity (fails the load)
    AwsRegions           -> EC2 DescribeRegions, else a built-in region list
    AwsInstances         -> EC2 DescribeInstances (all pages)
    AwsInstanceAction    -> EC2 Start/Stop/RebootInstances
    AwsDatabases         -> RDS DescribeDBInstances + DescribeDBClusters (all pages)
    AwsShell / AwsSignIn -> terminal.OpenCommand(aws, argv) -> pty ID for TerminalTab
```

### Assumptions

- The default AWS endpoint shape `https://<service>.<region>.amazonaws.com` (with `.cn` for `cn-` regions) covers the regions users pick. Custom endpoints are out.
- Lists are small enough to load in full per region: there is no paging in the page.
- Environment credentials (`AWS_ACCESS_KEY_ID`) override a profile's keys per SDK rules; that is acceptable and not special-cased.

### Sequencing

U1 (Go client) comes first, then U2 (service, CLI), then U3 (bindings and API), then U4, then U5 (UI). U6 checks everything.

## Implementation Units

### U1. AWS client package

**Goal:** a small Go package that lists profiles, loads a profile's credentials, and makes the signed Query calls this feature needs.

**Requirements:** R2, R3, R5, R8, R11, R12; KTD2, KTD3, KTD5, KTD6.

**Dependencies:** none.

**Files:**
- `internal/awsapi/profiles.go`, `internal/awsapi/client.go`, `internal/awsapi/ec2.go`, `internal/awsapi/rds.go`, `internal/awsapi/validate.go`
- `internal/awsapi/awsapi_test.go`
- `go.mod` (promote `aws-sdk-go-v2`, `config`, `credentials` from indirect to direct)

**Approach:**
1. `Profiles()` reads the config file (`[default]`, `[profile x]`, `[sso-session x]` skipped) and credentials file (`[x]`). It honours the SDK's default file names and their environment overrides. Each profile has its name, region, and its SSO root: itself when it has `sso_session` or `sso_start_url`, else the first such profile up its `source_profile` chain (loops stop). It also records whether it or its chain sets `mfa_serial`.
2. `New(ctx, profile, region)` loads `config.LoadDefaultConfig` with that profile and region. `endpoint` is a package variable so tests point it at `httptest`.
3. `query` posts a form (`Action`, `Version`, params), signs it with `v4.Signer`, and decodes either error shape (`Response>Errors>Error`, `ErrorResponse>Error`) into an error naming the AWS code and message.
4. `Identity`, `Regions`, `Instances` (follows `nextToken`), `InstanceAction`, `Databases` (follows `Marker`) decode only the fields R3, R5 and R11 list, plus `platform` for R10 and the database name and `DBClusterIdentifier` for R12. `Databases` also calls `DescribeDBClusters` (follows `Marker`) and sets each cluster member's save endpoint to its cluster's writer endpoint.
5. `DatabaseKind(engine)` implements KTD6. `ValidRegion`, `ValidInstanceID` implement KTD5.

**Patterns to follow:** `internal/s3sig/s3sig.go` (small signed client), `internal/importer/kubeconfig/import.go` (reading a user's config file).

**Test scenarios:**
- Profiles from a temp config and credentials file: default first, `[profile x]` named `x`, a credentials-only profile included, an `sso-session` section not listed, an SSO profile is its own SSO root, a role profile whose `source_profile` is that SSO profile names it as root, a `source_profile` loop ends without hanging, `mfa_serial` flagged.
- `AWS_CONFIG_FILE` pointing at a missing file yields only credentials-file profiles, not an error.
- `Instances` against an `httptest` server returning two pages: both pages' instances returned, name tag read, the second request carries `NextToken`.
- The request is signed: it has an `Authorization` header naming `AWS4-HMAC-SHA256` and the service and region.
- An EC2 error body (`UnauthorizedOperation`) and an RDS error body (`AccessDenied`) both surface the code and message.
- `Databases` follows `Marker`, reads endpoint, port, master user and database name, and gives an Aurora member its cluster's writer endpoint.
- `DatabaseKind` maps each engine family in KTD6 and returns "" for `db2-se`.
- `ValidInstanceID` refuses `-i-1`, `i-XYZ`, accepts `i-0123456789abcdef0`; `ValidRegion` refuses `us-east-1;x`.

**Verification:** `go test ./internal/awsapi/` passes with every test pointing at temp files and `httptest`, never at `~/.aws` or AWS.

### U2. AwsService and CLI terminals

**Goal:** the Wails service the tab calls, including the SSO sign-in and SSM shell terminals.

**Requirements:** R2 to R9, R11, R12; KTD1, KTD4, KTD5, KTD7.

**Dependencies:** U1.

**Files:**
- `internal/app/aws.go`, `internal/app/aws_test.go`
- `internal/awsapi/cli.go` (finding `aws`, argv builders, plugin check)
- `internal/apperr/apperr.go` (code `AWSSignIn`)
- `main.go` (register the service)

**Approach:**
1. `AwsProfiles`, `AwsAccount(profile, region)`, `AwsRegions(profile, region)`, `AwsInstances`, `AwsDatabases`, `AwsInstanceAction(profile, region, id, action)` check the profile against `Profiles()` and the other inputs per KTD5, refuse a profile flagged `mfa_serial` with the KTD7 MFA message, then call U1. `AwsRegions` returns the built-in commercial region list (plus the profile's region) with a `checked: false` flag when `DescribeRegions` fails.
2. Credential failures map per KTD7.
3. `AwsShell(owner, profile, region, id, cols, rows)` and `AwsSignIn(owner, profile, cols, rows)` find `aws` (an `apperr` naming the AWS CLI when missing). They run `ssm start-session --target <id> --profile <p> --region <r>` or `sso login --profile <SSO root of p>` via `terminal.OpenCommand`, and return `conns.terms.Open(owner, t)`.
4. `NewAwsService` appends missing install folders to the process PATH per KTD4.
5. `AwsTools()` reports whether `aws` and `session-manager-plugin` were found, so the page can show R7's hint before the shell opens.

**Patterns to follow:** `AdminService.ContainerTerminal` in `internal/app/admin.go`; `container.Find` and `childEnv` in `internal/container/runner_local.go`.

**Test scenarios:**
- `AwsInstances` with a profile not in the config files is refused before any request.
- `AwsInstanceAction` with action `terminate` is refused.
- An `sso_session` profile with no cached token returns code `AWSSignIn`; one with an expired cached token returns it with `expiresAt`.
- A role profile whose `source_profile` is an SSO profile returns `AWSSignIn` naming the SSO root, and `AwsSignIn` on it builds `sso login --profile <root>`.
- A `credential_process` printing malformed JSON that contains `sample-secret-key` returns an error that does not contain that string.
- A profile with `mfa_serial` is refused with the MFA message before any request.
- `AwsRegions` with a `DescribeRegions` stub returning `UnauthorizedOperation` returns the built-in list, unchecked, not an error.
- `AwsShell` with a profile named `--debug` is refused.
- The argv builders return the exact argument lists in step 3, and nothing passes through a shell.
- With `PATH` empty and no install folder holding `aws`, `AwsSignIn` fails naming the AWS CLI.

**Verification:** `go test ./internal/app/ -run Aws` passes; the service appears in regenerated bindings.

### U3. Bindings and page API

**Goal:** typed page access to the service.

**Requirements:** R1 to R12 (transport only).

**Dependencies:** U2.

**Files:**
- `src/bindings/hostbay/internal/app/awsservice.ts` and related generated files (regenerated, not hand-written)
- `src/api.ts`

**Approach:** run the repo's `wails3 generate bindings` command from `build/Taskfile.yml`, then add thin `aws*` wrappers and exported types to `src/api.ts` beside the Kubernetes ones.

**Test expectation:** none — generated code and one-line wrappers; covered by U4 and U5 tests and the TypeScript build.

**Verification:** `npm run build` type-checks.

### U4. AWS tab, terminals and start page entry

**Goal:** the tab itself, opened from the start page, with SSM and sign-in terminals.

**Requirements:** R1 to R8, R11; KTD1, KTD7.

**Dependencies:** U3.

**Files:**
- `src/tabs/aws/AwsTab.tsx`, `src/tabs/aws/aws.ts`, `src/tabs/aws/aws.test.ts`
- `src/state/sessions.ts` (`aws` on `Connection`, `TabKey` `aws`, `LocalTerm` kind `aws`)
- `src/state/SessionsProvider.tsx` (`openAws`, `openAwsTerm`)
- `src/tabs/TerminalTab.tsx` (open the `aws` term kind)
- `src/shell/SessionLayer.tsx`, `src/shell/StartPage.tsx`, `src/ui/icons.ts`
- `src/i18n/en.ts`

**Approach:**
1. `openAws` focuses an open AWS tab (as `openContainers` does) or opens one with a page-minted ID and a stand-in host (as `openFiles` does). Closing it calls nothing in the backend.
2. The tab loads profiles and `AwsTools`, then the account and regions for the chosen profile and region. It shows the identity line and a refresh button, with an EC2 / RDS switch and a filter box. A region list that could not be checked shows a note beside the picker.
3. On `AWSSignIn` it shows the message, the expiry when known, and "Sign in", which opens an `aws` terminal of action `login`.
4. Empty states use `src/ui/EmptyState`: no profiles names the config and credentials file paths and their environment overrides; an empty region says "No instances (or databases) in <region>"; a filter with no match says so.
5. Instance rows by state: `running` offers Shell (SSM), Stop and Reboot; `stopped` offers Start; every other state shows its name and no action. Save as host shows in every state. Start, stop and reboot go through `usePrompt().confirm` naming account, region and instance; the row's buttons are disabled until the call returns, then the list reloads once (no polling).
6. Missing tools (from `AwsTools`) show in the status row under the toolbar: without `aws`, Sign in and Shell (SSM) are disabled and the note names the AWS CLI and its install page; without only `session-manager-plugin`, Shell (SSM) stays enabled and the note names the plugin and its install page. Shell or sign-in failures go to the error toast.
7. `aws.ts` holds filter matching; U5 adds the draft builders.

**Patterns to follow:** `src/tabs/kubernetes/KubernetesTab.tsx` (list, toolbar, confirm, stale-answer guard), `openContainers` and `openPodTerm` in `src/state/SessionsProvider.tsx`.

**Test scenarios:**
- Filter matches name tag, instance ID and address, case-insensitively, and an empty filter keeps every row.
- `instanceActions` returns Shell/Stop/Reboot for `running`, Start for `stopped`, nothing for `stopping`.
- Covers AE1. The sign-in state is chosen only for the `AWSSignIn` code, not for another error.

**Verification:** the tab opens from the start page in the running app and lists profiles from `~/.aws`; vitest passes.

### U5. Save as host

**Goal:** instances and databases become prefilled host drafts in the normal editor.

**Requirements:** R10, R12; KTD6, KTD8.

**Dependencies:** U4.

**Files:**
- `src/components/HostManager.tsx` (`newHostFrom`)
- `src/tabs/aws/aws.ts`, `src/tabs/aws/aws.test.ts`, `src/tabs/aws/AwsTab.tsx`

**Approach:**
1. `instanceDraft(i)` builds an SSH draft per R10 (an RDP draft on port 3389 for platform `windows`, with the action labelled "Save as RDP host").
2. `databaseDraft(db)` builds a database draft with the Go-supplied kind; rows with no kind get no action.
3. `newHostFrom(draft)` opens the editor on `{ ...blank(), ...draft }`, so nothing is saved until the user saves.

**Test scenarios:**
- An instance with a public IP drafts that IP; one with only a private IP drafts the private one; the name tag becomes the alias, else the instance ID; the tags include `aws`.
- A Windows instance drafts kind `rdp` on port 3389.
- Covers AE2. A database of kind `postgres` at `db1.abc.us-east-1.rds.example.test:5432` drafts kind postgres with the cluster writer endpoint Go supplied, port, master user and database.

**Verification:** in the running app, "Save as SSH host" opens the editor prefilled and the host list is unchanged until saving.

### U6. End-to-end check

**Goal:** prove the feature in the running app and the test suites.

**Requirements:** all.

**Dependencies:** U1 to U5.

**Files:** none new.

**Approach:** build the UI, run both suites, launch the app, open the AWS tab and check profiles, account and lists against this machine's own AWS setup where it works. Nothing about that setup is written into the repo.

**Test expectation:** none — verification only.

**Verification:** per the Verification Contract.

## Verification Contract

- `npm run build`, then `go test ./...` and `npx vitest run` pass.
- `go vet ./internal/awsapi/ ./internal/app/` is clean.
- No test reads the real `~/.aws`. Tests set `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE` and `AWS_EC2_METADATA_DISABLED`. Fixtures use account `123456789012`, `example.test` names and `192.0.2.0/24` addresses.
- The hand-written XML field names are checked once against a real response from this machine's AWS setup (read-only calls), since the fixtures cannot prove them; nothing from that response is written into the repo.
- The running app opens the AWS tab and shows this computer's profiles. SSM is checked only up to the CLI's message, since the plugin is not installed here.

## Definition of Done

- Every unit's verification holds and the Verification Contract passes.
- No real account ID, ARN, hostname or profile name is in the diff or the commit messages.
- Only this plan's files are staged in each commit; the DBeaver session's files are untouched.
- No dead code from abandoned attempts remains.
