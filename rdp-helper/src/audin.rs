//! RDP microphone input over the AUDIO_INPUT dynamic virtual channel
//! (MS-RDPEAI). No IronRDP crate implements the client side, so the PDUs are
//! hand-encoded here.
//!
//! Handshake (server -> client unless noted):
//!   Version(0x01)            -> we echo Version
//!   Formats(0x02)            -> we reply Formats (our supported subset)
//!   Open(0x04)               -> we reply OpenReply(0x05), then start capturing
//!   FormatChange(0x08)       -> we reply FormatChange
//! Streaming (client -> server), repeated while open:
//!   DataIncoming(0x06) then Data(0x07) carrying raw PCM.
//!
//! The DvcProcessor handles the handshake. Mic capture runs on a cpal input
//! stream that fills a ring buffer; the active loop pulls PCM from
//! `take_pcm()` and sends Data PDUs via the channel id we expose.
//!
//! ponytail: one fixed PCM format (48kHz/16-bit/mono) advertised; the server
//! picks it. Ceiling: if a server demands a format we don't offer, add formats
//! + resample keyed on the Open PDU's format index.

use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use ironrdp::core::{Encode, EncodeResult, WriteCursor};
use ironrdp::dvc::{DvcClientProcessor, DvcEncode, DvcMessage, DvcProcessor};
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

const MSG_VERSION: u8 = 0x01;
const MSG_FORMATS: u8 = 0x02;
const MSG_OPEN: u8 = 0x04;
const MSG_OPEN_REPLY: u8 = 0x05;
const MSG_DATA_INCOMING: u8 = 0x06;
const MSG_DATA: u8 = 0x07;
const MSG_FORMAT_CHANGE: u8 = 0x08;

const WAVE_FORMAT_PCM: u16 = 0x0001;
const SAMPLE_RATE: u32 = 48_000;
const CHANNELS: u16 = 1;
const BITS: u16 = 16;
const BLOCK_ALIGN: u16 = CHANNELS * BITS / 8;
// Cap the mic ring so a stalled sender can't grow it unbounded (~2 s).
const MAX_BYTES: usize = (SAMPLE_RATE as usize) * BLOCK_ALIGN as usize * 2;

/// Shared mic state: captured PCM waiting to be sent, and whether the channel
/// is open (so capture only runs while the server wants audio).
#[derive(Default)]
pub struct MicShared {
    pcm: VecDeque<u8>,
    open: bool,
}

pub type Mic = Arc<Mutex<MicShared>>;

/// Raw pre-encoded DVC payload (we hand-build AUDIN PDUs as bytes).
struct RawPdu(Vec<u8>);
impl Encode for RawPdu {
    fn encode(&self, dst: &mut WriteCursor<'_>) -> EncodeResult<()> {
        dst.write_slice(&self.0);
        Ok(())
    }
    fn name(&self) -> &'static str {
        "AudinPdu"
    }
    fn size(&self) -> usize {
        self.0.len()
    }
}
impl DvcEncode for RawPdu {}

fn raw(bytes: Vec<u8>) -> DvcMessage {
    Box::new(RawPdu(bytes))
}

/// Our single advertised capture format, as an MS-RDPEAI AUDIO_FORMAT (18 bytes,
/// cbSize = 0). Layout: wFormatTag, nChannels, nSamplesPerSec, nAvgBytesPerSec,
/// nBlockAlign, wBitsPerSample, cbSize.
fn our_format() -> Vec<u8> {
    let avg = SAMPLE_RATE * BLOCK_ALIGN as u32;
    let mut v = Vec::with_capacity(18);
    v.extend_from_slice(&WAVE_FORMAT_PCM.to_le_bytes());
    v.extend_from_slice(&CHANNELS.to_le_bytes());
    v.extend_from_slice(&SAMPLE_RATE.to_le_bytes());
    v.extend_from_slice(&avg.to_le_bytes());
    v.extend_from_slice(&BLOCK_ALIGN.to_le_bytes());
    v.extend_from_slice(&BITS.to_le_bytes());
    v.extend_from_slice(&0u16.to_le_bytes()); // cbSize
    v
}

pub struct AudinChannel {
    mic: Mic,
    // cpal input stream; kept alive while the channel is registered.
    _stream: Option<cpal::Stream>,
}

// cpal::Stream is not Send on every backend; we only build/drop it on the
// session thread and never move it afterwards.
unsafe impl Send for AudinChannel {}

impl core::fmt::Debug for AudinChannel {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "AudinChannel")
    }
}

impl AudinChannel {
    pub fn new(mic: Mic) -> Self {
        let stream = build_input_stream(Arc::clone(&mic));
        if stream.is_none() {
            eprintln!("[rdp-audin] no input device — mic disabled");
        }
        AudinChannel { mic, _stream: stream }
    }
}

impl ironrdp::core::AsAny for AudinChannel {
    fn as_any(&self) -> &dyn core::any::Any {
        self
    }
    fn as_any_mut(&mut self) -> &mut dyn core::any::Any {
        self
    }
}

impl DvcClientProcessor for AudinChannel {}

