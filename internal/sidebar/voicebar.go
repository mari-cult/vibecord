package sidebar

import (
	"context"

	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
	"libdb.so/dissent/internal/gtkcord"
)

var voiceBarCSS = cssutil.Applier("voice-bar", `
	.voice-bar {
		padding: 8px 12px;
		background: alpha(@success_color, 0.1);
		border-top: 1px solid alpha(@theme_fg_color, 0.1);
	}
	.voice-bar-disconnected {
		background: none;
	}
	.voice-bar-channel {
		font-weight: 600;
		font-size: 0.9em;
	}
	.voice-bar-status {
		font-size: 0.8em;
		opacity: 0.7;
	}
	.voice-bar-buttons {
		margin-left: 8px;
	}
	.voice-bar-buttons button {
		min-width: 28px;
		min-height: 28px;
		padding: 4px;
		border-radius: 50%;
	}
	.voice-bar-muted {
		background: @error_color;
		color: white;
	}
	.voice-bar-muted:hover {
		background: lighter(@error_color);
	}
`)

// VoiceBar is a voice control bar shown when connected to a voice channel.
type VoiceBar struct {
	*gtk.Box
	ctx     context.Context
	manager *gtkcord.VoiceManager

	channelLabel *gtk.Label
	statusLabel  *gtk.Label
	muteButton   *gtk.Button
	deafenButton *gtk.Button
	leaveButton  *gtk.Button

	guildID   discord.GuildID
	channelID discord.ChannelID
}

// NewVoiceBar creates a new voice control bar.
func NewVoiceBar(ctx context.Context, manager *gtkcord.VoiceManager) *VoiceBar {
	b := &VoiceBar{
		ctx:     ctx,
		manager: manager,
	}

	b.channelLabel = gtk.NewLabel("")
	b.channelLabel.AddCSSClass("voice-bar-channel")
	b.channelLabel.SetHAlign(gtk.AlignStart)
	b.channelLabel.SetEllipsize(3) // PANGO_ELLIPSIZE_END

	b.statusLabel = gtk.NewLabel("Voice Connected")
	b.statusLabel.AddCSSClass("voice-bar-status")
	b.statusLabel.SetHAlign(gtk.AlignStart)

	labels := gtk.NewBox(gtk.OrientationVertical, 2)
	labels.Append(b.channelLabel)
	labels.Append(b.statusLabel)
	labels.SetHExpand(true)

	b.muteButton = gtk.NewButtonFromIconName("microphone-sensitivity-high-symbolic")
	b.muteButton.SetTooltipText("Toggle Mute")
	b.muteButton.ConnectClicked(func() {
		muted := manager.ToggleMute()
		b.updateMuteButton(muted)
	})

	b.deafenButton = gtk.NewButtonFromIconName("audio-volume-high-symbolic")
	b.deafenButton.SetTooltipText("Toggle Deafen")
	b.deafenButton.ConnectClicked(func() {
		deafened := manager.ToggleDeafen()
		b.updateDeafenButton(deafened)
	})

	b.leaveButton = gtk.NewButtonFromIconName("call-stop-symbolic")
	b.leaveButton.SetTooltipText("Disconnect")
	b.leaveButton.AddCSSClass("destructive-action")
	b.leaveButton.ConnectClicked(func() {
		manager.LeaveChannel(ctx)
		b.Disconnect()
	})

	buttons := gtk.NewBox(gtk.OrientationHorizontal, 4)
	buttons.AddCSSClass("voice-bar-buttons")
	buttons.Append(b.muteButton)
	buttons.Append(b.deafenButton)
	buttons.Append(b.leaveButton)

	b.Box = gtk.NewBox(gtk.OrientationHorizontal, 8)
	b.Append(labels)
	b.Append(buttons)

	voiceBarCSS(b)
	b.SetVisible(false)

	return b
}

// Connect shows the bar for the given channel.
func (b *VoiceBar) Connect(guildID discord.GuildID, channelID discord.ChannelID) {
	b.guildID = guildID
	b.channelID = channelID

	state := gtkcord.FromContext(b.ctx)
	ch, _ := state.Cabinet.Channel(channelID)
	if ch != nil {
		b.channelLabel.SetText(ch.Name)
	} else {
		b.channelLabel.SetText("Voice Channel")
	}

	b.statusLabel.SetText("Voice Connected")
	b.updateMuteButton(false)
	b.updateDeafenButton(false)
	b.RemoveCSSClass("voice-bar-disconnected")
	b.SetVisible(true)
}

// Disconnect hides the bar.
func (b *VoiceBar) Disconnect() {
	b.guildID = 0
	b.channelID = 0
	b.AddCSSClass("voice-bar-disconnected")
	b.SetVisible(false)
}

// IsConnected returns true if the bar is showing a connection.
func (b *VoiceBar) IsConnected() bool {
	return b.channelID.IsValid()
}

// ChannelID returns the connected channel ID.
func (b *VoiceBar) ChannelID() discord.ChannelID {
	return b.channelID
}

func (b *VoiceBar) updateMuteButton(muted bool) {
	if muted {
		b.muteButton.SetIconName("microphone-sensitivity-muted-symbolic")
		b.muteButton.AddCSSClass("voice-bar-muted")
		b.updateStatus()
	} else {
		b.muteButton.SetIconName("microphone-sensitivity-high-symbolic")
		b.muteButton.RemoveCSSClass("voice-bar-muted")
		b.updateStatus()
	}
}

func (b *VoiceBar) updateDeafenButton(deafened bool) {
	if deafened {
		b.deafenButton.SetIconName("audio-volume-muted-symbolic")
		b.deafenButton.AddCSSClass("voice-bar-muted")
		b.updateStatus()
	} else {
		b.deafenButton.SetIconName("audio-volume-high-symbolic")
		b.deafenButton.RemoveCSSClass("voice-bar-muted")
		b.updateStatus()
	}
}

func (b *VoiceBar) updateStatus() {
	session := b.manager.Session()
	if session == nil {
		b.statusLabel.SetText("Voice Connected")
		return
	}

	if session.IsDeafened() {
		b.statusLabel.SetText("Deafened")
	} else if session.IsMuted() {
		b.statusLabel.SetText("Muted")
	} else {
		b.statusLabel.SetText("Voice Connected")
	}
}
