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

    trafficAddress: http://127.0.0.1:9000/static
    voice:
      path: /v1/device/ws
      apiToken: replace-with-an-api-secret
      devices:
        szp-001:
          token: replace-with-a-device-secret
      tts:
        appKey: your-aliyun-nls-app-key
        accessKeyId: your-aliyun-access-key-id
        accessKeySecret: your-aliyun-access-key-secret
        voice: xiaoyun
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

**Voice WebSocket**
--
The ESP32 connects to `ws://<server>:8080/v1/device/ws` and first sends a `hello` JSON message with its `device_id` and token. The server keeps one live connection per configured device.

Audio producers may call `VoiceHub.StartAudio` and `VoiceHub.SendOpus` for raw 16 kHz mono Opus, or `VoiceHub.StartPCM` and `VoiceHub.SendPCM` for signed 16-bit little-endian PCM at 16 kHz mono. Binary WebSocket messages must not exceed 1024 bytes.

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
