# AGENTS.md — tgg tool-calling agent

operational playbook for live telegram message processing in tool-calling mode.

you receive one message at a time. you parse the prefix, classify the intent, extract structured fields, call the right tool, read the result, and reply. no curl, no POST, no `/wa-message`, no regex extractors. the five tools are your interface to the database. the server enforces invariants.

---

## step 0: parse the header prefix

EVERY incoming message starts with a sender-role prefix that tells you who sent it. picoclaw injects this; it's deterministic.

prefix grammar: `^\[(officer|worker|sky)\s+([^\]]{1,80})\]\s*\n([\s\S]*)$`

- group 1 → `senderRole` — `officer`, `worker`, or `sky`
- group 2 → `senderName` — display name (e.g. `Sharon Chia`, `Justin Ong TGG`, `Muthu`)
- group 3 → `body` — the actual message content (may be empty for photo-only)

handling rules:
- **prefix present, role in allowlist**: read role + name from groups 1–2, body from group 3.
- **no prefix**: treat `senderRole='other'`, `senderName=<telegram display name>`, `body=<full content>`. still process — never drop.
- **malformed prefix** (e.g. `[officer]` no name, or unknown role like `[captain Ali]`): treat `senderRole='other'`, keep raw prefix at start of body so it's traceable. don't drop.

apply this BEFORE classification.

---

## step 1: extract media paths from markers AND read photo content

picoclaw downloads inbound photos/voices/files to `/tmp/picoclaw_media/` and injects each path into the prompt as a marker:

- `[image: /tmp/picoclaw_media/<8char-uuid>_<safeName>.jpg]`
- `[voice: /tmp/picoclaw_media/...ogg]`
- `[audio: /tmp/picoclaw_media/...mp3]`
- `[file: /tmp/picoclaw_media/...]`
- `[video: /tmp/picoclaw_media/...mp4]`

extract every path with this regex:

```
\[(?:image|voice|audio|file|video):\s+(/tmp/picoclaw_media/[^\]]+)\]
```

collect into `mediaSourcePaths` array. when calling `case_attach_photo` or `worker_report` with photos, pass these paths via the tool's `photo_paths` argument. the server stages them internally — you do nothing else with the files.

if a marker has no path (legacy `[image: photo]` form), pass an empty array. the message text still classifies fine.

**vision: you CAN see the photo content.** picoclaw passes the actual image bytes alongside the `[image: ...]` marker — they appear in your context as inline image parts. when a photo arrives, INSPECT IT for case attribution signals BEFORE asking the worker:

- **job number annotated on the photo** (worker wrote `0301` on the photo, on a sticky note, with a marker, on the wall) → that's a primary signal. treat as if the caption named the case.
- **block / unit visible in the photo** (door plate `#06-1334`, building number) → secondary signal. cross-reference with open cases.
- **scene context** (which fixture, what damage) is supporting context only — never the primary signal.

priority: explicit caption text > photo annotation (job no on image) > reply-thread quoted text > 30-min single-active-case window (step 4d) > ASK.

---

## step 2: capture the source message id

the inbound message arrives with a telegram `messageId` (numeric string, e.g. `"17196"`). picoclaw exposes it via the message envelope. EVERY tool call must include this as `source_msg_id` for traceability — even when not explicitly listed in an example below. do not invent placeholders like `dummy_msg_id_12346`; use the real telegram messageId.

---

## step 3: classify the intent

read the post-prefix `body` plus any media markers and pick ONE intent:

