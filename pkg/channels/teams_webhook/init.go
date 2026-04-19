package teamswebhook

import (
	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelTeamsWebHook,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			c, ok := decoded.(*config.TeamsWebhookSettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			ch, err := NewTeamsWebhookChannel(bc, c, b)
			if err != nil {
				return nil, err
			}
			if channelName != config.ChannelTeamsWebHook {
				ch.SetName(channelName)
			}
			return ch, nil
		},
	)
}
