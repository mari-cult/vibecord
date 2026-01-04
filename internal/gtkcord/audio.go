package gtkcord

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"unsafe"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
	"gopkg.in/hraban/opus.v2"
)

const (
	// Discord voice settings
	sampleRate    = 48000
	channels      = 2
	frameDuration = 20 // ms
	frameSize     = sampleRate * frameDuration / 1000 // 960 samples per channel
)

// AudioCapture handles microphone input and Opus encoding.
type AudioCapture struct {
	mu      sync.Mutex
	client  *pulse.Client
	stream  *pulse.RecordStream
	encoder *opus.Encoder
	running bool
	stopCh  chan struct{}
}

// opusApplication is the application type for the Opus encoder.
const opusApplication = opus.AppVoIP

// NewAudioCapture creates a new audio capture instance.
func NewAudioCapture() (*AudioCapture, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("Dissent"))
	if err != nil {
		return nil, err
	}

	encoder, err := opus.NewEncoder(sampleRate, channels, opusApplication)
	if err != nil {
		client.Close()
		return nil, err
	}

	return &AudioCapture{
		client:  client,
		encoder: encoder,
		stopCh:  make(chan struct{}),
	}, nil
}

// pcmReader implements pulse.Writer for capturing audio (pulse calls Write with recorded data)
type pcmReader struct {
	dataCh chan []float32
}

func (r *pcmReader) Write(buf []byte) (int, error) {
	// Convert bytes to float32
	floats := make([]float32, len(buf)/4)
	for i := range floats {
		bits := uint32(buf[i*4]) | uint32(buf[i*4+1])<<8 | uint32(buf[i*4+2])<<16 | uint32(buf[i*4+3])<<24
		floats[i] = float32frombits(bits)
	}
	select {
	case r.dataCh <- floats:
	default:
		// Drop if buffer full
	}
	return len(buf), nil
}

func (r *pcmReader) Format() byte {
	return proto.FormatFloat32LE
}

func float32frombits(b uint32) float32 {
	return *(*float32)(unsafe.Pointer(&b))
}

func float32bits(f float32) uint32 {
	return *(*uint32)(unsafe.Pointer(&f))
}

// Start begins capturing audio and sending it through the provided writer.
func (a *AudioCapture) Start(ctx context.Context, writer func([]byte) (int, error)) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = true
	a.stopCh = make(chan struct{})
	a.mu.Unlock()

	dataCh := make(chan []float32, 10)
	reader := &pcmReader{dataCh: dataCh}

	// Create a record stream with stereo channel map
	channelMap := proto.ChannelMap{proto.ChannelFrontLeft, proto.ChannelFrontRight}
	stream, err := a.client.NewRecord(
		reader,
		pulse.RecordSampleRate(sampleRate),
		pulse.RecordChannels(channelMap),
		pulse.RecordMediaName("Discord Voice"),
	)
	if err != nil {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
		return err
	}

	a.mu.Lock()
	a.stream = stream
	a.mu.Unlock()

	stream.Start()

	go a.captureLoop(ctx, writer, dataCh)

	return nil
}

func (a *AudioCapture) captureLoop(ctx context.Context, writer func([]byte) (int, error), dataCh chan []float32) {
	defer func() {
		a.mu.Lock()
		a.running = false
		if a.stream != nil {
			a.stream.Stop()
			a.stream.Close()
			a.stream = nil
		}
		a.mu.Unlock()
	}()

	// Buffer for int16 PCM (for Opus encoder) - use larger buffer for incoming data
	pcmInt16 := make([]int16, 48000) // 1 second buffer max
	// Buffer for Opus encoded data
	opusBuf := make([]byte, 4000) // max Opus frame size

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case pcmFloat := <-dataCh:
			n := len(pcmFloat)
			if n == 0 {
				continue
			}

			// Clamp to buffer size
			if n > len(pcmInt16) {
				n = len(pcmInt16)
			}

			// Convert float32 to int16
			for i := 0; i < n; i++ {
				sample := pcmFloat[i]
				if sample > 1.0 {
					sample = 1.0
				} else if sample < -1.0 {
					sample = -1.0
				}
				pcmInt16[i] = int16(sample * 32767)
			}

			// Process in frame-sized chunks (960 samples * 2 channels = 1920)
			frameSamples := frameSize * channels
			for offset := 0; offset+frameSamples <= n; offset += frameSamples {
				// Encode to Opus
				opusLen, err := a.encoder.Encode(pcmInt16[offset:offset+frameSamples], opusBuf)
				if err != nil {
					slog.Debug("opus encode error", "err", err)
					continue
				}

				// Send to Discord
				if _, err := writer(opusBuf[:opusLen]); err != nil {
					slog.Debug("error writing voice data", "err", err)
				}
			}
		}
	}
}

// Stop stops the audio capture.
func (a *AudioCapture) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running {
		close(a.stopCh)
	}

	if a.stream != nil {
		a.stream.Stop()
		a.stream.Close()
		a.stream = nil
	}
	a.running = false
}

