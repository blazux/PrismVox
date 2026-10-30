<div align="center">
  <img src="logo.svg" width="80" alt="PrismVox"><br>
  <h1>PrismVox</h1>
  <p><em>An open-source AI phone receptionist for your SIP trunk or PBX.</em></p>
</div>

**[Documentation](https://www.thelastserver.com/docs/prismvox/installation/)**

PrismVox answers calls, retrieves information from your knowledge base, transfers
callers, records messages and makes outbound calls with a supplied mission. It
runs independently or connects to [Prism](https://github.com/blazux/prism), with
caller identification and access to the caller's permitted knowledge.

**MIT licensed.** Bring your own language model. ElevenLabs handles speech by
default; compatible self-hosted speech APIs are optional. No STT or TTS engine
is bundled or deployed.

![PrismVox call overview](docs/screenshots/overview.png)

## Features

- SIP telephony through Asterisk, including TLS/SRTP and UDP trunks.
- Streaming speech recognition and synthesis with ElevenLabs.
- Voice activity detection and caller interruption during playback.
- Blind and attended transfers, messages and an outbound call queue.
- Agent-controlled knowledge searches with PostgreSQL and pgvector.
- MCP tools over Streamable HTTP or legacy SSE.
- One shared conversation model for incoming calls, outbound missions and console
  chats, with separate instructions for inbound and outbound calls.
- Call history, transcripts, conversation supervision and a dark/light console.
- Optional Prism integration: recognized callers use Prism's voice agent;
  unknown callers stay with the switchboard.

## Requirements

- Docker and Docker Compose.
- A SIP trunk or PBX reachable from the deployment, including its media path.
- An OpenAI-compatible language model API with streaming and tool calling.
- An ElevenLabs account with Speech to Text and Text to Speech permissions.
  Models Read and Voices Read allow the console to load its catalogues.
- An OpenAI-compatible embedding API for semantic search in the standalone
  knowledge base. Ollama and vLLM can provide this API; configure its URL and
  model in Knowledge Base. Connected phone calls use Prism's knowledge sources.
- No GPU is required when your model and speech providers run elsewhere.

## Quick start

See the [installation guide](https://www.thelastserver.com/docs/prismvox/installation/)
for detailed setup instructions and links to SIP, speech-provider and console configuration.

```sh
git clone https://github.com/blazux/PrismVox.git
cd PrismVox
cp .env.example .env
```

Edit `.env`:

1. Replace `ARI_PASS`, `DB_PASS` and `UI_PASSWORD` with distinct strong passwords
   (`openssl rand -hex 24` generates one).
2. Set your `SIP_*` trunk values and `OPENAI_BASE_URL` / `OPENAI_MODEL`.
3. Set `AGENT_API_KEY` if you use the text agent or OpenAI-compatible API.

```sh
docker compose pull
docker compose up -d --no-build
```

Open **http://localhost:7860** and sign in using `UI_USER` / `UI_PASSWORD`.
The console opens a browser session for eight hours. Use **Sign out** to close
it. Existing API clients can continue to use HTTP Basic with the same credentials;
agent clients can use their separate `AGENT_API_KEY` Bearer token.
The HTTPS console at **https://localhost:7861** uses a self-signed certificate.
Both ports bind to localhost by default. For remote use, put the console behind
a trusted HTTPS reverse proxy; `UI_BIND` controls the published bind address.
Browser microphone access requires HTTPS or localhost.

The stack starts Asterisk, the Go call agent, the console and PostgreSQL/pgvector,
and uses your configured model, speech and embedding APIs. Persistent data lives
in Docker volumes. Embeddings for the standalone knowledge base are configured
from **Knowledge Base → Embedding provider**.

### Docker images and updates

`PRISMVOX_TAG=unstable` follows builds from `main`. Use `latest` for the most recent
versioned release, or a version tag such as `v1.0.0` to pin a release. Set the same
tag for all three PrismVox images through `.env`; `DOCKERHUB_NAMESPACE` defaults to `blazux`.

| Service | Docker Hub image |
| --- | --- |
| Call agent | `blazux/prismvox-agent` |
| Console | `blazux/prismvox-ui` |
| SIP server | `blazux/prismvox-asterisk` |

To update, retrieve the current Compose files with `git pull`, then run:

```sh
docker compose pull
docker compose up -d --no-build
```

Named volumes retain the database, encryption key, certificates and audio files.
The console runs from its image; an existing `./ui/app:/app` bind mount in a local
override must be removed to use the published console code.

To build from source, run `docker compose up -d --build`. For live console edits,
add the development override:

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

### Publishing images

The GitHub Actions workflow tests the call agent and console with a disposable
PostgreSQL database before building all three images for `linux/amd64` and
`linux/arm64`. Pushes to `main` publish `unstable`; tags beginning with `v` publish
that tag and `latest`. Pull requests run tests without publishing. The workflow
can also be started manually from **Actions → Build & Push Docker images**.

In the PrismVox repository, add these Actions secrets under
**Settings → Secrets and variables → Actions**:

- `DOCKERHUB_USERNAME`: the Docker Hub account used for publication.
- `DOCKERHUB_TOKEN`: a Docker Hub access token with write permission for the three
  image repositories.

These are the same secret names used by Prism. Configure them for PrismVox too,
or grant this repository access to the corresponding organization secrets.

To generate the token, sign in to [Docker Home](https://app.docker.com/), open
**Account settings → Personal access tokens → Generate new token**, enter a
description such as `GitHub PrismVox`, and grant repository read/write access.
Copy the generated token; Docker displays its value only once. On GitHub, click
**New repository secret**, enter `DOCKERHUB_TOKEN` in **Name**, paste the copied
token into **Secret**, then click **Add secret**. Repeat with
`DOCKERHUB_USERNAME` as **Name** and your Docker Hub login as **Secret**.

### Configure ElevenLabs

![PrismVox speech-provider settings](docs/screenshots/speech.png)

In **Settings → Voice & greeting → Speech providers**:

1. Select **ElevenLabs** for recognition and synthesis.
2. Enter your API key and click **Connect**.
3. Choose a voice and a speech model, then **Save voice settings**.
4. Use the voice preview in Settings → Voice & greeting to hear the selected voice.

Defaults are `scribe_v2_realtime` for recognition and `eleven_flash_v2_5` for
synthesis, with English as the default greeting language. Existing saved personas,
greetings and phrases are preserved when upgrading.

**Connect** loads the available models and voices; **Save** persists the settings.
If Models Read is unavailable, your selected model is retained and the console
explains how to enable the full catalogue. A blank API key input preserves the
saved key. Changes apply to new calls, not calls already in progress.

### Conversation languages

New installations start in English. **Default / greeting language** selects the
initial language; **Adapt to the caller** lets recognition and the agent follow
another language during the call, including an explicit request to switch.
**Keep the default language** pins recognition and conversation to that language.
Use a multilingual speech model for automatic switching. Language support and
pronunciation still depend on the selected recognition, speech and chat models.

Silence prompts, transfers and spoken replies follow the conversation. Brief acknowledgements
and names retain the established language. Transfer recipients have a separate
conversation and do not change the caller’s language. **Call summary language**
selects the operator’s language; blank uses the default language. Transcripts retain
the original words.

**Preload fallback languages** accepts comma-separated two-letter codes such as
`en,fr,es`. Save settings and use **Prepare fallback translations** before calls.
The preparation uses the configured Vox model for customized phrases. It stores
translated templates, not caller conversations. Standard English and French
fallbacks work without a model. Configuration edits produce a new cache identity;
old translations cannot replace an edited phrase. Translation failures are shown
as unavailable and do not prevent saving configuration.

Other detected languages can prepare their templates in the background. Until a
translation is ready, available generic phrases are used; an unavailable phrase
is omitted instead of spoken in another language. Preload languages that need
fallback announcements during a model outage. Speech audio is generated on demand
and cached with its voice, model and language settings.

Waiting-clip entries can declare `language` (for example `en` or `fr`) or
`neutral: true` for a language-free vocalization. Legacy worded packs are treated
as French. Clips in another language are excluded; neutral clips remain usable.

**Voice stability** accepts 0–1: lower values allow more vocal variation, higher
values favour consistency. Leave it empty to use the voice's provider setting.
Short replies are synthesized together where possible; streaming waits at most
200 ms after the first complete sentence to group up to 400 characters. Longer
or slower replies start progressively. Completed audio requests are linked within
each call where the provider supports request stitching.

Identical speech is reused from a private audio cache across calls and agent
restarts. The first synthesis still streams immediately. A change to the text,
voice, model, language, stability, pronunciation or provider credential selects
new audio. Interrupted or failed syntheses are not cached. The cache retains at
most 256 MiB and 4,096 clips, removing the least recently used clips first.
It lives in the `tts_cache` Docker volume and can contain personalized speech.
Set `TTS_CACHE_DIR=off` to disable reuse. If you edit a voice directly at the
provider without changing its ID, clear the cache to hear the updated voice:
`docker compose exec agent sh -c 'rm -f /var/cache/prismvox/tts/*.audio'`.
The prerecorded waiting cue remains a separate local audio file. It starts after
three seconds of initial response silence. Once started, it finishes before ready
response audio is played; caller interruption or hangup can still stop it.

To rotate several prerecorded fillers, set `WAIT_CUE_DIR` to a directory inside
the agent containing WAV files and `clips.json`. The picker avoids its three most
recent choices across calls. Clips marked `lookup_only` are eligible only while
a read or search tool is running, in standalone and Prism-connected calls. Unknown
operations and writes use neutral fillers. There is at most one filler per
response, and fast responses do not play one.

```json
[
  {"file": "alors.wav", "text": "Alors…"},
  {"file": "je-regarde.wav", "text": "Je regarde…", "lookup_only": true}
]
```

Use mono WAV audio at 8 kHz, up to two seconds per clip and 32 clips per pack.
Include at least one neutral clip; four or more allow the full repetition window.
Generate the files with your chosen voice and replace them when changing voice.
An invalid pack falls back to `WAIT_CUE_WAV` when configured. Keep the directory
in a persistent volume, for example `/sounds/waiting-cues`.

Audio, text sent for synthesis and transcripts are processed by ElevenLabs.
Provider charges apply. Language-model requests go to the model provider you
configure. The API key is encrypted in PostgreSQL and never returned by read APIs.

### Connect your own STT/TTS services

In **Speech providers**, select **Self-hosted · OpenAI-compatible API** independently
for recognition and synthesis. Set the API root, including `/v1`, and the model
ID. For synthesis, also enter a voice ID if your service requires one. An optional
service Bearer key can authenticate these external APIs; it is stored encrypted
and is separate from your ElevenLabs credential.

PrismVox uses these contracts, without depending on a particular model or engine:

| Operation | Request | Response |
| --- | --- | --- |
| Recognition | `POST /v1/audio/transcriptions`, multipart `file`, optional `model`, `language`, `prompt`, `response_format=json` | JSON with `text` |
| Synthesis | `POST /v1/audio/speech`, JSON `input`, `model`, optional `voice`, `response_format=wav` | Mono WAV audio, resampled for telephony |

You can mix providers, for example external recognition with ElevenLabs synthesis.
Deploy and maintain the speech servers separately. Their URLs must be reachable
from the Docker network: `localhost` inside a container is that container, not
your host. On Linux, `host.docker.internal` maps to the host gateway; the service
must listen on an address reachable from that network.

There is no automatic adapter for arbitrary proprietary APIs. **Legacy HTTP
service** preserves the older contract: recognition posts a multipart `audio`
file to `/transcribe`; synthesis posts JSON `text` to the configured `/synthesize`
URL. Optional `hotwords`, `spell_words` and `spell_exceptions` retain compatibility
with existing servers. Legacy voice management routes depend on that server.
For OpenAI-compatible services, manage voice uploads/deletion on the speech server.

## Configure the receptionist

![PrismVox agent configuration](docs/screenshots/agent.png)

| Console page | What you configure or inspect |
| --- | --- |
| Overview | SIP status, active calls and aggregate call activity |
| Settings → Agents & models | Shared Vox model, separate inbound/outbound instructions and knowledge/MCP switches |
| Settings → Voice & greeting | Greeting, voice, speech providers, spoken phrases and pronunciation |
| Settings → Transfers & directory | Contacts, blind/attended transfer preferences and recipient dialogue limit |
| Settings → Phone connection | Trunk, TLS/UDP transport and transfer method |
| Knowledge Base | Collections and reference documents available for retrieval |
| MCP | Tool servers, connection details and exposed tools |
| Outbound calls | Missions, scheduling, attempts and outcomes |
| Call logs / Conversations | Call history, transcripts and tool activity |

A compatible model must support structured tool calls: saying “I will transfer
you” alone cannot transfer a call. The receptionist must call the transfer,
message or hang-up tool. Keep the persona concise and give it a knowledge base
for company-specific facts. Start with a few test calls before connecting a
production number.

PrismVox uses **one model** for incoming unknown callers, outbound missions,
transfer recipients, summaries and console chats. Configure it in
**Settings → Agents & models → PrismVox model — all calls & chat**. The
optional API URL applies to all these uses; an empty URL uses `OPENAI_BASE_URL`.
Gateway credentials are not forwarded to a different model endpoint.

When upgrading, the existing inbound model and endpoint become the shared Vox
configuration. Previous outbound overrides are no longer used. Inbound and
outbound instructions remain separate.

### Saving settings

In standalone mode, **Save changes** saves only the current form: agents/models,
voice/greeting/phrases, or the transfer dialogue limit. **Discard changes** reloads
that form's saved values. Switching settings tabs preserves unsaved edits; reloading
the page does not. Dictionary edits have their own Save/Cancel controls.

The standalone voice selector saves immediately. Speech providers have their own
**Save voice settings** button; **Connect** only loads available models and voices.
For SIP, **Save** stores the line settings without activating them. **Apply**
saves the settings and reloads the trunk in Asterisk.
Saved call settings apply to new calls.

## Configure the standalone knowledge base

Open **Knowledge Base → Embedding provider — local knowledge base** and enter:

- **API URL**: the OpenAI-compatible API root, including `/v1`, for example
  `http://host.docker.internal:11434/v1` for a reachable Ollama server.
- **Embedding model**: the ID of an embedding model available on that server.
- **API key**: optional; the provider credential if authentication is required.

Use **Test connection** to check access and discover the vector dimensions.
**Save & index documents** validates and saves the provider, then indexes existing
passages in batches with progress. A blank key field preserves the saved key;
changing the URL clears it. Stored keys are encrypted and never returned by read
APIs. The endpoint must be reachable from both the console and the call agent.

Add TXT, Markdown or text-bearing PDF documents to a collection. New uploads are
indexed automatically. Enable knowledge search in **Settings → Agents & models**
so the receptionist can request relevant passages. The console's text agent uses
the same provider and index. Connected phone calls continue to use Prism's own
knowledge sources.

The index records the API URL, model and dimensions. A provider/model change
requires re-indexing, even when dimensions are identical. **Re-index embeddings**
rebuilds every passage in bounded requests and can be rerun after an interruption.
The original document text is retained. If the provider is unavailable, stored
text remains available through the existing bounded document fallback; semantic
search resumes when compatible vectors are ready.

Existing 384-dimensional indexes are migrated without deleting document text.
After upgrading, configure the provider and finish indexing before relying on
semantic search. The former bundled embedding service is no longer deployed;
stop its old container after active calls have ended. No Docker volumes need to
be removed. Previously published `prismvox-embeddings` images are no longer built
or used by the current stack.

Optional bootstrap values are `EMBEDDING_BASE_URL`, `EMBEDDING_MODEL` and
`EMBEDDING_API_KEY`. Saved provider settings take precedence. The retired
`EMBEDDINGS_URL` variable described a different `/embed` protocol; replace it with
the compatible API root and model above.

## Optional Prism integration

Both personal and multi-user Prism deployments are supported. In personal mode,
set the owner’s number in **Settings → Profile → Phone**. Calls from that number
use their personal assistant and existing organizer/mailbox; other callers reach
the standard. No Prism user account or multi-user conversion is required.


Set `PRISM_URL` and `PRISM_TOKEN` in PrismVox. In Prism, set `VOX_URL` to the
PrismVox console address and use `VOX_USER` and `VOX_PASSWORD` for its console
credentials. Both services must be reachable on a shared Docker network; keep
the call agent on a single network for reliable RTP addressing. When using the
standard project name, that network is `prismvox_agentnet`.

- Prism supplies the transfer directory from its user profiles.
- Recognized callers use Prism's voice agent with their profile, scoped
  knowledge, notes, tasks, calendar and connected mailbox. Their account
  permissions apply. Configure accounts and mailbox rules from Settings;
  shell, server administration, secrets, MCP and custom tools stay unavailable
  by phone.
- Unknown callers use PrismVox's switchboard persona and Prism's selected public
  voice knowledge collection. Configure that collection in Prism.
- Prism relays transfers, messages and hang-up requests to PrismVox/Asterisk.
- The standalone console points to Prism when docked; its protected administration
  API remains available to Prism.
- **Agents & models → Conversation model — recognised callers**, under **Admin → Telephony** in shared mode or **Settings → Telephony** in personal mode,
  selects the model for recognized callers. An empty selection follows the Prism
  deployment default; the web default is unchanged. Unknown callers and outbound
  missions share the **PrismVox model — unknown callers & outbound calls** in
  the same section. Its endpoint is independent of the Prism model catalogue.

Prism’s telephony configuration groups settings into **Agents & models**,
**Voice & greeting**, **Transfers & directories** and **Phone connection**.
Use the Save button beside the block you edited. Voice and greetings share
**Save voice & greeting**; the public knowledge collection selector saves
immediately. **Save & apply** saves and reloads the SIP trunk.

`PRISM_PUBLIC_URL` sets the browser-facing Prism URL when `PRISM_URL` is internal.

## Conversation memory and call archives

Each phone call starts a separate conversation. During a long standalone call,
PrismVox summarizes older exchanges in the background and keeps recent messages
verbatim. The next turns use that summary once it is ready. If summarization
fails, the original messages remain and PrismVox retries later. Calls answered
by Prism use Prism's own context management.

The full call transcript is kept separately and saved to PostgreSQL when the
call ends. The final summary uses both sides of that transcript and the recorded
action outcomes. An initiated blind transfer does not confirm that the recipient
answered; a recorded message does not imply delivery. A process crash before
the final save can lose the in-progress transcript.

Archived calls are available for supervision; they are not automatically loaded
into future calls. Recognized callers connected to Prism can reuse their
personal profile and relevant saved lessons, with a separate conversation per
call. National and international caller-number formats match by their trailing
significant digits; multiple matching approved profiles leave the caller
unidentified. Presented caller ID is not strong authentication.

**Supervision** consults the complete authorized archive on demand. Ask about a
period, a caller or outbound activity without selecting a call window first.
The agent uses one paginated archive-reading tool; results include the exact
matching total. Transcripts and tool results are fetched by call ID. Broad content
analyses may require several reads; incomplete coverage must be stated.

Supervision conversations and tool traces persist across process restarts. A
bounded recent window and a retained handoff keep model context small. **Agent
context → Lighten context** preserves the full conversation archive. **Calls**
provides manual number, period, direction and text filters.

Connected Prism exposes a **Telephony** app. A global administrator selects
existing groups under **Admin → Telephony → App access**. Each authorized group has its own supervision
conversation and sees the standard’s calls. Recognized callers’ private calls
remain owner-scoped. Outbound tasks retain their initiating owner or standard
scope. Installation IDs prevent reusing another Prism installation’s user IDs.
Older records without audience metadata remain in the private **Administrator
archive**; no ownership is guessed from historical phone numbers. In personal
mode the owner sees standard calls and their own recognized calls.

In the standalone console, **Test the agent** keeps its text transcript in
**Conversations**. **New session** starts fresh model context and preserves the
archive. **Delete** in Conversations removes that text-session archive. Text
context uses a recent-message window; it does not use telephone compaction.
The OpenAI-compatible API
is stateless: the client supplies conversation history on each request.

Standalone PrismVox uses shared administration/API credentials, not separate
user accounts. Integrations must assign distinct session IDs to distinct
conversations; the `contact` label does not isolate access.

## Configuration and persistent data

`.env.example` lists bootstrap variables. SIP, persona, phrases, dictionary,
model tiers and speech settings can subsequently be changed in the console.
Saved database values take precedence over their bootstrap values.

| Variables | Purpose |
| --- | --- |
| `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE` | Deployment identity and optional model overlay |
| `ARI_USER`, `ARI_PASS` | Internal Asterisk API credentials |
| `DB_PASS` | PostgreSQL password |
| `UI_USER`, `UI_PASSWORD`, `UI_BIND`, `UI_TLS_SAN` | Console authentication and publication |
| `AGENT_API_KEY` | Bearer authentication for text-agent/API clients |
| `SIP_*` | Initial trunk configuration and TLS verification |
| `OPENAI_BASE_URL`, `OPENAI_MODEL`, `OPENAI_API_KEY` | Language-model gateway |
| `WAIT_CUE_WAV` | Optional prerecorded WAV path inside the agent container; played once after three seconds of initial response silence |
| `WAIT_CUE_DIR` | Optional directory containing `clips.json` and prerecorded fillers; rotates clips and avoids recent repeats |
| `STT_PROVIDER`, `TTS_PROVIDER` | `elevenlabs`, `openai` or legacy `service` |
| `ELEVENLABS_*` | ElevenLabs credentials, voice, models, language and stability |
| `STT_URL`, `TTS_URL`, `STT_MODEL`, `TTS_MODEL`, `TTS_VOICE` | External speech APIs |
| `SPEECH_SERVICE_API_KEY` | Optional Bearer credential for external speech APIs |
| `TTS_CACHE_DIR` | Persistent audio cache directory; `off` disables reuse |
| `PRISM_URL`, `PRISM_TOKEN`, `PRISM_PUBLIC_URL` | Optional Prism connection |
| `WHISPER_HOTWORDS`, `TTS_SPELL_*` | Dictionary bootstrap, independent of the speech engine |
| `EMBEDDING_BASE_URL`, `EMBEDDING_MODEL`, `EMBEDDING_API_KEY` | Optional standalone embedding API bootstrap; saved Knowledge Base settings override these values |
| `RAG_TOP_K`, `BARGEIN_MS` | Knowledge retrieval and interruption timing |
| `LOG_SENSITIVE_DATA`, `DEBUG` | Optional content and SIP diagnostics |

Keep `pgdata`, `secrets` and `asterisk_dynamic` together when moving a deployment.
Stored credentials use AES-256-GCM; the encryption key lives in the persistent
`secrets` volume, outside PostgreSQL. Losing it makes those credentials unreadable.
Console TLS certificates live in `ui_certs`; generated media uses `sounds`.
Reusable synthesized speech lives in the private, bounded `tts_cache` volume.
Do not run `docker compose down -v` to update an installation.

### Optional local language model

`docker-compose.llm-local.yml` adds a GPU-backed llama.cpp endpoint; this is a
language model, not a speech engine. Its default recipe uses Qwen3.5-9B Q8_0 in
non-thinking mode. Download the GGUF referenced in that file to
`.dev/models/qwen3.5-9b`, append the overlay to `COMPOSE_FILE`, then select
`http://fast-llm:8080/v1` / `qwen3.5-9b` in the model configuration.
Allow about 12 GB of free GPU memory. The diagnostic API binds to localhost on
port 18120. `FAST_LLM_MODEL_DIR`, `FAST_LLM_MODEL_FILE`, `FAST_LLM_MODEL_NAME` and
`FAST_LLM_PORT` override the recipe.

## Upgrading from PrismConnect / older PrismVox installations

Keep the existing `COMPOSE_PROJECT_NAME`, database, encryption-key volume and SIP
configuration. Update `COMPOSE_FILE` to `docker-compose.yml` (plus the optional
language-model overlay). The old `docker-compose.local.yml` and
`docker-compose.elevenlabs.yml` speech services have been removed.
`docker-compose.direct-voice.yml` remains a compatibility overlay.

Before updating, select direct ElevenLabs in Speech providers, or point the
external service configuration at your separately deployed STT/TTS APIs.
Do not remove old speech containers or volumes until you have verified calls
with the replacement configuration. New installations default to ElevenLabs;
explicit saved provider choices remain unchanged.

## Security

Use distinct strong credentials and a trusted HTTPS reverse proxy for remote
administration. An empty console password or agent API key leaves that surface
open; keep it on a trusted network only. Asterisk's ARI and PostgreSQL are not
published on host ports. Restrict SIP/media traffic to your carrier or PBX.
TLS trunk certificate verification is enabled for new installations; configure
your carrier's CA when needed. UDP does not encrypt SIP or media.

Routine logs omit conversation content. `LOG_SENSITIVE_DATA=true` and
`DEBUG=true` can expose transcripts or SIP details; disable them after debugging.
Functional transcripts and tool history remain in PostgreSQL. Protect and back up
the database, persistent encryption key and trunk configuration. AES encryption
does not protect against an administrator controlling the running host.

## Development checks

```sh
(cd agent && go test -race -p 1 ./... && go vet ./...)
python -m unittest discover -s ui/tests -v
node ui/tests/voice-browser.cjs
node ui/tests/dom-regressions.cjs
```

Install the UI requirements plus `httpx` for Python tests, and Playwright/Chromium
or jsdom for browser/DOM tests. Set `VOX_TEST_DATABASE_URL` to a disposable
PostgreSQL/pgvector database for integration tests. The tests use isolated schemas
and synthetic APIs; they do not make telephone calls or paid model requests.

## License

[MIT](LICENSE).
