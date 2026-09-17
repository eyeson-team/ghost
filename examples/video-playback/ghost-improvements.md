# ghost: suggested improvements

Findings from extending `examples/video-playback` to support VP8, VP9, AV1,
H264 and Opus passthrough. Everything below is in the ghost library or its
dependencies rather than in the example, so each item affects RTMP and RTSP
sources too.

Ordered by impact. Items 1 and 2 are the ones worth doing first.

---

## 1. RTCP from the sending tracks is never read

**Where:** `client.go`, `initStack`

```go
_, videoTrackErr = peerConnection.AddTrack(videoTrack)
_, audioTrackErr = peerConnection.AddTrack(audioTrack)
```

The `*webrtc.RTPSender` that `AddTrack` returns is discarded.

**Why it matters:** in pion, incoming RTCP is only pulled through the
interceptor chain when something calls `Read` on the sender. Nothing does, so
every RTCP packet the media server sends back — NACKs, PLI, receiver reports,
REMB — is dropped on the floor. ghost is therefore flying blind: it has no idea
whether anything it sends is arriving.

This is also a prerequisite for item 2. Registering a NACK responder without
draining the sender would have no effect, so these two changes belong together.

**How:**

```go
videoSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
if videoTrackErr != nil {
    return videoTrackErr
}
go drainRTCP(videoSender)

// and the same for the audio sender

// drainRTCP reads incoming RTCP so that interceptors (NACK, reports) can
// process it. Without this, feedback from the far end is never seen.
func drainRTCP(sender *webrtc.RTPSender) {
    buf := make([]byte, 1500)
    for {
        if _, _, err := sender.Read(buf); err != nil {
            return
        }
    }
}
```

The read returns an error when the peer connection closes, so the goroutine
ends on its own.

---

## 2. NACK is advertised in SDP but not implemented

**Where:** `client.go`, `initStack`

```go
vCodecFBs := []webrtc.RTCPFeedback{
    {Type: "nack"},
    {Type: "nack", Parameter: "pli"},
    {Type: "goog-remb"},
}
...
interceptReg := &interceptor.Registry{}
err := webrtc.ConfigureRTCPReports(interceptReg)
```

The SDP promises `nack` and `nack pli`, but the registry only gets
`ConfigureRTCPReports`, which adds sender and receiver reports and nothing
else. There is no NACK responder, so a packet the far end asks to have resent
is never resent.

**Why it matters:** video codecs code most frames as differences from earlier
ones, so a single lost packet in a keyframe means the receiver can decode
nothing until the next keyframe. With no retransmission and no way to ask a
file for a fresh keyframe on demand, that can be seconds of black or frozen
picture, or permanent if the GOP is long. We saw exactly this: files froze and
never recovered, and one showed no picture at all.

It matters for audio too. Opus has no FEC enabled here (item 3), so a lost
audio packet is simply gone.

**How:** one line, using what pion already ships:

```go
interceptReg := &interceptor.Registry{}
if err := webrtc.ConfigureNack(&m, interceptReg); err != nil {
    return err
}
if err := webrtc.ConfigureRTCPReports(interceptReg); err != nil {
    return err
}
```

`ConfigureNack` exists in the pion version ghost already pins
(`webrtc/v3@v3.2.42`). It adds a responder that keeps a short history of sent
packets and resends on request. Needs item 1 to take effect.

---

## 3. Opus is registered with a non-standard codec capability

**Where:** `client.go`, `initStack`

```go
opusCaps := webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000,
    Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil}
```

Three problems in one line.

**`Channels: 0`.** RFC 7587 requires the rtpmap encoding parameter for Opus to
be 2, even for mono audio. With 0, pion emits an rtpmap without a channel
count. Stacks vary in how they cope. Streaming a stereo Opus file produced
audio that was intelligible but metallic; re-encoding the same file to mono
with `-ac 1` cleared it up, which points at the channel signalling rather than
at loss.

**Empty `SDPFmtpLine`.** The usual WebRTC line is
`minptime=10;useinbandfec=1`. Without `useinbandfec=1`, Opus in-band forward
error correction is off, so there is no cheap recovery from isolated audio
loss — which, combined with item 2, means no recovery at all.

**`RTCPFeedback: nil`.** Audio gets no feedback at all, so NACK cannot apply to
it even once item 2 lands.

**How:**

```go
opusCaps := webrtc.RTPCodecCapability{
    MimeType:     webrtc.MimeTypeOpus,
    ClockRate:    48000,
    Channels:     2,
    SDPFmtpLine:  "minptime=10;useinbandfec=1",
    RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}},
}
```

Worth checking against the media server first, since it is SIP-oriented and may
have expectations about channel count. If mono is what it really wants, that is
worth stating explicitly in ghost's docs so callers know to send mono.

---

## 4. `TerminateCall` can block forever

**Where:** `client.go`

```go
func (cl *Client) TerminateCall() error {
    return cl.call.Terminate(context.Background())
}
```

`gosepp.Call.Terminate` sends a terminate message and then waits:

```go
select {
case <-ctx.Done():
    return fmt.Errorf("timeout")
case <-c.termCh:
}
```

A background context has a nil `Done` channel, so that case can never fire. If
the signalling websocket is down, the terminate message is dropped by gosepp's
sender goroutine (it logs `failed to send.` and moves on) and the confirmation
never arrives, so the call never returns.

**Why it matters:** exactly the situation where you most want to shut down —
the connection has failed — is the one where shutdown hangs. In our case the
process needed `kill -9`.

**How:** either give the caller control,

```go
func (cl *Client) TerminateCallContext(ctx context.Context) error {
    return cl.call.Terminate(ctx)
}
```

keeping `TerminateCall` as a wrapper with a sensible default,

