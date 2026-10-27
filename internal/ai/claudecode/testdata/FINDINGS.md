# Claude Code characterization (U1)

Recorded 2026-10-27 on macOS with Claude Code 2.1.310 (the CLI), plus 2.1.309 (CLI,
older install) and 2.1.308 (the copy inside Claude Desktop). Every run used a throwaway prompt
that names no secret, an empty working folder, and the person's own sign-in. Identifiers
(email, organization, session and message ids, user name, home and plugin paths) are replaced
by fixed placeholders in this file and in every fixture; a test scans for them.

Fixtures are real recordings except where marked synthetic. They are replayed byte for byte by
`claudetest`.

| Fixture | What it is |
|---|---|
| `plain.jsonl` | One-word answer. Event order: `system/init`, `system/status`, `stream_event`s, `assistant`, `rate_limit_event`, `result` |
| `tool_block.jsonl` | A tagged tool block. The tag is split across `text_delta` chunks (`<hb-7f3a` then `9c>`), so the parser must hold a partial tag back |
| `signed_out.jsonl` | Signed out. Exit 1. `assistant` carries `"error":"authentication_failed"`, `result.is_error` is true |
| `rate_limit.jsonl`, `billing_error.jsonl`, `org_not_allowed.jsonl` | Synthetic: the signed-out shape with `error` and text changed to the codes the headless page lists. Replace with recordings when a live account hits them |
| `api_retry.jsonl` | `system/api_retry` events (`attempt`, `max_retries`, `retry_delay_ms`, `error_status`, `error`), recorded against an unreachable base URL. The run never finished |
| `killed_midstream.jsonl` | A stream cut after four deltas: no `message_stop`, no `result` |
| `unknown_option.jsonl` | Empty. A rejected flag prints `error: unknown option '--x'` to stderr, nothing to stdout, exit 1 |
| `auth_status_*.json` | `claude auth status` for signed in and signed out |

## Answers to the deferred questions

1. **Desktop's copy shares the CLI sign-in.** Yes. The bundled program
   (`~/Library/Application Support/Claude/claude-code/<version>/<hash>/claude.app/Contents/MacOS/claude`
   on macOS) reports `loggedIn: true`, `authMethod: claude.ai`, and ran a model call on 2.1.308.
   Discovery lists it by path only.
2. **Start-up and tokens.** Wall time per call is about 2.6 to 3.2 s for a one-word reply on
   `haiku`, of which about 1.3 s is the model's own time (`result.duration_ms`). Process start-up
   is therefore about 1.4 s, under the 3 s line. A
   history of 1, 10 and 40 turns (about 300 to 6,000 tokens) moved wall time by under 0.6 s.
   `claude --version` takes 10 ms and `claude auth status` 80 ms. With Claude Code's default
   system prompt each call carries about 2,900 cached input tokens that Hostbay does not
   control; a short fixed `--system-prompt` cut that to about 630. So KTD2 gains
   `--system-prompt <fixed constant>` and sends Hostbay's real system prompt on stdin.
3. **Minimum version.** Every KTD2 flag was accepted by 2.1.308, 2.1.309 and 2.1.310. Older
   versions were not available to test, so the minimum is 2.1.308. The startup probe stays the
   real detector (an unknown flag exits 1 with `unknown option`, see above).
4. **System prompt form.** There is no `--system-prompt-file` in 2.1.310. The only options are
   `--system-prompt <text>` and `--append-system-prompt <text>` on the command line, or text in
   the prompt on stdin. Hostbay uses a fixed, non-secret `--system-prompt` constant and puts
   everything else (its system prompt, the conversation and the tag) on stdin. Nothing about the
   host or the tag reaches `ps`.
5. **`--safe-mode` and settings `env`.** It does **not** ignore them. A project
   `.claude/settings.json` with `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9"}}` made a
   `--safe-mode` run retry against that address forever (`api_retry` events, no result in 25 s).
   Adding `--setting-sources local` fixed it (reply in 2.6 s) and also stops the person's user
   settings from applying. KTD2 therefore gains `--setting-sources local`, and the working folder
   stays empty. The person's own user-level `settings.json` `env` block was not changed to
   test it; `--setting-sources local` excludes the user source, which covers it.
6. **Hooks, tools, plugins.** With `--safe-mode`, `system/init` lists `tools: []`,
   `mcp_servers: []`, `skills: []`, `slash_commands: []`, and `--include-hook-events` shows no
   hook event, although the person's `settings.json` has hooks. The control run without
   `--safe-mode` produced 17 `hook_started` and 17 `hook_response` events. **`init.plugins` still
   lists the person's installed plugins (18 on this machine; 3 with `--setting-sources local`) and
   `init.agents` lists built-in agents. These are names only, not loaded:** `plugin_full_loads` was
   0 and no plugin tool, skill or hook appeared. The plan's fail-closed rule must not trigger on
   `plugins`; it triggers on a non-empty `tools`, `mcp_servers`, `skills` or `slash_commands`,
   or any `hook_started` event. KTD2 gains `--include-hook-events` so the last is visible.
