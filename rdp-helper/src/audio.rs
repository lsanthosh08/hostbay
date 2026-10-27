//! RDP audio: remote speaker output via the `rdpsnd` static virtual channel.
//!
//! The server sends PCM wave chunks through `RdpsndClientHandler::wave`; we push
//! the samples into a ring buffer that a cpal output stream drains to the Mac's
//! speakers. We advertise a single PCM format (48 kHz / 16-bit / stereo) so we
//! never have to transcode — the server resamples to match.
//!
//! ponytail: one fixed PCM format + a mutex-guarded VecDeque ring. Ceiling: if
//! the server won't produce 48k/16/stereo, add more advertised formats and a
//! resample step keyed on `format_no`.

use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use ironrdp::rdpsnd::client::RdpsndClientHandler;
use ironrdp::rdpsnd::pdu::{AudioFormat, AudioFormatFlags, PitchPdu, VolumePdu, WaveFormat};
use std::borrow::Cow;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

const SAMPLE_RATE: u32 = 48_000;
const CHANNELS: u16 = 2;
const BITS: u16 = 16;
// Cap the buffer so a stalled output device can't grow it without bound
// (~2 s of audio). Older samples are dropped if we overrun.
const MAX_SAMPLES: usize = (SAMPLE_RATE as usize) * CHANNELS as usize * 2;

type Ring = Arc<Mutex<VecDeque<i16>>>;

/// rdpsnd backend: parks incoming PCM in a ring buffer + owns the cpal stream.
pub struct AudioOut {
    formats: Vec<AudioFormat>,
    ring: Ring,
    // Kept alive for the session; dropping it stops playback.
    _stream: Option<cpal::Stream>,
}

// cpal::Stream isn't Send on all backends, but the rdpsnd handler must be Send.
// The stream is only ever touched on the audio thread that constructs AudioOut
// and never moved across threads afterwards, so this is sound in our usage.
unsafe impl Send for AudioOut {}

impl core::fmt::Debug for AudioOut {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(f, "AudioOut")
    }
}

impl AudioOut {
    pub fn new() -> Self {
        let ring: Ring = Arc::new(Mutex::new(VecDeque::new()));
        let stream = build_output_stream(Arc::clone(&ring));
        if stream.is_none() {
            eprintln!("[rdp-audio] no output device — remote audio disabled");
        }
        AudioOut {
            formats: vec![AudioFormat {
                format: WaveFormat::PCM,
                n_channels: CHANNELS,
                n_samples_per_sec: SAMPLE_RATE,
                n_avg_bytes_per_sec: SAMPLE_RATE * (CHANNELS * BITS / 8) as u32,
                n_block_align: CHANNELS * BITS / 8,
                bits_per_sample: BITS,
                data: None,
            }],
            ring,
            _stream: stream,
        }
    }
}

impl RdpsndClientHandler for AudioOut {
    fn get_flags(&self) -> AudioFormatFlags {
        AudioFormatFlags::ALIVE | AudioFormatFlags::VOLUME
    }

    fn get_formats(&self) -> &[AudioFormat] {
        &self.formats
    }

    fn wave(&mut self, _format: &AudioFormat, _ts: u32, data: Cow<'_, [u8]>) {
        // Interpret the bytes as little-endian i16 PCM and enqueue.
        let mut ring = self.ring.lock().unwrap();
        for chunk in data.chunks_exact(2) {
            ring.push_back(i16::from_le_bytes([chunk[0], chunk[1]]));
        }
        // Drop oldest samples if we've overrun (device stalled / not draining).
        while ring.len() > MAX_SAMPLES {
            ring.pop_front();
        }
    }

    fn set_volume(&mut self, _volume: VolumePdu) {}
    fn set_pitch(&mut self, _pitch: PitchPdu) {}
    fn close(&mut self) {
        self.ring.lock().unwrap().clear();
    }
}

/// Open a cpal output stream that pulls i16 samples from `ring`. Returns None if
/// there's no usable output device. macOS CoreAudio is f32-only, so f32 is the
/// primary path (converting i16 -> f32); i16 is a fallback for hosts that take
/// it directly.
fn build_output_stream(ring: Ring) -> Option<cpal::Stream> {
    let host = cpal::default_host();
    let device = host.default_output_device()?;
    let config = cpal::StreamConfig {
        channels: CHANNELS,
        sample_rate: cpal::SampleRate(SAMPLE_RATE),
        buffer_size: cpal::BufferSize::Default,
    };
    let err_fn = |e| eprintln!("[rdp-audio] output stream error: {e}");

    let ring_f = Arc::clone(&ring);
    let f32_stream = device.build_output_stream(
        &config,
        move |out: &mut [f32], _| {
            let mut ring = ring_f.lock().unwrap();
            for sample in out.iter_mut() {
                // i16 -> f32 in [-1, 1]; silence on underrun.
                *sample = ring.pop_front().map(|s| s as f32 / 32768.0).unwrap_or(0.0);
            }
        },
        err_fn,
        None,
    );
    if let Ok(stream) = f32_stream {
        stream.play().ok()?;
        return Some(stream);
    }

    // Fallback: device that accepts i16 directly.
    let stream = device
        .build_output_stream(
            &config,
            move |out: &mut [i16], _| {
                let mut ring = ring.lock().unwrap();
                for sample in out.iter_mut() {
                    *sample = ring.pop_front().unwrap_or(0);
                }
            },
            err_fn,
            None,
        )
        .ok()?;
    stream.play().ok()?;
    Some(stream)
}
