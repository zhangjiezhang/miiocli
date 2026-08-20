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
      devices:
        szp-001:
          token: replace-with-a-device-secret
      tts:
        appKey: your-aliyun-nls-app-key
        accessKeyId: your-aliyun-access-key-id
        accessKeySecret: your-aliyun-access-key-secret
        voice: xiaoyun
      codex:
        enabled: true
        callbackBaseUrl: http://192.168.1.5:9200
        registerPath: /v1/codex/register
        eventPath: /v1/codex/events
        token: replace-with-a-long-random-secret
        sessionTtlSeconds: 90
        timeoutSeconds: 600
    ota:
      directory: ./releases
      adminToken: replace-with-an-ota-admin-secret
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
- `/websocket` - online device selection and voice message delivery through the Speak API.
- `/codex` - Codex session status, device bindings, event history, and direct session messaging.

**Voice WebSocket**
--
The ESP32 connects to `ws://<server>:8080/v1/device/ws` and first sends a `hello` JSON message with its `device_id` and token. The server keeps one live connection per configured device.

Audio producers may call `VoiceHub.StartAudio` and `VoiceHub.SendOpus` for raw 16 kHz mono Opus, or `VoiceHub.StartPCM` and `VoiceHub.SendPCM` for signed 16-bit little-endian PCM at 16 kHz mono. Binary WebSocket messages must not exceed 1024 bytes.

For push-to-talk commands, the device sends `listen_start`, streams 16 kHz mono signed 16-bit PCM as binary messages, then sends `listen_end`. The server uses Alibaba Cloud NLS speech recognition, routes the text to the Codex session currently bound to that ESP32, and returns `command_state` events (`transcribing`, `thinking`, `working`, `done`, or `error`). The final Codex summary is spoken through the existing TTS stream. A short BOOT press cycles through online Codex sessions; a long press records the command.

Set `voice.logInput: true` to log ESP32 utterance IDs, audio size and duration, and recognized text. It defaults to `false`; raw PCM data and authentication tokens are never written to this log. Recognized text may contain sensitive information, so enable it only when needed for diagnostics.

Codex clients keep their normal CLI, SDK/IDEA, or App interaction. A plugin or client adapter registers each existing session with miiocli and exposes an HTTP message endpoint. Messages from the original Codex UI, the `/codex` page, and ESP32 can therefore coexist in the same session. Set a high-entropy connector token in `voice.codex.token` and configure the plugin/adapter with the same value. Because `app.yaml` contains connector, device, API, OTA, and Alibaba Cloud credentials, restrict it to the service account, for example with `chmod 600 app.yaml`.

Connector endpoints:

- `POST /v1/codex/register` - register or heartbeat a session with its `session_id`, `thread_id`, current `turn_id`, client surface, state, capabilities, and message endpoint.
- `POST /v1/codex/events` - report accepted, working, done, or error events for an external message.
- `GET /v1/codex/sessions` - list registered sessions for the web console. Uses `voice.apiToken`.
- `POST /v1/codex/sessions/{session_id}/messages` - send a web-console message to an existing session.
- `POST /v1/codex/sessions/{session_id}/bind` - bind an ESP32 device to a session.

Registration example:

```json
{
  "session_id": "session-123",
  "thread_id": "thread-123",
  "turn_id": "turn-456",
  "surface": "cli",
  "title": "Firmware development",
  "project": "lichuang-esp32s3",
  "branch": "master",
  "state": "running",
  "endpoint": "http://codex-host:9210/v1/sessions/session-123/messages",
  "capabilities": ["turn_start", "turn_steer"]
}
```

The adapter decides how to inject a message: use `turn/steer` for a steerable active turn, start a new turn on the same thread when idle, or queue until the client can accept input. A Codex plugin can package the hooks, skill, and adapter, but the plugin must still use the control API provided by its host client. Hooks alone cannot inject an unsolicited user message into an active model turn.

**Aliyun TTS API**
--
Send a protected request to start a stream on an online device. The server uses Alibaba Cloud NLS streaming synthesis with native 16 kHz PCM output and forwards it to the device as `pcm_s16le` frames.

```bash
curl -X POST http://<server>:8080/v1/devices/szp-001/speak \
  -H "Authorization: Bearer replace-with-an-api-secret" \
  -H "Content-Type: application/json" \
  -d '{"text":"服务器温度过高，请及时处理。","voice":"xiaoyun"}'
```

The response returns `202 Accepted` and a `stream_id`. Configure either an AccessKey pair for automatic NLS token renewal or `voice.tts.token` for a short-lived token.

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
Open `http://<server>:8080/ota` to upload and manage ESP32 firmware releases. The admin page and release APIs use `ota.adminToken`; ESP32 check and download requests use the device ID and token configured under `voice.devices`.

Device endpoints:

- `GET /v1/ota/check` with `Authorization: Bearer <device-token>`, `X-Device-ID`, `X-Firmware-Version`, and `X-OTA-Channel` headers. Returns `204` when no newer release exists.
- `GET /v1/ota/firmware/<release-id>.bin` with the same device credentials.

Admin endpoints:

- `GET /v1/ota/releases`
- `POST /v1/ota/releases` as multipart form data with `firmware`, `version`, `channel`, `mandatory`, and `notes`.
- `DELETE /v1/ota/releases/<release-id>`

The uploaded version must match the ESP-IDF application version embedded in the `.bin`. Set the firmware version in the project root `version.txt` before building and uploading.

Before the first OTA release, flash the ESP32 once over USB with the new partition table, bootloader, OTA data, and factory application. Later releases only require uploading `build/lvgl.bin` on the OTA page. Changing from the old single factory partition to A/B slots cannot be done safely by an application-only OTA.
