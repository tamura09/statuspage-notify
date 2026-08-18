# statuspage-notify

Polls an [Atlassian Statuspage](https://www.atlassian.com/software/statuspage) and
reports every incident and scheduled maintenance into a Discord **forum** channel,
one thread per incident.

One binary, one Lambda function per page being watched, all deployed from
[tamura09/aws-terraform](https://github.com/tamura09/aws-terraform) into the same
Discord forum channel. Each identifies itself there by its webhook username and
thread prefix, taken from `PAGE_LABEL`:

| Function | Page | Posts as |
| --- | --- | --- |
| `claude-status-notify` | status.claude.com | **Claude Status** |
| `github-status-notify` | githubstatus.com | **GitHub Status** |
| `vercel-status-notify` | vercel-status.com | **Vercel Status** |
| `supabase-status-notify` | status.supabase.com | **Supabase Status** |
| `ubiquiti-status-notify` | status.ui.com | **Ubiquiti Status** |
| `cloudflare-status-notify` | cloudflarestatus.com | **Cloudflare Status** |
| `nature-status-notify` | nature.statuspage.io | **Nature Remo Status** |
| `mercari-status-notify` | status.mercari.com | **Mercari Status** |

`provided.al2023` / `arm64`, us-east-1, run every minute by EventBridge.

## Why a thread per incident, and why not edit one message

The obvious design — post once and edit that message as the incident progresses —
does not work, because **Discord never sends a notification for a message edit**.
Only `MESSAGE_CREATE` produces a push notification or an unread badge; adding an
`@mention` during an edit does not change that. The recovery message, the one
update people actually want, would be the one nobody is told about.

So every Statuspage update is posted as its own message. To keep that from
flooding the channel, all the updates for one incident go into one thread:

| Statuspage | Discord |
| --- | --- |
| New incident or maintenance | New forum post (`thread_name`), titled `🔴 <page> · YYYY-MM-DD <incident name>` |
| `identified`, `monitoring`, … | New message inside that thread |
| `resolved` / `completed` | New message inside that thread, green, with the role mention — and the thread title flips to `🟢` |

A **forum** channel specifically, because a webhook cannot create a thread in a
plain text channel — `thread_name` is only accepted on forum and media channels,
and `POST /channels/{id}/messages/{id}/threads` needs a bot token. Posting into an
existing thread (`?thread_id=`) works either way and un-archives it automatically.

Follow-ups inside a thread only notify members who already joined it, and nobody
has joined a thread a webhook created seconds earlier — so `MENTION_ROLE_ID` is
what makes the recovery notification actually arrive. It is applied to the
thread-opening post and to the terminal update, and nothing else, so the updates
in between stay quiet.

The forum channel must **not** have "Require members to select tags when posting"
enabled: this function sends no `applied_tags`, and Discord rejects the post with
a 400 if the channel demands one.

## The thread title marker needs a bot token

`🔴` → `🟢` is the one thing a webhook cannot do. `thread_name` is only accepted
when the post is created, and renaming afterwards is `PATCH /channels/{id}`,
which needs a real identity. So `DISCORD_BOT_TOKEN_PARAMETER_NAME` buys exactly
one capability: rewriting the marker when an incident resolves. Every message is
still posted through the webhook.

Leave it unset and everything else works unchanged — threads simply keep the
marker they opened with. The phase is recorded either way, so adding a token
later does not rewrite the backlog.

The rename fires only when the phase actually flips, never on the
investigating → identified → monitoring steps in between, because Discord rate
limits a thread rename to twice per ten minutes while messages are far cheaper.
A rename that fails is logged and dropped: the update itself has already been
posted, and the marker is a convenience for reading the channel list.

Setting it up:

1. https://discord.com/developers/applications → New Application → **Bot** → copy the token
2. **OAuth2 → URL Generator** → scope `bot`, permission **Manage Threads** → open the generated URL and add it to the server
3. Put the token in the SSM parameter Terraform creates for it

## Behaviour

- Reads `incidents.json` and `scheduled-maintenances.json`. Not `summary.json`:
  that one carries only *unresolved* incidents, so an incident vanishes from it
  the moment it is resolved — precisely the update that must be delivered.
- Updates are posted oldest first, so a thread reads in chronological order.
- State (thread id per incident, update ids already posted) lives in one S3
  object per function, so a re-run never double-posts.
- Anything older than `MAX_UPDATE_AGE` is recorded as seen **without** being
  posted. This is what stops the first run after a deploy, or the first run after
  the function has been broken for a day, from replaying resolved incidents.
- If a post fails, that incident stops for this run and is retried whole on the
  next one; other incidents still go out. A thread deleted in Discord (404) is
  replaced rather than wedging the incident forever.
- 429 and 5xx are retried in process, honouring Discord's `retry_after`.

## Configuration

Environment variables, set by Terraform:

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `DISCORD_WEBHOOK_PARAMETER_NAME` | yes | — | SSM parameter holding the forum channel's webhook URL |
| `STATE_BUCKET` | yes | — | S3 bucket for the state object |
| `STATUS_PAGE_BASE_URL` | no | Claude's page | Statuspage API root, e.g. `https://www.githubstatus.com/api/v2` |
| `PAGE_LABEL` | no | *(none)* | Names the page in Discord: webhook username `<label> Status` and thread prefix. Unset degrades to `Status` with no prefix |
| `STATE_KEY` | no | `statuspage/state.json` | Key of the state object. **Must differ per function** |
| `MENTION_ROLE_ID` | no | *(none)* | Discord **role** id mentioned when a thread opens and when it resolves. Not the server id — that is the `@everyone` role, which renders as `@@everyone` and notifies nobody |
| `DISCORD_BOT_TOKEN_PARAMETER_NAME` | no | *(none)* | SSM parameter holding a bot token, used only to flip the thread title marker on resolution |
| `MAX_UPDATE_AGE` | no | `24h` | Updates older than this are absorbed silently |
| `STATE_RETENTION` | no | `720h` | How long an incident stays in the state object |

The webhook URL is a secret and lives in SSM Parameter Store
(`SecureString`), never in the function's environment: environment variables are
readable by anyone who can call `GetFunctionConfiguration`. `PAGE_LABEL` and
`MENTION_ROLE_ID` are not secrets — a role id identifies a role inside one server
and every member of it can see it — so they sit in the environment.

## Adding another status page

Check it is an Atlassian Statuspage first — `curl -sf <page>/api/v2/incidents.json`
is the whole test. Then:

1. Add an entry to `statuspage_notify_pages` in `aws-terraform`'s root
   `locals.tf` (label and API base URL). Everything else — function, role, log
   group, EventBridge rule, IAM policies, deploy permission — is generated from
   it. Apply.
2. Add `<key>-status-notify` to `FUNCTION_NAMES` in
   [.github/workflows/build.yml](.github/workflows/build.yml).

Terraform first: the deploy step fails on `ResourceNotFoundException` if it is
told to update a function that does not exist yet.

A page's first run posts nothing older than `MAX_UPDATE_AGE`, so adding one does
not dump its incident history into the channel.

## Local run

```bash
go test ./...
```

## CI/CD

[.github/workflows/build.yml](.github/workflows/build.yml) vets and tests on pull
requests and on pushes to `main`. On push to `main` it also builds the `bootstrap`
binary, zips it, uploads it to
`s3://aws-terraform-lambda-artifacts-<account_id>-us-east-1/lambda/statuspage-notify.zip`
and calls `aws lambda update-function-code` for every function in `FUNCTION_NAMES`.

AWS calls authenticate via GitHub OIDC, assuming the
`github-actions-lambda-artifacts` role defined in `tamura09/aws-terraform`.
Terraform still owns the AWS resources; this repository owns only the function
source and its build pipeline.
