# Release notes

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
