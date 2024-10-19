package fasthttp

import (
	"github.com/rs/zerolog"
	"os"
	"testing"
)

func TestLog(t *testing.T) {
	l := zerolog.New(os.Stdout)
	l.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str("key", "value")
	})
	sub1 := l.With().Logger()
	sub2 := l.With().Logger()
	sub1.Log().Str("key1", "value1").Send()
	sub2.Log().Str("key2", "value2").Send()
	l2 := zerolog.New(os.Stdout).Sample(zerolog.RandomSampler(100))
	l2.Log().Msg("test")
}
