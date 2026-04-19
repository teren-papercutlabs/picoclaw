package http

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
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
