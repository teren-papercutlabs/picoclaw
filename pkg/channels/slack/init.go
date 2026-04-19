package slack

import (
	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelSlack,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			c, ok := decoded.(*config.SlackSettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			return NewSlackChannel(bc, c, b)
		},
	)
}