7. **Environment.** The program needs `USER` (or `LOGNAME`) and `HOME` to find its macOS
   sign-in: with an empty environment plus `HOME` it reported signed out. So the scrub is the
   current environment minus the credential and provider variables, never an allowlist.
   - **Remove** (credential or provider switch, found by reading the variable names the
     program knows): every `ANTHROPIC_*`; `AWS_BEARER_TOKEN_BEDROCK`; `CLAUDECODE`;
     `CLAUDE_BRIDGE_OAUTH_TOKEN`, `CLAUDE_SESSION_INGRESS_TOKEN_FILE`,
     `CLAUDE_TRUSTED_DEVICE_TOKEN`, `CLAUDE_BG_SOCKET_TOKENS_PATH`; and, among `CLAUDE_CODE_*`,
     any name containing `OAUTH`, `API_KEY`, `CLIENT_KEY`, `GATEWAY`, `BASE_URL`, `USE_BEDROCK`,
     `USE_VERTEX`, `USE_FOUNDRY`, `USE_MANTLE`, `SECRET`, `PASSWORD`, `PASSPHRASE`, `CREDS` or
     `TOKEN` (but not `TOKENS`, which are size limits), or starting with `CLAUDE_CODE_HOST_`, `CLAUDE_CODE_MESSAGING_`, `CLAUDE_CODE_SESSION_`,
     `CLAUDE_CODE_SDK_`, `CLAUDE_CODE_CCR_`, plus `CLAUDE_CODE_ENTRYPOINT`,
     `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_EXECPATH`, `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST`,
     `CLAUDE_CODE_ACCOUNT_UUID`, `CLAUDE_CODE_ORGANIZATION_UUID`, `CLAUDE_CODE_USER_EMAIL` and
     the `CLAUDE_CODE_SKIP_*_AUTH` switches. The session variables matter when Hostbay is started
     from inside another Claude Code session: this machine's own environment carried
     `ANTHROPIC_BASE_URL` and a messaging token.
   - **Keep:** `HOME`, `USER`, `LOGNAME`, `USERPROFILE`, `PATH`, `TMPDIR`, proxy and certificate
     variables (`HTTPS_PROXY`, `NO_PROXY`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_*`), and every other
     `CLAUDE_CODE_*` setting such as `CLAUDE_CODE_GIT_BASH_PATH`, the certificate-store settings
     and the person's own privacy opt-outs (`DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING`,
     `CLAUDE_CODE_DISABLE_*`).
8. **`auth status`.** JSON with `loggedIn`, `authMethod` (`claude.ai` here), `apiProvider`
   (`firstParty`) and account fields that Hostbay discards. Signed out is `loggedIn: false`,
   `authMethod: none`. A signed-in status in a different config folder reported signed out,
   so status must run in the same environment as the model calls.
9. **Documented approval routing.** The headless page describes `--permission-prompt-tool`
   and `--permission-prompts host`: both only answer permission prompts for Claude Code's own
   tools, and a model call with tools off has none. They give Hostbay no control over step
   order, mode, target terminal or completion envelope, so KTD1's rejected rows stand. The
   `capabilities` array on `system/init` (`interrupt_receipt_v1` and similar) is for
   feature detection of stream-json input, which this plan does not use.
10. **Kill.** The fake pauses mid-stream and a test kills it; the real program was not
    killed in this spike. Process-tree kill is U2's job and is tested against the fake.

## Stop conditions

- A Claude Code tool, hook or MCP server reachable under the KTD2 flags: **not found** on an
  unmanaged machine. Admin-managed policy settings are not testable here, so the fail-closed
  check stays.
- The program needs Hostbay to supply or read a credential: **no**.
- Median start-up above 3 s: **no** (about 1.4 s; the 40-turn call reaches 3.2 s wall because
  the model reads a longer prompt).
- Anthropic's legal page forbids running the unmodified binary for the user's own sign-in:
  **no**, but see the next section.

## Anthropic's legal page, re-read 2026-10-27

`code.claude.com/docs/en/legal-and-compliance`. The text quoted in the plan's Problem Frame is
unchanged. The page also has a section, "Can customers offer Claude Code in their products?", that
the Problem Frame does not quote: preinstalling or *running* Claude Code in a product or service
"requires agreeing to our Commercial Terms of Service and complying with the conditions below",
which are an unmodified binary, no removing or restricting its authentication methods, no paying
for or reselling or intermediating usage, and each end user signing in with their own credential.
Hostbay satisfies every listed condition (it neither bundles nor modifies the program, it never
touches sign-in, and each user uses their own plan). Whether "running Claude Code in your
product" covers launching the user's own installed program is the same open sentence the owner
already decides before release. It does not block building, and the owner decision gains this
sentence.
