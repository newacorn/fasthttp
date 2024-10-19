package fasthttp

import "github.com/rs/zerolog"

func init() {
	zerolog.CallerFieldName = "C"
	zerolog.MessageFieldName = "M"
	zerolog.LevelFieldName = "L"
	zerolog.ErrorFieldName = "E"
	zerolog.TimestampFieldName = "T"
	zerolog.ErrorStackFieldName = "S"
}