impl DvcProcessor for AudinChannel {
    fn channel_name(&self) -> &str {
        "AUDIO_INPUT"
    }

    fn start(&mut self, _channel_id: u32) -> ironrdp::pdu::PduResult<Vec<DvcMessage>> {
        // Server drives the handshake; nothing to send on open.
        Ok(Vec::new())
    }

    fn process(&mut self, _channel_id: u32, payload: &[u8]) -> ironrdp::pdu::PduResult<Vec<DvcMessage>> {
        let Some(&msg_id) = payload.first() else {
            return Ok(Vec::new());
        };
        match msg_id {
            MSG_VERSION => {
                // Echo the server's Version (MessageId + Version u32).
                let version = payload.get(1..5).unwrap_or(&[1, 0, 0, 0]);
                let mut out = vec![MSG_VERSION];
                out.extend_from_slice(version);
                eprintln!("[rdp-audin] version");
                Ok(vec![raw(out)])
            }
            MSG_FORMATS => {
                // Reply with our supported formats.
                // Layout: MessageId(1) NumFormats(u32) cbSizeFormatsPacket(u32) then formats.
                let fmt = our_format();
                let mut out = vec![MSG_FORMATS];
                out.extend_from_slice(&1u32.to_le_bytes()); // NumFormats
                out.extend_from_slice(&(fmt.len() as u32).to_le_bytes()); // cbSizeFormatsPacket
                out.extend_from_slice(&fmt);
                eprintln!("[rdp-audin] formats");
                Ok(vec![raw(out)])
            }
            MSG_OPEN => {
                // Server opened the stream. Reply OpenReply (result = 0 / S_OK)
                // and mark the channel open so capture flows.
                self.mic.lock().unwrap().open = true;
                let mut out = vec![MSG_OPEN_REPLY];
                out.extend_from_slice(&0u32.to_le_bytes()); // Result HRESULT = S_OK
                eprintln!("[rdp-audin] open -> capturing");
                Ok(vec![raw(out)])
            }
            MSG_FORMAT_CHANGE => {
                // Echo the format index the server selected.
                let idx = payload.get(1..5).unwrap_or(&[0, 0, 0, 0]);
                let mut out = vec![MSG_FORMAT_CHANGE];
                out.extend_from_slice(idx);
                Ok(vec![raw(out)])
            }
            _ => Ok(Vec::new()),
        }
    }

    fn close(&mut self, _channel_id: u32) {
        let mut m = self.mic.lock().unwrap();
        m.open = false;
        m.pcm.clear();
    }
}

/// Build the two PDUs that carry a chunk of mic PCM: DataIncoming then Data.
/// Returns an empty vec if there's nothing to send or the channel isn't open.
pub fn take_pcm_pdus(mic: &Mic) -> Vec<DvcMessage> {
    let pcm: Vec<u8> = {
        let mut m = mic.lock().unwrap();
        if !m.open || m.pcm.is_empty() {
            return Vec::new();
        }
        // Send in bounded chunks so a single PDU never gets too large.
        let take = m.pcm.len().min(4096);
        m.pcm.drain(..take).collect()
    };
    // DataIncoming (MessageId only), then Data (MessageId + PCM).
    let incoming = vec![MSG_DATA_INCOMING];
    let mut data = Vec::with_capacity(pcm.len() + 1);
    data.push(MSG_DATA);
    data.extend_from_slice(&pcm);
    vec![raw(incoming), raw(data)]
}

/// cpal input stream that captures mic audio into the ring while the channel is
/// open. Converts f32 (CoreAudio default) to i16 LE PCM.
fn build_input_stream(mic: Mic) -> Option<cpal::Stream> {
    let host = cpal::default_host();
    let device = host.default_input_device()?;
    let config = cpal::StreamConfig {
        channels: CHANNELS,
        sample_rate: cpal::SampleRate(SAMPLE_RATE),
        buffer_size: cpal::BufferSize::Default,
    };
    let err_fn = |e| eprintln!("[rdp-audin] input stream error: {e}");

    let mic_f = Arc::clone(&mic);
    let f32_stream = device.build_input_stream(
        &config,
        move |input: &[f32], _| {
            let mut m = mic_f.lock().unwrap();
            if !m.open {
                return; // don't buffer while the server isn't listening
            }
            for &s in input {
                let v = (s.clamp(-1.0, 1.0) * 32767.0) as i16;
                m.pcm.extend(v.to_le_bytes());
            }
            while m.pcm.len() > MAX_BYTES {
                m.pcm.pop_front();
            }
        },
        err_fn,
        None,
    );
    if let Ok(stream) = f32_stream {
        stream.play().ok()?;
        return Some(stream);
    }

    // Fallback: device that yields i16 directly.
    let stream = device
        .build_input_stream(
            &config,
            move |input: &[i16], _| {
                let mut m = mic.lock().unwrap();
                if !m.open {
                    return;
                }
                for &v in input {
                    m.pcm.extend(v.to_le_bytes());
                }
                while m.pcm.len() > MAX_BYTES {
                    m.pcm.pop_front();
                }
            },
            err_fn,
            None,
        )
        .ok()?;
    stream.play().ok()?;
    Some(stream)
}
