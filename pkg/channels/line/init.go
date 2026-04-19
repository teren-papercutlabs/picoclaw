package line

import (
	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelLINE,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			c, ok := decoded.(*config.LINESettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			return NewLINEChannel(bc, c, b)
		},
	)
}