// Close releases all resources.
func (a *AudioCapture) Close() {
	a.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.client != nil {
		a.client.Close()
		a.client = nil
	}
}

// AudioPlayback handles audio playback from received Opus packets.
type AudioPlayback struct {
	mu       sync.Mutex
	client   *pulse.Client
	stream   *pulse.PlaybackStream
	decoders map[uint32]*opus.Decoder // SSRC -> decoder
	running  bool
	audioCh  chan audioPacket
	stopCh   chan struct{}
	pcmCh    chan []float32
}

type audioPacket struct {
	ssrc uint32
	opus []byte
}

// pcmWriter implements pulse.Reader for playback (pulse calls Read to get data to play)
type pcmWriter struct {
	pcmCh chan []float32
}

func (w *pcmWriter) Read(buf []byte) (int, error) {
	select {
	case data := <-w.pcmCh:
		// Convert float32 to bytes
		n := len(data)
		if n*4 > len(buf) {
			n = len(buf) / 4
		}
		for i := 0; i < n; i++ {
			bits := float32bits(data[i])
			buf[i*4] = byte(bits)
			buf[i*4+1] = byte(bits >> 8)
			buf[i*4+2] = byte(bits >> 16)
			buf[i*4+3] = byte(bits >> 24)
		}
		return n * 4, nil
	default:
		// No data available, output silence
		for i := range buf {
			buf[i] = 0
		}
		return len(buf), nil
	}
}

func (w *pcmWriter) Format() byte {
	return proto.FormatFloat32LE
}

// NewAudioPlayback creates a new audio playback instance.
func NewAudioPlayback() (*AudioPlayback, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("Dissent"))
	if err != nil {
		return nil, err
	}

	return &AudioPlayback{
		client:   client,
		decoders: make(map[uint32]*opus.Decoder),
		audioCh:  make(chan audioPacket, 100),
		stopCh:   make(chan struct{}),
		pcmCh:    make(chan []float32, 10),
	}, nil
}

// Start begins playback.
func (a *AudioPlayback) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil
	}
	a.running = true
	a.stopCh = make(chan struct{})
	a.mu.Unlock()

	writer := &pcmWriter{pcmCh: a.pcmCh}

	// Create playback stream with stereo channel map
	channelMap := proto.ChannelMap{proto.ChannelFrontLeft, proto.ChannelFrontRight}
	stream, err := a.client.NewPlayback(
		writer,
		pulse.PlaybackSampleRate(sampleRate),
		pulse.PlaybackChannels(channelMap),
		pulse.PlaybackMediaName("Discord Voice"),
	)
	if err != nil {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
		return err
	}

	a.mu.Lock()
	a.stream = stream
	a.mu.Unlock()

	stream.Start()

	go a.playbackLoop(ctx)

	return nil
}

func (a *AudioPlayback) playbackLoop(ctx context.Context) {
	defer func() {
		a.mu.Lock()
		a.running = false
		if a.stream != nil {
			a.stream.Stop()
			a.stream.Close()
			a.stream = nil
		}
		a.mu.Unlock()
	}()

	// PCM buffer for decoded audio (int16 for opus decoder)
	pcmInt16 := make([]int16, 5760*channels) // max Opus frame size

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case pkt := <-a.audioCh:
			a.mu.Lock()
			decoder, ok := a.decoders[pkt.ssrc]
			if !ok {
				var err error
				decoder, err = opus.NewDecoder(sampleRate, channels)
				if err != nil {
					a.mu.Unlock()
					slog.Debug("failed to create decoder", "ssrc", pkt.ssrc, "err", err)
					continue
				}
				a.decoders[pkt.ssrc] = decoder
			}
			a.mu.Unlock()

			// Decode Opus to PCM (int16)
			n, err := decoder.Decode(pkt.opus, pcmInt16)
			if err != nil {
				slog.Debug("opus decode error", "err", err)
				continue
			}

			// Convert int16 to float32 for PulseAudio
			pcmFloat := make([]float32, n*channels)
			for i := 0; i < n*channels; i++ {
				pcmFloat[i] = float32(pcmInt16[i]) / 32768.0
			}

			// Send to playback stream
			select {
			case a.pcmCh <- pcmFloat:
			case <-time.After(10 * time.Millisecond):
				// Drop if buffer full
			}
		}
	}
}

// PlayPacket queues an audio packet for playback.
func (a *AudioPlayback) PlayPacket(ssrc uint32, opusData []byte) {
	select {
	case a.audioCh <- audioPacket{ssrc: ssrc, opus: opusData}:
	default:
		// Drop packet if buffer is full
	}
}

// Stop stops playback.
func (a *AudioPlayback) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running {
		close(a.stopCh)
	}

	if a.stream != nil {
		a.stream.Stop()
		a.stream.Close()
		a.stream = nil
	}
	a.running = false
}

// Close releases all resources.
func (a *AudioPlayback) Close() {
	a.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.client != nil {
		a.client.Close()
		a.client = nil
	}
}
