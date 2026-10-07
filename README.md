# miiocli


[![CICD](https://github.com/clickbg/miiocli/workflows/CICD/badge.svg?branch=main)](https://github.com/clickbg/miiocli/actions/workflows/cicd.yaml)
[![UPDATE](https://github.com/clickbg/miiocli/workflows/UPDATE/badge.svg?branch=main)](https://github.com/clickbg/miiocli/actions/workflows/update.yaml)
[![PUBLISH](https://github.com/clickbg/miiocli/workflows/PUBLISH/badge.svg)](https://github.com/clickbg/miiocli/actions/workflows/publish.yaml)

<img src="https://www.docker.com/wp-content/uploads/2022/03/vertical-logo-monochromatic.png" width="20" height="20"> [Avaliable on DockerHub](https://hub.docker.com/r/clickbg/miiocli)

Docker images containg miiocli from https://github.com/rytilahti/python-miio  
The container is being automatically upgraded for new Python versions which will also update miiocli.  
Depending on interest I might start doing numbered releases, if you have a need for specific version of miiocli please let me know.  
For the time being I am releasing latest only since a version build takes up to 2h.  

**Usage**
--
Command Line:

    docker run --rm --name miiocli clickbg/miiocli:latest miiocli --help

Config example:

    kiwi:
      veid: your-veid
      apiKey: your-api-key
      intervalSeconds: 30
    trafficAddress: http://127.0.0.1:9000/static
    voice:
      path: /v1/device/ws
      apiToken: replace-with-an-api-secret
      logInput: false
      records:
        directory: ./voice-records
        retentionDays: 5
      asr:
        provider: auto
        localUrl: http://192.168.1.8:18080
        timeoutSeconds: 15
      vad:
        enabled: true
        provider: energy
        thresholdDb: -40
        minSpeechMs: 120
        paddingMs: 200
      vp:
        enabled: false # Enable after registering speakers and supplying the service key.
        baseUrl: http://192.168.1.62:8005
        # apiKey may be omitted when MIIOCLI_VP_API_KEY is set.
        similarityThreshold: 0.4
        timeoutSeconds: 10
        requireMatch: false
        speakers:
          - id: speaker-001
            name: 张三
            description: 家庭成员
      devices:
        szp-001:
          token: replace-with-a-device-secret
      tts:
        provider: aliyun # local or aliyun (default)
        localUrl: http://192.168.1.8:18081
        localSpeed: 1.0
        timeoutSeconds: 90
        appKey: your-aliyun-nls-app-key
        accessKeyId: your-aliyun-access-key-id
        accessKeySecret: your-aliyun-access-key-secret
        voice: xiaoyun
      nanobot:
        enabled: true
        baseUrl: https://nanobot.local.pascall.cn
        apiKey: replace-with-a-nanobot-api-key
        model: nanobot
        timeoutSeconds: 600
    ota:
      directory: ./releases
    webApp:
      directory: ./web-releases
      publishDirectory: ./ipad-show
    mis:
      - name: plug-living-room
        alias: 客厅插座
        sort: 10
        ip: 192.168.1.10
        token: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
        drive: cuco
        hostName: esxi-01
        asyn: true

KiwiVM service credentials are read from `kiwi.veid` and `kiwi.apiKey` in the
YAML configuration file. When both values are present, the Go service fetches `getServiceInfo` at startup
and then every `kiwi.intervalSeconds` seconds (30 seconds by default). The latest successful response and its derived quota,
traffic, and reset-time fields are exposed as `data.kiwi` by `/static` and shown
on the web console. A transient API failure is reported in the status fields
without discarding the last successful data.

**Web console**
--
Open `http://<server>:8080/` for the service console. The shared navigation provides these pages:

- `/` - feature overview, device metrics, endpoint reference, and usage examples.
- `/ota` - ESP32 firmware release management.
- `/webapp` - iPad PWA build upload, activation, and rollback management.
- `/websocket` - online device selection and voice message delivery through the Speak API.
- `/voice-records` - searchable ASR, Nanobot and TTS history, timings and retention settings.

**Voice WebSocket**
--
The ESP32 connects to `ws://<server>:8080/v1/device/ws` and first sends a `hello` JSON message with its `device_id` and token. The server keeps one live connection per configured device.

Audio producers may call `VoiceHub.StartAudio` and `VoiceHub.SendOpus` for raw 16 kHz mono Opus, or `VoiceHub.StartPCM` and `VoiceHub.SendPCM` for signed 16-bit little-endian PCM at 16 kHz mono. Binary WebSocket messages must not exceed 1024 bytes.

For push-to-talk commands, the device sends `listen_start`, streams 16 kHz mono signed 16-bit PCM as binary messages, then sends `listen_end`. The server uses the configured ASR provider, sends the text to Nanobot through its OpenAI-compatible `/v1/chat/completions` endpoint, and returns `command_state` events (`transcribing`, `thinking`, `working`, `done`, or `error`). The final Nanobot response is spoken through the existing TTS stream. Legacy short BOOT press session events are ignored; a long press records the command.

`voice.asr.provider` supports `aliyun` (default when omitted), `local` (local service only),
and `auto` (local first, Alibaba Cloud NLS fallback). Local ASR sends the original PCM to
`voice.asr.localUrl` + `/v1/transcribe`, with `Content-Type: audio/pcm`.
The deployed local service no longer requires a token: omit `localToken` and unset
`MIIOCLI_LOCAL_ASR_TOKEN` to send requests without an Authorization header.
For compatibility with authenticated deployments, an explicitly configured `localToken`
or `MIIOCLI_LOCAL_ASR_TOKEN` still sends Bearer authentication; neither is required
for `local` or the local leg of `auto`.
YAML values are literal: `${VARIABLE}` interpolation is not supported.
`timeoutSeconds` limits the local HTTP request, defaults to 15, and accepts 1–120.
Local requests bypass HTTP proxy environment variables and do not follow redirects.

In `auto` mode, connection errors, timeouts, non-200 responses (including busy/429), malformed
or oversized JSON, and empty results trigger cloud fallback. Caller cancellation does not.
The deployed RK3588 service accepts audio up to 20 seconds; longer recordings fall back in
`auto` mode and fail in `local` mode. Aliyun ASR credentials are required for `aliyun` and `auto`.
TTS is configured independently through `voice.tts.provider`. No ESP32 firmware change is needed.

Set `voice.logInput: true` to log ESP32 utterance IDs, audio size and duration, and recognized text. It defaults to `false`; raw PCM data and authentication tokens are never written to this log. Recognized text may contain sensitive information, so enable it only when needed for diagnostics.

Nanobot authentication uses `voice.nanobot.apiKey`. Each device is sent as a stable OpenAI `user` value, so Nanobot maintains an independent conversation for every ESP32. `model` is optional and may be omitted if the server is configured to accept its default model. Because `app.yaml` contains device, API, OTA, Alibaba Cloud, and Nanobot credentials, restrict it to the service account, for example with `chmod 600 app.yaml`.

**Voice call records**
--
Recording is enabled automatically. Each interaction is persisted atomically as a private
JSON file in `voice.records.directory` (default `./voice-records`), including ASR provider,
recognized text, complete Nanobot reply, actual spoken text, per-stage status/errors and
elapsed milliseconds. Audio is recorded as format, byte count and duration metadata;
raw recordings are not retained. TTS records also include time to first audio and bytes sent.
Direct Speak API calls produce TTS-only records. The full Nanobot response is preserved even
when device speech is limited to 600 characters.

TTS duration measures synthesis and delivery to the device connection, not the completion
of audible playback. The record remains processing until asynchronous TTS completes.
Incomplete operations are marked interrupted after a service restart. Average stage timings
include completed successful/failed stages only and use all records matching the query.
The list and average cards cover VAD, ASR, VP, Nanobot and TTS; unknown-speaker VP
results count as completed stages. Unexecuted legacy stages display as a dash,
while valid zero-millisecond timings display as `0 ms`.

Keep the directory on persistent storage. The published image runs in `/app`, so the default
directory is `/app/voice-records` inside the existing `/app` volume. Records default to 5 days;
`retentionDays` accepts 1–365. The management page can change this setting; it persists in
`settings.json` and takes precedence over YAML on subsequent starts. Expired records are
removed at startup, hourly, on queries and immediately after a retention change.

Management pages load automatically without administrator/API token inputs. The console uses
`/console/v1/...` routes without caller authentication. OTA/Web App need no token configuration;
voice-only server credentials are never embedded in HTML or browser storage.
Console routes allow only management endpoints and reject cross-origin browser requests;
mutations require same-origin browser metadata. Access to the console grants management access.
The original `/v1/...` external and device APIs retain Bearer authentication.
Record API endpoints for external callers (Bearer authentication):

- `GET /v1/voice/records`: filters `device`, `status`, `q` (input/output keyword), `start` and
  `end` (RFC3339), with `page` and `limit` (default 20, maximum 100); newest records first.
  Returns compact text previews, matching counts and mean stage timings.
- `GET /v1/voice/records/<id>`: complete inputs, outputs and timing details.
- `GET /v1/voice/records/settings`: current retention setting.
- `PUT /v1/voice/records/settings`: JSON `{"retention_days":5}`; shortening retention
  permanently removes expired records.

Console and board routes are separate:

| Purpose | Path | Authentication |
| --- | --- | --- |
| Firmware management | `/console/v1/ota/releases` and subpaths | No administrator token required from the caller |
| Web App management | `/console/v1/web/releases`, `/console/v1/web/current` | No administrator token required from the caller |
| Device list and speech delivery | `/console/v1/devices`, `/console/v1/devices/<id>/speak` | No API token required from the caller |
| Conversation records and retention | `/console/v1/voice/records` and subpaths | No API token required from the caller |
| Board WebSocket | `/v1/device/ws` | Device ID/token in hello |
| Board firmware check | `/v1/ota/check` | Device Bearer token and `X-Device-ID` |
| Board firmware download | `/v1/ota/firmware/<id>.bin` | Device Bearer token and `X-Device-ID` |

The console router does not expose board authentication/check/download paths.
Scripts use the same `/console/v1/...` management paths. OTA and Web App management are not registered under `/v1/...`; voice integration APIs remain authenticated there.

**Local and Alibaba Cloud TTS**

`voice.tts.provider` accepts `local` or `aliyun`; omission preserves Alibaba Cloud behavior.
Use `local` with `localUrl: http://192.168.1.8:18081` to call the sibling `tts/` project's
`POST /tts/stream` interface. The client reads raw PCM with `X-Sample-Rate`, keeps incomplete
samples across HTTP chunks, and streams resampled 16 kHz PCM to the device. Text longer than
500 characters is split into ordered requests within one device playback stream. Local
`localSpeed` defaults to 1.0 (0.7–1.5); `timeoutSeconds` defaults to 90 (1–300), covering the
whole synthesis including long-text requests. Local failures cancel playback and are recorded;
there is no implicit cloud fallback. Local TTS requires no Alibaba Cloud credentials. If ASR
is still `aliyun`/`auto`, its NLS credentials remain required in `voice.tts`.

Local Matcha has one fixed voice, so the Speak API's `voice` field only affects Alibaba Cloud.
The Speak API and device playback protocol are the same for both providers.

**VAD and voiceprint flow**

The flow follows [xiaozhi-esp32-server](https://github.com/xinnan-tech/xiaozhi-esp32-server):
`PCM → VAD → (ASR ∥ VP) → Nanobot → TTS`. Both VAD and VP are opt-in, preserving old configurations.
VAD runs when the existing push-to-talk `listen_end` arrives; it does not stop recording early
or introduce hands-free listening. The local `energy` detector measures DC-corrected RMS in
20 ms frames, rejects silence and clicks shorter than `minSpeechMs`, and trims leading/trailing
silence while preserving internal pauses and `paddingMs` of context. `thresholdDb` is dBFS
(default -40, range -90 to -5). `minSpeechMs` defaults to 120 (20–2000), `paddingMs` defaults
to 200 (positive values up to 2000; omitted/zero uses the default). This is an energy VAD,
not XiaoZhi's Silero neural model: sustained noise/music can pass the gate. Tune it using your
microphone's noise floor. Silence produces an `ignored` record and skips ASR, VP, Nanobot and TTS.

VP uses the deployed 3DSpeaker-compatible service at `http://192.168.1.62:8005` (configurable).
Set `voice.vp.enabled: true`, configure `speakers` with IDs already registered at the service,
and supply `apiKey` or environment variable `MIIOCLI_VP_API_KEY`. IDs must be unique and contain
no commas. The client wraps the filtered PCM in a 16 kHz mono PCM16 WAV and sends multipart
`file` and comma-separated `speaker_ids` to `POST /voiceprint/identify` with Bearer authentication,
matching the deployed OpenAPI and XiaoZhi's voiceprint client. Registration is managed by that
service's `POST /voiceprint/register` endpoint (`speaker_id`, WAV `file`, same Bearer key).
No samples are registered or deleted automatically.

A configured candidate above `similarityThreshold` (default 0.4) becomes a matched speaker;
low scores or unexpected IDs become unknown speakers. VP errors/timeouts are recorded and
allow the conversation to continue by default, as in XiaoZhi. `requireMatch: true` stops before
Nanobot/TTS for unknown speakers or service errors; voiceprints are advisory identification,
not a substitute for device/API authentication. ASR and VP run concurrently with separate
stage timings. Nanobot receives speaker ID, name, description, score and match status as
structured reference metadata in a system message; the recognized user text and per-device
conversation ID remain intact. The record page displays VAD/VP results and speaker details.
Audio and service keys are never persisted in records.

**Speak API**
--
Send a protected request to start a stream on an online device. The server uses Alibaba Cloud NLS streaming synthesis with native 16 kHz PCM output and forwards it to the device as `pcm_s16le` frames.

```bash
curl -X POST http://<server>:8080/v1/devices/szp-001/speak \
  -H "Authorization: Bearer replace-with-an-api-secret" \
  -H "Content-Type: application/json" \
  -d '{"text":"服务器温度过高，请及时处理。","voice":"xiaoyun"}'
```

The response returns `202 Accepted` and a `stream_id`. For Alibaba Cloud, configure either an AccessKey pair for automatic NLS token renewal or `voice.tts.token` for a short-lived token.

Use `GET /v1/devices` with the same Bearer API token to list devices that currently have an active WebSocket session.

**Official references**
---
- [Alibaba Cloud Intelligent Speech Interaction console](https://nls-portal.console.aliyun.com/): create an NLS project, obtain its `appKey`, and view the voices available to the current account.
- [Alibaba Cloud speech synthesis overview](https://help.aliyun.com/zh/isi/developer-reference/overview-of-speech-synthesis?spm=5176.11801677.console-base_help.dexternal.477725afI2ojga#topic-2572243): official speech synthesis service documentation.
- [Alibaba Cloud NLS Go SDK](https://github.com/aliyun/alibabacloud-nls-go-sdk): the official SDK used by this service.
- [NLS Go SDK streaming TTS documentation](https://github.com/aliyun/alibabacloud-nls-go-sdk/blob/master/docs/TTS.md): synthesis parameters and callback behavior.

The authoritative voice list is the list shown in the NLS console after login; it varies by account, region, and enabled service entitlements.

**OTA service**
--
Open `http://<server>:8080/ota` to upload and manage ESP32 firmware releases. OTA management does not require an administrator token. ESP32 check and download requests use the device ID and token configured under `voice.devices`.

Device endpoints:

- `GET /v1/ota/check` with `Authorization: Bearer <device-token>`, `X-Device-ID`, and `X-Firmware-Version` headers. Returns the marked pending-update release when its version differs from the device, or `204` when the device already runs it. `X-OTA-Channel` remains accepted for compatibility, while the administrator's unique pending-update selection controls the target release.
- `GET /v1/ota/firmware/<release-id>.bin` with the same device credentials.

Admin endpoints:

- `GET /console/v1/ota/releases`
- `POST /console/v1/ota/releases` as multipart form data with `firmware`, `version`, `channel`, `mandatory`, and `notes`.
- `PUT /console/v1/ota/releases/<release-id>` marks that release as the sole pending-update version.
- `DELETE /console/v1/ota/releases/<release-id>`

The uploaded version must match the ESP-IDF application version embedded in the `.bin`. Set the firmware version in the project root `version.txt` before building and uploading. A new upload is marked pending update automatically and clears the marker from every other release. `/static` exposes it as `data.firmware_version` so devices can trigger an OTA check whenever their local version differs.
The service retains the three most recently uploaded releases. Older release records and firmware files are removed automatically after an upload and when the service starts.

Before the first OTA release, flash the ESP32 once over USB with the new partition table, bootloader, OTA data, and factory application. Later releases only require uploading `build/lvgl.bin` on the OTA page. Changing from the old single factory partition to A/B slots cannot be done safely by an application-only OTA.

**Web App release service**
--
Open `http://<server>:8080/webapp`. The service initializes without an administrator token, using default storage paths when the `webApp` configuration is omitted.

Configuration:

- `webApp.directory` stores uploaded ZIP files and `index.json`. Keep this directory on persistent storage.
- `webApp.publishDirectory` is the directory replaced when a version is activated. It must be separate from `webApp.directory`.

Build the PWA, then compress the **contents** of `dist` so that `index.html` is at the ZIP root:

```bash
npm run build
cd dist
zip -r ../ipad-show-1.0.0.zip .
```

The ZIP root must contain `index.html`, `version.json`, `manifest.webmanifest`, and `sw.js`. The version in `version.json` must match the uploaded page version. Archives are limited to 32 MiB compressed, 128 MiB extracted, and 2048 entries; absolute paths, parent traversal, duplicate paths, symbolic links, and special files are rejected.

The release service uses two API resource paths:

- `GET /console/v1/web/releases` lists uploaded builds.
- `POST /console/v1/web/releases` uploads multipart fields `artifact`, `version`, `control_version`, and optional `notes`.
- `DELETE /console/v1/web/releases/<release-id>` deletes a non-active build and its ZIP. The the current release cannot be deleted.
- `GET /console/v1/web/current` publicly returns the active page version, positive integer control version, activation time, and `/ipad-show/` URL. It returns `{"current":null}` before the first activation.
- `PUT /console/v1/web/current` activates `{"release_id":"<id>"}`.

Upload example:

```bash
curl -X POST http://<server>:8080/console/v1/web/releases \
  -H "Origin: http://<server>:8080" \
  -F "artifact=@ipad-show-1.0.0.zip;type=application/zip" \
  -F "version=1.0.0" \
  -F "control_version=1" \
  -F "notes=Initial iPad dashboard release"
```

Activation example:

```bash
curl -X PUT http://<server>:8080/console/v1/web/current \
  -H "Origin: http://<server>:8080" \
  -H "Content-Type: application/json" \
  -d '{"release_id":"<id>"}'
```

Upload does not alter the active page. Activation validates the stored ZIP again, extracts it to a temporary sibling directory, replaces `webApp.publishDirectory`, and persists the active metadata. A failed validation, extraction, replacement, or metadata write keeps the previous page active. Any uploaded version can be activated again for rollback. The active PWA is served from `/ipad-show/`; `version.json` is never cached, the service worker and HTML use revalidation, and hashed files under `assets/` are immutable.

The service retains at most five Web App releases. After each upload and when the service starts, it keeps the current release plus the newest other releases and automatically removes excess records, ZIP artifacts, and orphan ZIP files.