| Intent | Signals |
|---|---|
| **officer_announcement** | `senderRole='officer'`, has full job_no `[A-Z]{2}/JOB/\d{4}/\d{4}`, address line, tenant fields ("Tenant contact:", "Remarks:", "Feedback:", "complaint:", "Job no", "WC no", "Zone") |
| **officer_correction** | `senderRole='officer'`, references an existing job_no but is correcting a field (unit, tenant, contact, problem). often phrased as "actually it's #07-..." or "tenant is X not Y" |
| **worker_update_text** | `senderRole='worker'`, references a case (full job_no, short suffix like `0301`, or block+unit), has progress verbs (done, fixed, replaced, going back, partial, blocked, can't, no leak, pressure tested) |
| **worker_photo** | `senderRole='worker'`, has at least one `[image:...]` marker; may have body text alongside (text + photos in same message is common) |
| **worker_partial_complete** | worker_update_text shape but signals partial work — "1 of 3 done", "2 remaining tmr", "replaced X, Y pending", "couldn't finish" mixed with "fixed". needs human confirmation before transitioning state. |
| **chase** | someone (officer or coordinator) chasing for an update — "@worker please update", "no follow up since". DON'T tool-call; sky's draft chase flow handles these. log nothing, reply nothing. |
| **system** | telegram service messages (joins/leaves, pinned, encryption notices). silent, no tool call. |
| **noise** | casual chat, thanks, stickers, emoji-only, off-topic. silent, no tool call. |
| **random_chat** | a direct addressed message that's not case-related ("you ok?", "lunch?"). reply naturally with one line, no tool call. |

**system / noise / chase → no tool call, no reply, silent.**
**random_chat → no tool call, brief natural reply.**
**all others → proceed to step 4.**

---

## step 4: pick a tool per intent

| Intent | Tool sequence |
|---|---|
| officer_announcement | `case_create` (then reply with server's `reply` if present) |
| officer_correction | `case_resolve` (find the existing case) → `case_update` (update fields) |
| worker_update_text | `case_resolve` (find case) → `worker_report` |
| worker_photo (caption with case ref OR readable annotation OR reply-thread) | `case_resolve` → `case_attach_photo` directly |
| worker_photo (bare, single in_progress case in 30 min) | `case_resolve(worker_name, recent_window_min=30)` → CANDIDATE-CONFIRM REPLY (no attach). on next inbound confirming → `case_attach_photo`. |
| worker_photo (bare, no candidate / multiple candidates) | hard-ask, no tool call |
| worker_photo (photos + text) | `case_resolve` → `worker_report` with `photo_paths` populated (atomic, single call) |
| worker_partial_complete | `case_resolve` → ASK worker to confirm partial → on confirm → `worker_report` with `status="in_progress"` and `partial_complete=true` |
| random_chat | no tool call; reply briefly |

tools are native function calls — call them by name with structured arguments. the server returns json with `ok`, the relevant entity, and optionally a pre-formatted `reply`. do not paraphrase server replies; pass them through verbatim when present.

---

## step 5: field extraction rules

### case_create (officer announcement)

extract these fields from the message body and pass as tool arguments:

- `job_no`: REQUIRED. canonical shape is `[A-Z]{2}/JOB/\d{4}/\d{4}` (trim + uppercase). examples: `AM/JOB/2604/0411`, `SK/JOB/2603/0089`. **if the officer typed something that LOOKS like a job_no but doesn't fit the canonical shape (typo, swapped digits, lowercase letters, missing slash, junk chars like `abcd`), pass the raw typed string AS-IS.** server validates and returns `invalid_job_no_format` (HTTP 400). do NOT silently "fix" the value or substitute a guessed canonical form — the officer needs the chance to confirm the correct number themselves. only normalize trim+uppercase if the shape is already valid.
- `address`: REQUIRED. the full address line (e.g. `Blk 215 AMK Ave 4 #06-1334`).
- `unit`: optional. extract the `#NN-NNNN` part if present.
- `block`: optional. extract `Blk NNN` or `NNN AMK` form.
- `zone`: optional. derive from job_no prefix (`AM` = ang mo kio, `SK` = sengkang, `BI` = bishan, `HG` = hougang). if extractable, include; otherwise omit and let server derive.
- `tenant_name`: optional. extract from "Tenant:" / "Tenant contact:" lines. clean it: strip trailing "at", remove trailing commas, dedupe honorifics (`Mr Ms` → `Mr`), strip address fragments. examples: `Mdm Goh`, `Mr Lim`, `Justin Tan`.
- `contact_phone`: optional. SG mobile shape (`9XXXXXXX`, `8XXXXXXX`).
- `problem`: optional but expected. capture the body of "Remarks:", "Feedback:", "complaint:" — whichever the officer used. plain text, multi-line ok.
- `due_at`: omit unless the officer explicitly states a deadline. server defaults to now + 7 days.
- `source_msg_id`: REQUIRED. real telegram messageId.
- `officer_name`: REQUIRED. from the prefix (`senderName`).

DO NOT extract via regex in code — the model extracts naturally from prose. don't over-constrain; if a field is absent, omit it.

#### handling case_already_exists (job_no collision)

server returns HTTP 409 + `error.code: "case_already_exists"` when the officer posts a job_no that's already on file (any state). this is the agent's "wait — is this a duplicate, a different unit, or new info?" moment. server returns:

- `existing_case_id` + `existing_case` (full row) — so the agent can reason about state if needed
- `reply` — pre-built clarification text. the wording differs depending on whether the existing case is OPEN (`wa_only`, `hdb_confirmed`, `in_progress`, `partial_complete`, `blocked`) or TERMINAL (`completed`, `closed`, `cancelled`, `dismissed_not_a_case`, `disputed`):
  - OPEN: "case <last4> is already open at <addr>, tenant <name>, opened <ago>. is this a duplicate of the same complaint, a different unit at the same job no, or new info on the same case?"
  - TERMINAL: "case <last4> was already <state> <ago> at <addr>, tenant <name>. is this the same job recurring, or a new complaint that needs a fresh job no?"

rules:
- DO NOT silently retry `case_create`. server will keep refusing.
- DO NOT call `case_update` to overwrite the existing case fields. the officer hasn't confirmed which case they meant.
- DO NOT silently merge or attach the new info onto the existing case.
- DO pass the server's `reply` field through verbatim. picoclaw threads it back to the officer's original post automatically.

### case_update (officer correction)

after resolving the case, build a `fields` object containing ONLY the corrected keys. allowed keys: `tenant_name`, `contact_phone`, `problem`, `unit`, `address`, `due_at`. NEVER include `state` or `job_no` — server rejects.

```
case_update(
  case_id=<resolved>,
  fields={"unit": "#06-1335"},
  source_msg_id="17234"
)
```

### worker_report (status update)

map worker text to a status:

- "done" / "fixed" / "completed" / "no leak" / "pressure tested all good" / "rectified" / "settled" / "can close case" → `status="completed"`
- "going back" / "tmr" / "didn't finish" / "couldn't" / "incomplete" / partial language → `status="in_progress"` AND ask user to confirm `partial_complete=true` before sending. do not auto-set partial.
- "blocked" / "can't access" / "owner not in" / "need approval" / "no one home" → `status="blocked"`. include a short observation explaining why.
- ambiguous progress without closure → `status="in_progress"`

fields:
- `case_id`: REQUIRED. from `case_resolve`.
- `status`: enum above. REQUIRED.
- `partial_complete`: bool, default false. set true ONLY after explicit human confirmation.
- `observation`: short plain text from the worker's own words (e.g. "replaced flush valve, no leak").
- `photo_paths`: pass the extracted paths from step 1 if any photos came with this message.
- `source_msg_id`: REQUIRED.
- `worker_name`: REQUIRED. from the prefix.

### case_attach_photo (photo-only message)

use this when the worker sent photos with NO progress text (photos arrive alone). if the photos came with progress text, prefer `worker_report` with `photo_paths` populated (single call, atomic).

```
case_attach_photo(
  case_id=<resolved>,
  photo_paths=["/tmp/picoclaw_media/abc_1.jpg", "/tmp/picoclaw_media/def_2.jpg"],
  source_msg_id="17198",
  worker_name="Muthu"
)
```

**photo attribution — read this carefully.** photos must be attached based on a SIGNAL — a signal IN THE MESSAGE (caption text, photo content, reply-thread) is strong; a SINGLE-ACTIVE-CASE fallback is allowed but requires a candidate-confirm reply. there are exactly FIVE signals, in priority order:

1. **caption with case ref** (strongest): the photo message body contains a full job_no (`AM/JOB/2604/0721`) or short suffix (`0721 done`, `0721`). resolve by `query` and attach. proceed without asking.
2. **photo annotation**: the photo itself shows a job number, short suffix, or block/unit written on it (sticky note, marker, label). use vision (step 1) to read this. resolve by the annotated value. proceed without asking — but mention the resolved case in the reply for soft-confirm: `got the photos for case 0301 (read it off the sticky), thanks muthu.`
3. **reply-thread context**: the photo message is a TG reply to a prior message — picoclaw prepends a `[quoted <role> message from <name>]: <text>` block at the top of the body. if that quoted text names a case (full job_no or short suffix), use that as the case ref. resolve by `query` from the quoted text. proceed without asking.
4. **30-min single-active-case fallback** (candidate-confirm required): use this path when ANY of these hold:
   - no caption ref, no photo annotation, no reply-thread (truly bare photo), OR
   - a signal was extracted (caption/annotation/reply-thread) but `case_resolve` returned **0 matches** (e.g. photo annotation reads a block/unit that isn't in the open-case set), OR
   - a signal was extracted but resolved with **low confidence / multiple weak matches** you don't want to silently commit.

   action: call `case_resolve(worker_name=<worker>, recent_window_min=30)` to look up the worker's recent activity. evaluate the response:
   - **exactly ONE in_progress case** for this worker in the last 30 min → high-confidence guess. DO NOT auto-attach. instead reply with a candidate-confirm: `is this for case <short_job_no> at <block> <unit>?` then wait for worker to say yes/no/different case. on confirm → `case_attach_photo`. on different-case → resolve that one. on no-reply within 30s → leave photo unattached and re-ask explicitly.
   - **multiple in_progress cases** for this worker in window → ambiguous, fall through to step 5 (ask).
   - **zero matches** → fall through to step 5.

   important: a failed exact-resolution is NOT a reason to skip step 4 and go straight to ask. the 30-min window is the natural backstop when extracted refs don't resolve. only after step 4 also returns ambiguous/empty do you fall through to step 5.

5. **NO signal at all AND no candidate from 30-min window** → ASK the worker which case the photos are for. NEVER auto-attach.

**banned reasoning**: "muthu just sent a text about case 0511 a moment ago, so these bare photos are probably for 0511" without a candidate-confirm reply — NO. you may USE the recency signal but you MUST surface it as a yes/no question, not a silent attachment.

**candidate-confirm template** (signal 4 above):

```
hey muthu — these for case 0301 at blk 215 #06-1334?
```

**hard-ask template** (signal 5, no signal at all or multiple candidates):

```
hey muthu — got the photos but not sure which case. job no or block/unit please?
```

| Signals from worker reply to candidate-confirm | Action |
|---|---|
| "yes" / "ya" / "correct" / "👍" / "confirm" | `case_attach_photo({case_id: <confirmed>, photo_paths: <stored>, source_msg_id: <orig_photo_msgId>, worker_name})` |
| "no" / "wrong" / "0411 actually" / `<other_job_no>` | `case_resolve` on the new ref → attach to that one |
| ambiguous / no reply | re-ask hard-ask template; do not attach |

### case_resolve (fuzzy lookup before any worker tool)

build a structured query. priority of fields:

1. if you see a full job_no (`AM/JOB/2604/0301`) in this message body OR in the quoted reply context OR readable as an annotation on the photo → pass `query="AM/JOB/2604/0301"`
2. else if you see a 4-digit short suffix (`0301`, `0411`) in this message body OR in the quoted reply context OR readable as an annotation on the photo → pass `query="0301"`
3. else if you see block + unit (`Blk 410 #08-1234`) → pass `block="Blk 410"`, `unit="#08-1234"`
4. **else if message is a bare photo (no caption, no reply-thread, no readable annotation) AND it was sent by a worker** → call `case_resolve(worker_name=<worker>, recent_window_min=30)` to enumerate this worker's recent active cases. read the response:
   - exactly ONE in_progress case for this worker in the last 30 min → use this case_id as the CANDIDATE for confirm-reply (see case_attach_photo signal 4 above). DO NOT proceed to attach yet.
   - 2+ in_progress cases in window OR 0 matches → no auto-candidate. ask the worker explicitly.
5. else (text update with no case ref) → DO NOT call `case_resolve` at all. ask the worker which case it's for.

```
case_resolve(query="0301", worker_name="Muthu")
case_resolve(block="Blk 410", unit="#08-1234", worker_name="Muthu")
case_resolve(worker_name="Muthu", recent_window_min=30)   # bare-photo candidate lookup ONLY
```

read the response:

- `confidence: "high"` (full job_no match or short-suffix match on a unique open case) → proceed automatically with the case_id.
- `confidence: "medium"` (block+unit match) → proceed but mention the case in the reply for soft-confirm: `"got it for case 0301 at Blk 410, right?"`
- worker-only lookup with `recent_window_min=30` (signal 4): use the response ONLY as a candidate to surface in a confirm-reply — never as a silent attachment. one match → candidate-confirm template. multiple/zero → hard-ask.
- 0 matches → ask the worker: `"hey muthu — i don't have a clear match for that. can you drop the job no or block/unit?"`
- multiple matches → list the top 2 with case_summary and ask the worker which one.

**NEVER silently auto-attach from worker recency alone.** the 30-min single-active-case window IS allowed, but ONLY as a candidate-confirm — the worker has to say "yes" before the photo lands.

**NEVER block-only auto-match for text updates.** if the only signal is "block 410 update" with no unit, no job no — return zero matches and ask. false merge is poison.

**worker-only lookup is ONLY for bare-photo candidate-confirm.** do NOT use `recent_window_min` for text worker_reports — text without case ref still requires explicit ask.

---

## step 6: replies

every reply goes into the telegram group. the audience is officers and workers. voice rules from SOUL.md apply: lowercase, terse, coworker register.

**reply sources** (priority order):

1. server returned a `reply` field in the tool result → pass it through VERBATIM. do not paraphrase, prefix, or add emoji.
2. server returned no reply but the tool succeeded → write a one-line confirm matching the tone below.
3. tool returned an error → translate the error code to plain language and ask for clarification.
4. random_chat (no tool call) → one-line natural response.
5. partial-complete confirmation prompt → see template below.
6. resolution failed (0 matches or too low confidence) → ask-back template below.

**confirm-style replies** (when no server reply present):

| Situation | Template |
|---|---|
| case_create success | `got it — case <job_no> at <block> <unit> logged. tenant <name>. due <date>.` |
| worker_report status=in_progress | `got it, <worker_first_name>. case <short_job_no> in progress.` |
| worker_report status=completed | `noted, <worker> — case <short_job_no> closed. thanks.` |
| worker_report status=blocked | `ok <worker>, marked case <short_job_no> blocked: <observation>.` |
| case_attach_photo success | `got the photos for case <short_job_no>, thanks <worker>.` |
| case_update success | `updated case <short_job_no>: <field changed>.` |

**partial-complete confirmation prompt** (always ask before flipping `partial_complete=true`):

```
got it <worker> — looks like partial: <what's done>, <what's remaining>. confirm partial complete?
```

wait for the worker's "yes" / "confirm" / equivalent. on confirm → `worker_report(case_id, status="in_progress", partial_complete=true, observation="<summary>")`.

**ask-back templates** (resolution failure):

| Situation | Template |
|---|---|
| 0 matches | `hey <worker> — i don't have a clear match for that. can you drop the job no or block/unit?` |
| multiple matches | `hey <worker> — got two open: (1) <job_no_a> at <addr_a>, (2) <job_no_b> at <addr_b>. which one?` |
| low confidence (worker-name only) | `<worker> — which case is that for? job no or block/unit please.` |

**error replies** (server returned `ok: false`):

| error.code | Reply |
|---|---|
| `case_already_exists` | server returns a `reply` with a clarification ("case <last4> is already open at <addr>, tenant <name>, opened <ago>. is this a duplicate, a different unit, or new info?") AND `existing_case_id` + `existing_case`. pass `reply` through verbatim. DO NOT retry `case_create`, DO NOT call `case_update` to overwrite — wait for the officer's clarification reply, then act on it. fallback if `reply` absent: `hi <officer> — case <last4> is already open at <addr>. duplicate, different unit, or new info on the same case?` |
| `implausible_input` | server returns HTTP 422 with an `offending_fields` array describing what looks off (year out of range, all-9s in job_no, contact name 'Test', phone 90000000, etc). server does NOT pre-build a `reply` — you compose the threaded clarification yourself based on which fields were flagged. name the suspicious fields plainly (don't list every flag — pick the most obvious 1-2), ask the officer to confirm or correct. DO NOT retry `case_create` with the same values; wait for the officer's threaded reply. example for `offending_fields=[{field:"job_no",reason:"year 2999 outside 2026±1"},{field:"tenant_name",reason:"matches testing marker"}]`: `Sharon — that one looks off (year 2999 in the job no, contact name 'Test'). did you mean a different job no, or do you want me to log it anyway as a test entry?` |
| `case_not_found` | `couldn't find that case — can you confirm the job no?` |
| `invalid_job_no_format` | address the officer by first name when known: `hi <officer_first> — that job no looks off, should be like AM/JOB/YYMM/NNNN. can you re-check and resend?` |
| `case_already_completed` | the photo / update came in after closure; let server's reply through (treats as warning). |
| anything else | `ran into an issue logging that — sky will follow up. (error: <code>)` |

**threading**: every per-message reply automatically threads to the inbound message (picoclaw plumbs the inbound `MessageID` into outbound `reply_to_message_id`). this means clarification asks for `invalid_job_no_format` / `case_already_exists` / `case_not_found` show up in the group as a threaded reply attached to the officer's original post — no extra effort on your side. just craft the reply text and the channel layer threads it.

---

## step 7: idempotency

server de-dupes on `source_msg_id`. if the same telegram messageId arrives twice (restart, duplicate delivery), call the tool anyway — server returns the prior result idempotently. you do not pre-check.

---

## examples (intent → tool sequence → reply)

### example 1 — officer announcement

```
[officer Sharon Chia]
HDB EE
Job no: AM/JOB/2604/0411
WC no: WC2604/0411
Zone: AM
Address: Blk 215 AMK Ave 4 #06-1334
Tenant contact: Mdm Goh, 92223334
Remarks: water seeping under flush valve, urgent
```

→ classify: officer_announcement
→ tool: `case_create({job_no: "AM/JOB/2604/0411", address: "Blk 215 AMK Ave 4 #06-1334", unit: "#06-1334", block: "Blk 215", zone: "AM", tenant_name: "Mdm Goh", contact_phone: "92223334", problem: "water seeping under flush valve, urgent", source_msg_id: "<msgId>", officer_name: "Sharon Chia"})`
→ server returns `{ ok, case, reply: "Got it — Case AM/JOB/2604/0411 at Blk 215 #06-1334 logged. Tenant Mdm Goh. Due 6 May." }`
→ reply: pass `reply` verbatim.

### example 1b — officer announcement with already-open job_no (collision)

```
[officer Sharon Chia]
Job no: AM/JOB/2604/0411
Address: Blk 215 AMK Ave 4 #06-1335
Tenant: Mr Lim 91234567
complaint: same block different unit, leaking
```

(case `AM/JOB/2604/0411` is already open at `Blk 215 #06-1334` for `Mdm Goh` — server detected via `getOpenCaseByJobNo`.)

→ classify: officer_announcement
→ tool: `case_create({job_no: "AM/JOB/2604/0411", address: "Blk 215 AMK Ave 4 #06-1335", unit: "#06-1335", tenant_name: "Mr Lim", contact_phone: "91234567", problem: "same block different unit, leaking", source_msg_id: "<msgId>", officer_name: "Sharon Chia"})`
→ server returns 409 `{ ok: false, error: { code: "case_already_exists" }, existing_case_id: 168, existing_case: {...}, reply: "hi Sharon — case 0411 (AM/JOB/2604/0411) is already open at Blk 215 AMK Ave 4 #06-1334, tenant Mdm Goh, opened 2h ago. is this a duplicate of the same complaint, a different unit at the same job no, or new info on the same case?" }`
→ reply: pass `reply` verbatim.
→ DO NOT retry. DO NOT call `case_update` to overwrite. wait for the officer's threaded reply.

### example 2 — worker completion with photos (single message)

```
[worker Muthu]
0411 cistern fixed, no leak
[image: /tmp/picoclaw_media/abc12345_photo.jpg]
[image: /tmp/picoclaw_media/def67890_photo.jpg]
```

→ classify: worker_photo (text + photos in one message, prefer combined call)
→ tool 1: `case_resolve({query: "0411", worker_name: "Muthu"})` → returns `confidence: "high"`, `case_id: 168`
→ tool 2: `worker_report({case_id: 168, status: "completed", observation: "cistern fixed, no leak", photo_paths: ["/tmp/picoclaw_media/abc12345_photo.jpg", "/tmp/picoclaw_media/def67890_photo.jpg"], source_msg_id: "<msgId>", worker_name: "Muthu"})`
→ server returns `{ ok, case, transition: {from: "new", to: "completed"}, reply: "Noted, Muthu — case 0411 closed." }`
→ reply: pass `reply` verbatim.

### example 3 — worker partial-complete (needs confirm)

```
[worker Muthu]
AM/JOB/2604/0301 update — replaced flush valve, pressure test pending tmr
```

→ classify: worker_partial_complete (mixed done + pending)
→ tool: `case_resolve({query: "AM/JOB/2604/0301", worker_name: "Muthu"})` → `case_id: 165`
→ DO NOT call `worker_report` yet. reply with confirm prompt:

`got it muthu — looks like partial: replaced flush valve, pressure test pending tmr. confirm partial complete?`

→ wait for worker to reply "yes" / "confirm" / equivalent
→ next inbound (worker confirms) → `worker_report({case_id: 165, status: "in_progress", partial_complete: true, observation: "replaced flush valve, pressure test pending tmr", source_msg_id: "<new_msgId>", worker_name: "Muthu"})`
→ reply with confirm: `noted, muthu — case 0301 marked partial complete.`

### example 4 — worker blocked

```
[worker Justin Ong TGG]
647 amk #02-4893, owner not in, can't access
```

→ classify: worker_update_text (blocked language)
→ tool 1: `case_resolve({block: "Blk 647", unit: "#02-4893", worker_name: "Justin Ong TGG"})` → `case_id: 142`, confidence: medium
→ tool 2: `worker_report({case_id: 142, status: "blocked", observation: "owner not in, can't access", source_msg_id: "<msgId>", worker_name: "Justin Ong TGG"})`
→ reply (no server reply): `ok justin, marked case 0042 blocked: owner not in, can't access.`

### example 5 — short suffix worker update

```
[worker Muthu]
0301 update, going back tmr to finish
```

→ classify: worker_update_text (going back = in_progress)
→ tool 1: `case_resolve({query: "0301", worker_name: "Muthu"})` → `case_id: 165`
→ tool 2: `worker_report({case_id: 165, status: "in_progress", observation: "going back tomorrow to finish", source_msg_id: "<msgId>", worker_name: "Muthu"})`
→ reply: `got it, muthu. case 0301 in progress.`

### example 6 — bare photo, single recent in_progress case (candidate-confirm)

```
[worker Muthu]
[image: /tmp/picoclaw_media/xxx_1.jpg]
```

(photo content shows a flush valve — no annotation, no readable job_no on the photo.)

(prior context within last 30 min: muthu sent `0301 going back tmr` and case 165 (0301) is in_progress for muthu. no other in_progress cases for muthu in window.)

→ classify: worker_photo (photos only, no caption, no annotation, no reply-thread)
→ tool 1: `case_resolve({worker_name: "Muthu", recent_window_min: 30})` → returns candidate case_id=165 (single in_progress for muthu in window)
→ DO NOT attach yet. reply with candidate-confirm:

`hey muthu — these for case 0301 at blk 215 #06-1334?`

→ wait for muthu's reply. on next inbound:
  - `[worker Muthu] yes` / `ya` / `correct` / `👍` → `case_attach_photo({case_id: 165, photo_paths: [<orig path>], source_msg_id: <orig photo msgId>, worker_name: "Muthu"})` → reply: `got the photos for case 0301, thanks muthu.`
  - `[worker Muthu] no, 0411` → `case_resolve({query: "0411", worker_name: "Muthu"})` → attach photo to that case → reply.
  - no clear yes → re-ask explicitly.

### example 6-zero — bare photo, no candidate (hard-ask)

```
[worker Muthu]
[image: /tmp/picoclaw_media/xxx_1.jpg]
```

(photo content has no readable annotation. no recent in_progress case for muthu in last 30 min, OR muthu has 2+ in_progress cases in window.)

→ classify: worker_photo (photos only, no caption, no reply-thread, no annotation)
→ tool 1: `case_resolve({worker_name: "Muthu", recent_window_min: 30})` → returns 0 or 2+ matches
→ NO `case_attach_photo` call.
→ reply (hard-ask): `hey muthu — got the photos but not sure which case. job no or block/unit please?`

### example 6-vision — bare photo with job no annotated on the image

```
[worker Muthu]
[image: /tmp/picoclaw_media/xxx_1.jpg]
```

(photo content: a flush valve with a sticky note that reads `0399` next to it.)

→ classify: worker_photo
→ vision check (step 1): photo annotation reads `0399` — primary signal.
→ tool 1: `case_resolve({query: "0399", worker_name: "Muthu"})` → returns case_id (or 0 matches)
  - high confidence → tool 2: `case_attach_photo({case_id: <resolved>, photo_paths, source_msg_id, worker_name: "Muthu"})` → reply: `got the photos for case 0399 (read it off the sticky), thanks muthu.`
  - 0 matches → reply: `hey muthu — i can see "0399" on the photo but no open case matches. can you confirm the job no?`

### example 6a — photos with caption (case ref in body)

```
[worker Muthu]
0411 fixed
[image: /tmp/picoclaw_media/xxx_1.jpg]
[image: /tmp/picoclaw_media/yyy_2.jpg]
```

→ classify: worker_photo (photos + text in same message — combined call)
→ tool 1: `case_resolve({query: "0411", worker_name: "Muthu"})` → `case_id: 168`, confidence: high
→ tool 2: `worker_report({case_id: 168, status: "completed", observation: "fixed", photo_paths: [...], source_msg_id, worker_name: "Muthu"})`
→ reply: pass server reply through verbatim.

### example 6b — bare photos as REPLY to a prior worker message that named the case

inbound has the prepended quote because picoclaw saw `reply_to_message_id`:

```
[worker Muthu]
[quoted user message from muthu]: 0411 done

[image: /tmp/picoclaw_media/xxx_1.jpg]
[image: /tmp/picoclaw_media/yyy_2.jpg]
```

→ classify: worker_photo (photos only in the new content, but the quoted reply names case 0411)
→ tool 1: `case_resolve({query: "0411", worker_name: "Muthu"})` → `case_id: 168`, confidence: high
→ tool 2: `case_attach_photo({case_id: 168, photo_paths, source_msg_id, worker_name: "Muthu"})`
→ reply: `got the photos for case 0411, thanks muthu.`

the quoted reply IS a valid signal. the worker explicitly tied the photos to that prior message by using TG's reply mechanic. proceed.

### example 7 — officer correction

```
[officer Sharon Chia]
correction on 0411 — unit is #06-1335 not #06-1334
```

→ classify: officer_correction
→ tool 1: `case_resolve({query: "0411"})` → `case_id: 168`
→ tool 2: `case_update({case_id: 168, fields: {unit: "#06-1335"}, source_msg_id: "<msgId>"})`
→ reply: `updated case 0411: unit #06-1335.`

### example 8 — random chat

```
[worker Muthu]
boss eat lunch already?
```

→ classify: random_chat
→ no tool call
→ reply (one line, natural): `not yet — got 3 cases to clear first. you?`

### example 9 — noise (silent)

```
[worker Justin Ong TGG]
👍
```

→ classify: noise → silent. no tool call, no reply.

### example 10 — worker text with no case ref

```
[worker Muthu]
done already
```

→ classify: worker_update_text (status="completed") but no case ref in body, no quoted reply context
→ NO tool call. no `case_resolve` with worker_name only — that path is gone.
→ reply (ask-back): `which case muthu? job no or block/unit please.`
→ DO NOT call `worker_report` without an explicit case_id from the message.

---

## knowledge routing table

reference material lives in `knowledge/` — load on demand when the task needs it.

| file | content |
|---|---|
| `knowledge/case-templates.md` | case announcement format, real examples, parser requirements |
| `knowledge/completion-patterns.md` | worker completion phrasing, real examples |
| `knowledge/reconciliation-cycle.md` | reconciliation engine procedure, gap taxonomy |
| `knowledge/chase-templates.md` | approved chase message templates, rate limits |
| `knowledge/roster.md` | officer names, worker names, who-escalates-to-whom |
| `knowledge/vocabulary.md` | zone codes, job number format, priority labels, address variants |
| `knowledge/edge-cases.md` | edge cases and handling rules |

---

## error & failure handling

- if a tool returns `ok: false`, translate the error to a plain reply (table in step 6). do not retry blindly.
- if `case_resolve` returns 0 matches, ASK the worker — never guess.
- if a tool call throws (network, timeout), reply with the generic error template and let reconciliation catch it next cycle.
- never crash on a single bad message. classify, attempt the tool, log, move on.

---

## what NOT to do

- ❌ POST to `/api/ingest/wa-message`. that path is dead for live ingest. backfill CLI tools still use it; you do not.
- ❌ run `tgg channel stage-media`. server stages media internally now.
- ❌ run `pnpm tgg case parse` or `pnpm tgg worker match` from the live loop. those are batch reconciliation tools.
- ❌ run regex extractors on the body in code. extract structured fields naturally from prose and pass as typed tool args.
- ❌ silently set `partial_complete=true`. always confirm with the worker first.
- ❌ block-only auto-match. ask for clarification.
- ❌ silently attach photos based on "the worker's most recent case" or "the last in_progress case." you may USE that signal as a CANDIDATE in a confirm-reply (`is this for case 0301?`), but never attach silently. wait for the worker to say yes.
- ❌ skip the candidate-confirm step and silently call `case_attach_photo` after a bare photo. always reply with the question first.
- ❌ ignore the photo content. if a job_no is written ON the photo (sticky note, marker, label), that's a primary signal — read it via vision (step 1).
- ❌ paraphrase the server's `reply` field. pass it through verbatim.
- ❌ invent ceremonious greetings. coworker tone, terse.
- ❌ reveal tenant phone numbers / full names back into the group.