```go
func (cl *Client) TerminateCall() error {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    return cl.call.Terminate(ctx)
}
```

Either is a small change and both are backwards compatible. Related: gosepp
dropping a message it cannot send is silent from the caller's point of view;
returning an error from `SendMsg` would let `Terminate` fail fast instead of
waiting for a reply that was never requested.

---

## 5. Connection failure is never surfaced to the caller

**Where:** `client.go`, `initStack`

```go
peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
    cl.logger.Info("ICE Connection State has changed: %s", connectionState.String())
    switch connectionState {
    case webrtc.ICEConnectionStateConnected:
        if cl.connectedHandler != nil {
            cl.connectedHandler(true, videoTrack, audioTrack)
        }
    }
})
```

`Disconnected` and `Failed` are logged and nothing else. Note also that
`ConnectedHandler` takes a `connected bool` that is only ever called with
`true`, so the parameter is currently dead.

**Why it matters:** when ICE failed during a test, the media path was gone for
good but the process sat there reconnecting signalling indefinitely, with no
way for the application to notice. A long-running player or bridge cannot
implement any restart or alerting policy without this.

**How:** the handler signature already has room for it.

```go
case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
    if cl.connectedHandler != nil {
        cl.connectedHandler(false, nil, nil)
    }
```

Passing nil tracks is a little awkward; a separate
`SetConnectionStateHandler(func(webrtc.ICEConnectionState))` would be cleaner
and would also expose `Disconnected`, which is recoverable and worth
distinguishing from `Failed`.

---

## 6. No congestion feedback

`webrtc.ConfigureTWCCSender` is available in the pinned pion version and is not
used. Without it there is no transport-wide congestion control, so ghost has no
bandwidth estimate and no basis for adapting.

This is a larger change than the others, since having an estimate is only
useful if something acts on it, and for file or RTMP passthrough there is no
encoder to turn down. Still worth discussing: even just exposing the estimate
would let an application warn, switch source, or drop to a lower-bitrate file.

Related, and cheaper: the sending side does no packet pacing. Writing a frame's
RTP packets back to back bursts hard on high-bitrate sources — a 14 Mbit/s 720p
file produced 106 packets written at once, 30 times a second. We added pacing
in the example (spreading each frame's packets across roughly the frame
interval, bounded so playback never falls behind), but every ghost source has
this problem, so it arguably belongs in the library next to the track.

---

## 7. Example dependency pins are behind

```
examples/rtmp-server/go.mod     ghost/v2 v2.8.1
examples/rtsp-client/go.mod     ghost/v2 v2.8.1
examples/video-playback/go.mod  ghost/v2 v2.7.1-0.20250626063411-6535290faea3
```

Current release is v2.9.9. The video-playback pin is old enough that
`WithForceVP8Codec` and `WithForceVP9Codec` do not exist in it — only the H264
and AV1 options do — so the example cannot select those codecs at all without a
bump. That is easy to hit and confusing to diagnose.

Worth considering whether the examples should be a Go workspace or share a
module, so they move together with the library.

---

## 8. The WebM parser mis-reads block timecodes (upstream)

**Where:** `github.com/ebml-go/webm`, `reader.go`

```go
p.Timecode = tbase + time.Millisecond*time.Duration(
    uint(data[1])<<8+uint(data[2]))
```

Matroska stores a block's timecode as a **signed** 16-bit offset from the
cluster timecode. This reads it as unsigned, so every negative offset comes
back exactly 65536ms too large.

**Why it matters:** it is not cosmetic. ffmpeg produces negative offsets
routinely when muxing with audio, because a cluster opens on a video keyframe
while an audio block in it can carry a slightly earlier presentation time. We
reproduced it immediately: a 60 second file reported a maximum timecode of
2m3s, with jumps of exactly +65.535s at every cluster boundary. Playback then
sleeps for a minute on the bad packet and the RTP timestamp jumps 65 seconds
forward and back, so the receiver decodes nothing.

**How:** the fix at the parse site is two lines — read the offset as `int16`:

```go
p.Timecode = tbase + time.Millisecond*time.Duration(
    int16(uint16(data[1])<<8|uint16(data[2])))
```

The example currently works around it downstream by detecting jumps larger than
half the wrap and subtracting 65536ms, which is reliable but is a heuristic on
the output rather than a fix. A patch upstream, or a small fork, would be
better. The library looks lightly maintained, so a fork may be the pragmatic
route.

---

## 9. Documentation gaps

- **Codec support is undiscoverable.** Which codecs ghost can negotiate, and
  which the media server accepts, is currently only findable by reading
  `client.go`. A short matrix in the README would save a lot of guessing. The
  video-playback example in particular had no README and silently packetized
  everything as VP8 regardless of what the file contained.
- **Meeting lifetime.** That a meeting shuts down shortly after the last
  participant leaves, and that a guest link only joins an existing meeting
  rather than creating one, is documented in the top-level README but easy to
  miss. It presents as a silent 180 second hang in `WaitReady`, which looks
  like a freeze. A log line while waiting would help, as would an error that
  names the likely cause.
- **`webm.Parse` lifetime.** It starts a goroutine that keeps reading from the
  file and parks at EOF waiting for a seek. Closing the file without calling
  `Shutdown()` and draining the channel leaks a goroutine and a file handle per
  playback loop. Worth a note wherever the parser is used.

---

## Suggested order

1. Items 1 + 2 together (RTCP drain + NACK) — biggest quality win, small diff.
2. Item 3 (Opus capability) — one line, fixes audio quality.
3. Item 4 (terminate timeout) — small, removes an unkillable-process failure.
4. Item 8 (timecode) — affects any WebM source; upstream or fork.
5. Items 5, 7, 9 — small, mostly ergonomics.
6. Item 6 (congestion control) — needs a design discussion first.
