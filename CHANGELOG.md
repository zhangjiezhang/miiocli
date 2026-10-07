# Release notes

## 1.0.6 — 2026-10-07

- Show VAD and VP alongside ASR, Nanobot and TTS in the voice-record list's stage
  timings column, matching the stages already available in record details.
- Add VAD/VP average timing cards and API statistics over all matching records.
  Include completed unknown-speaker VP results; exclude unexecuted, processing
  and interrupted stages. Display missing legacy stages as a dash and preserve
  valid zero-millisecond timings.
- Serve the record page with `Cache-Control: no-store` to prevent stale UI caching.

Validation: Go tests and race checks, `go vet`, Linux amd64/arm64 builds, and
browser verification of the updated list and average cards using a local preview
with records read from the deployed service. Added a regression test covering
filtering, pagination, unknown-speaker results and completed-stage averages.

## 1.0.5 — 2026-10-07

- Remove the local ASR token requirement. Both `local` and the local leg of `auto`
  work without `localToken` or `MIIOCLI_LOCAL_ASR_TOKEN`; tokenless requests omit
  the Authorization header entirely. Optional Bearer credentials remain supported
  for compatibility with authenticated deployments.
- Remove the token from the example configuration and document tokenless use.

Validation: Go tests and race checks, `go vet`, Linux amd64/arm64 builds, and live
tokenless transcription at `http://192.168.1.8:18080/v1/transcribe`. The generated
test audio was recognized as “你好，请打开客厅的灯。”.

## 1.0.4 — 2026-10-07

- Add independently configurable local/Alibaba Cloud TTS. Local Matcha TTS defaults
  to `http://192.168.1.8:18081/tts/stream`, resamples streamed PCM to the ESP32's
  16 kHz playback format, carries incomplete HTTP samples, and splits long replies
  into requests of at most 500 characters. Existing configurations retain Alibaba
  Cloud behavior and the Speak API remains compatible.
- Add optional local energy VAD before transcription: filter silence and brief
  clicks, trim the edges with context padding, and preserve pauses within speech.
  This operates on completed push-to-talk captures; it does not implement Silero
  inference, automatic recording endpointing, or hands-free listening.
- Add optional 3DSpeaker-compatible voiceprint identification against
  `http://192.168.1.62:8005/voiceprint/identify`, using authenticated multipart WAV
  uploads and registered candidate speaker IDs. Run ASR and VP concurrently.
  Configure similarity thresholds, timeouts, and whether a match is required.
  Unknown speakers/service errors allow conversation by default.
- Pass speaker metadata to Nanobot and show VAD, VP, speaker results and timings
  in searchable voice records. Do not persist raw audio or service credentials.
- Include the new sources in the Go Docker build and document configuration.

Validation: Go tests and race checks, `go vet`, Linux amd64/arm64 builds, and a
live local TTS test using a simulated device connection. The deployed VP OpenAPI
was checked; multipart identification, thresholds, parallel execution, and failure
policies were verified with mock services. Live VP identification requires the
deployment's private API key and registered speaker IDs and was not performed.
