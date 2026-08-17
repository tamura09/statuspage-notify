# claude-status-notify

Polls [status.claude.com](https://status.claude.com) and reports every incident and
scheduled maintenance into a Discord **forum** channel, one thread per incident.

Deployed via [tamura09/aws-terraform](https://github.com/tamura09/aws-terraform) as the
`claude-status-notify` Lambda function (`provided.al2023` / `arm64`, us-east-1),
run every minute by EventBridge.

## Why a forum channel, and why not edit one message

The obvious design — post once and edit that message as the incident progresses —
does not work, because **Discord never sends a notification for a message edit**.
Only `MESSAGE_CREATE` produces a push notification or an unread badge; adding an
`@mention` during an edit does not change that. The recovery message, the one
update people actually want, would be the one nobody is told about.

So every Statuspage update is posted as its own message. To keep that from
flooding the channel, all the updates for one incident go into one thread:

| Statuspage | Discord |
| --- | --- |
| New incident or maintenance | New forum post (`thread_name`), titled `YYYY-MM-DD <incident name>` |
| `identified`, `monitoring`, … | New message inside that thread |
| `resolved` / `completed` | New message inside that thread, green, optionally with a role mention |

A **forum** channel specifically, because a webhook cannot create a thread in a
plain text channel — `thread_name` is only accepted on forum and media channels,
and `POST /channels/{id}/messages/{id}/threads` needs a bot token. Posting into an
existing thread (`?thread_id=`) works either way and un-archives it automatically.

Follow-ups inside a thread only notify members who already joined it, so set
`MENTION_ROLE_ID` if the recovery message needs to reach everyone. It is applied
to the thread-opening post and to the terminal update, and nothing else.

The forum channel must **not** have "Require members to select tags when posting"
enabled: this function sends no `applied_tags`, and Discord rejects the post with
a 400 if the channel demands one.

## Behaviour

- Reads `incidents.json` and `scheduled-maintenances.json`. Not `summary.json`:
  that one carries only *unresolved* incidents, so an incident vanishes from it
  the moment it is resolved — precisely the update that must be delivered.
- Updates are posted oldest first, so a thread reads in chronological order.
- State (thread id per incident, update ids already posted) lives in one S3
  object, so a re-run never double-posts.
- Anything older than `MAX_UPDATE_AGE` is recorded as seen **without** being
  posted. This is what stops the first run after a deploy, or the first run after
  the function has been broken for a day, from replaying resolved incidents.
- If a post fails, that incident stops for this run and is retried whole on the
  next one; other incidents still go out. A thread deleted in Discord (404) is
  replaced rather than wedging the incident forever.

## Configuration

Environment variables, set by Terraform:

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `DISCORD_WEBHOOK_PARAMETER_NAME` | yes | — | SSM parameter holding the forum channel's webhook URL |
| `STATE_BUCKET` | yes | — | S3 bucket for the state object |
| `STATE_KEY` | no | `claude-status/state.json` | Key of the state object |
| `MENTION_ROLE_ID` | no | *(none)* | Discord role mentioned when a thread opens and when it resolves |
| `STATUS_PAGE_BASE_URL` | no | `https://status.claude.com/api/v2` | Statuspage API root |
| `MAX_UPDATE_AGE` | no | `24h` | Updates older than this are absorbed silently |
| `STATE_RETENTION` | no | `720h` | How long an incident stays in the state object |

The webhook URL is a secret and lives in SSM Parameter Store
(`/claude-status-notify/discord-webhook-url`, `SecureString`), never in the
function's environment: environment variables are readable by anyone who can call
`GetFunctionConfiguration`.

## Local run

```bash
go test ./...
```

There is no live-posting test. To try it by hand against a throwaway forum
channel, set the environment variables above with real AWS credentials and invoke
the deployed function:

```bash
aws lambda invoke --function-name claude-status-notify --region us-east-1 /dev/stdout
```

## CI/CD

[.github/workflows/build.yml](.github/workflows/build.yml) vets and tests on pull
requests and on pushes to `main`. On push to `main` it also builds the `bootstrap`
binary, zips it, uploads it to
`s3://aws-terraform-lambda-artifacts-<account_id>-us-east-1/lambda/claude-status-notify.zip`
and calls `aws lambda update-function-code` so the deployed function picks it up
immediately.

AWS calls authenticate via GitHub OIDC, assuming the
`github-actions-lambda-artifacts` role defined in `tamura09/aws-terraform`.
Terraform still owns the AWS resources; this repository owns only the function
source and its build pipeline.
