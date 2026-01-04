package gtkcord

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/voice"
	"github.com/diamondburned/arikawa/v3/voice/voicegateway"
)

// VoiceSession manages a single voice connection.
type VoiceSession struct {
	mu      sync.RWMutex
	state   *State
	session *voice.Session

	guildID   discord.GuildID
	channelID discord.ChannelID

	muted    bool
	deafened bool

	// Audio control
	micCancel     context.CancelFunc
	speakerCancel context.CancelFunc

	// Audio components
	capture  *AudioCapture
	playback *AudioPlayback
}

// NewVoiceSession creates a new voice session manager.
func NewVoiceSession(state *State) (*VoiceSession, error) {
	voiceSession, err := voice.NewSession(state.State.State)
	if err != nil {
		return nil, err
	}

	capture, err := NewAudioCapture()
	if err != nil {
		slog.Warn("failed to create audio capture", "err", err)
		// Continue without mic - user can still listen
	}

	playback, err := NewAudioPlayback()
	if err != nil {
		slog.Warn("failed to create audio playback", "err", err)
		// Continue without playback - user can still speak
	}

	return &VoiceSession{
		state:    state,
		session:  voiceSession,
		capture:  capture,
		playback: playback,
	}, nil
}

// JoinChannel joins the specified voice channel.
func (s *VoiceSession) JoinChannel(ctx context.Context, guildID discord.GuildID, channelID discord.ChannelID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.guildID = guildID
	s.channelID = channelID

	slog.Info("joining voice channel",
		"guild_id", guildID,
		"channel_id", channelID)

	if err := s.session.JoinChannel(ctx, channelID, s.muted, s.deafened); err != nil {
		return err
	}

	// Start speaker (receiving audio)
	speakerCtx, speakerCancel := context.WithCancel(context.Background())
	s.speakerCancel = speakerCancel
	go s.runSpeaker(speakerCtx)

	// Start microphone if not muted
	if !s.muted {
		micCtx, micCancel := context.WithCancel(context.Background())
		s.micCancel = micCancel
		go s.runMicrophone(micCtx)
	}

	return nil
}

// LeaveChannel leaves the current voice channel.
func (s *VoiceSession) LeaveChannel(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	slog.Info("leaving voice channel",
		"guild_id", s.guildID,
		"channel_id", s.channelID)

	// Stop audio components
	if s.micCancel != nil {
		s.micCancel()
		s.micCancel = nil
	}
	if s.speakerCancel != nil {
		s.speakerCancel()
		s.speakerCancel = nil
	}

	s.guildID = 0
	s.channelID = 0

	return s.session.Leave(ctx)
}

// Close releases all resources.
func (s *VoiceSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.micCancel != nil {
		s.micCancel()
		s.micCancel = nil
	}
	if s.speakerCancel != nil {
		s.speakerCancel()
		s.speakerCancel = nil
	}
	if s.capture != nil {
		s.capture.Close()
		s.capture = nil
	}
	if s.playback != nil {
		s.playback.Close()
		s.playback = nil
	}
}

// runMicrophone handles audio capture and sending.
func (s *VoiceSession) runMicrophone(ctx context.Context) {
	slog.Info("microphone started")
	defer slog.Info("microphone stopped")

	// Tell Discord we're speaking
	if err := s.session.Speaking(ctx, voicegateway.Microphone); err != nil {
		slog.Error("failed to set speaking state", "err", err)
	}

	if s.capture == nil {
		slog.Warn("no audio capture available")
		<-ctx.Done()
		s.session.Speaking(context.Background(), voicegateway.NotSpeaking)
		return
	}

	// Start audio capture and send to Discord
	if err := s.capture.Start(ctx, s.session.Write); err != nil {
		slog.Error("failed to start audio capture", "err", err)
		<-ctx.Done()
		s.session.Speaking(context.Background(), voicegateway.NotSpeaking)
		return
	}

	<-ctx.Done()

	s.capture.Stop()

	// Tell Discord we stopped speaking
	s.session.Speaking(context.Background(), voicegateway.NotSpeaking)
}

