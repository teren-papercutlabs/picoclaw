package http

import (
	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/channels"
	"github.com/teren-papercutlabs/pclaw/pkg/config"
)

func init() {
	channels.RegisterSafeFactory(config.ChannelHTTP,
		func(bc *config.Channel, settings *config.HTTPSettings, b *bus.MessageBus) (channels.Channel, error) {
			ch, err := NewHTTPChannel(settings, b)
			if err != nil {
				return nil, err
			}
			// BaseChannel name defaults to "http"; update if the config key differs.
			if bc.Name() != "" && bc.Name() != channelName {
				ch.SetName(bc.Name())
			}
			return ch, nil
		})
}
