package fasthttp

import (
	"github.com/newacorn/goutils/unsafefn"
	"time"
)

type AbsoluteTime int64

var (
	startTimeLocal = time.Now()
	startTimeUTC   = time.Now().UTC()
)

var (
	startUnixNano   = time.Now().UnixNano()
	startUnixMicro  = time.Now().UnixMicro()
	startUnixMilli  = time.Now().UnixMilli()
	startUnixSecond = time.Now().Unix()
)

var (
	startAbsoluteNano   = unsafefn.NanoTime()
	startAbsoluteMicro  = unsafefn.NanoTime() / 1e3
	startAbsoluteMilli  = unsafefn.NanoTime() / 1e6
	startAbsoluteSecond = unsafefn.NanoTime() / 1e9
)

func absoluteToUTC(n int64) time.Time {
	return startTimeUTC.Add(time.Duration(n - startAbsoluteNano))
}

func absoluteToLocal(n int64) time.Time {
	return startTimeUTC.Add(time.Duration(n - startAbsoluteNano)).Local()
}

func absoluteNano() int64 {
	return unsafefn.NanoTime()
}