// runSpeaker handles receiving and playing audio.
func (s *VoiceSession) runSpeaker(ctx context.Context) {
	slog.Info("speaker started")
	defer slog.Info("speaker stopped")

	if s.playback == nil {
		slog.Warn("no audio playback available")
		<-ctx.Done()
		return
	}

	// Start playback
	if err := s.playback.Start(ctx); err != nil {
		slog.Error("failed to start audio playback", "err", err)
		<-ctx.Done()
		return
	}

	for {
		select {
		case <-ctx.Done():
			s.playback.Stop()
			return
		default:
			packet, err := s.session.ReadPacket()
			if err != nil {
				if err == io.EOF {
					s.playback.Stop()
					return
				}
				slog.Debug("error reading voice packet", "err", err)
				time.Sleep(10 * time.Millisecond)
				continue
			}

			// Send packet to playback
			s.playback.PlayPacket(packet.SSRC(), packet.Opus)
		}
	}
}

// SetMuted sets the muted state.
func (s *VoiceSession) SetMuted(muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.muted = muted

	if muted {
		if s.micCancel != nil {
			s.micCancel()
			s.micCancel = nil
		}
	} else if s.channelID.IsValid() && s.micCancel == nil {
		micCtx, micCancel := context.WithCancel(context.Background())
		s.micCancel = micCancel
		go s.runMicrophone(micCtx)
	}
}

// SetDeafened sets the deafened state.
func (s *VoiceSession) SetDeafened(deafened bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deafened = deafened

	if deafened {
		if s.speakerCancel != nil {
			s.speakerCancel()
			s.speakerCancel = nil
		}
	} else if s.channelID.IsValid() && s.speakerCancel == nil {
		speakerCtx, speakerCancel := context.WithCancel(context.Background())
		s.speakerCancel = speakerCancel
		go s.runSpeaker(speakerCtx)
	}
}

// IsMuted returns the current mute state.
func (s *VoiceSession) IsMuted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.muted
}

// IsDeafened returns the current deafen state.
func (s *VoiceSession) IsDeafened() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deafened
}

// IsConnected returns true if connected to a voice channel.
func (s *VoiceSession) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelID.IsValid()
}

// ChannelID returns the current channel ID.
func (s *VoiceSession) ChannelID() discord.ChannelID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelID
}

// GuildID returns the current guild ID.
func (s *VoiceSession) GuildID() discord.GuildID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guildID
}

// VoiceManager manages voice sessions across guilds.
type VoiceManager struct {
	state   *State
	mu      sync.RWMutex
	session *VoiceSession
}

// NewVoiceManager creates a new voice manager.
func NewVoiceManager(state *State) *VoiceManager {
	return &VoiceManager{
		state: state,
	}
}

// JoinChannel joins a voice channel, leaving any existing connection first.
func (m *VoiceManager) JoinChannel(ctx context.Context, guildID discord.GuildID, channelID discord.ChannelID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Leave and cleanup existing session if any
	if m.session != nil {
		// Best effort leave
		m.session.LeaveChannel(ctx)
		m.session.Close()
		m.session = nil
	}

	// Always create a new session - arikawa voice.Session is single-use
	var err error
	m.session, err = NewVoiceSession(m.state)
	if err != nil {
		return err
	}

	return m.session.JoinChannel(ctx, guildID, channelID)
}

// LeaveChannel leaves the current voice channel.
func (m *VoiceManager) LeaveChannel(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session == nil {
		return nil
	}

	return m.session.LeaveChannel(ctx)
}

// ToggleMute toggles the mute state.
func (m *VoiceManager) ToggleMute() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session == nil {
		return false
	}

	newState := !m.session.IsMuted()
	m.session.SetMuted(newState)
	return newState
}

// ToggleDeafen toggles the deafen state.
func (m *VoiceManager) ToggleDeafen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.session == nil {
		return false
	}

	newState := !m.session.IsDeafened()
	m.session.SetDeafened(newState)
	return newState
}

// Session returns the current session.
func (m *VoiceManager) Session() *VoiceSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.session
}

// IsConnected returns true if connected to any voice channel.
func (m *VoiceManager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.session != nil && m.session.IsConnected()
}
