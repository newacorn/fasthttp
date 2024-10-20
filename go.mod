module github.com/newacorn/fasthttp

go 1.22.5

toolchain go1.22.6

require (
	github.com/bytedance/gopkg v0.1.1
	github.com/gookit/goutil v0.6.17
	github.com/klauspost/compress v1.17.11
	github.com/newacorn/brotli v0.0.0-20241020004304-ec39f4a41287
	github.com/newacorn/cbrotli/go/cbrotli v0.0.0-20241020012012-8fb4aa8de81a
	github.com/newacorn/goutils v0.0.0-20241020013356-15e1e4834c16
	github.com/newacorn/simple-bytes-pool v0.0.0-20241019202108-1ca97a547e01
	github.com/pkg/errors v0.9.1
	github.com/puzpuzpuz/xsync/v3 v3.4.0
	github.com/rs/zerolog v1.33.0
	github.com/valyala/bytebufferpool v1.0.0
	github.com/valyala/fasthttp v1.55.0
	github.com/valyala/tcplisten v1.0.0
	github.com/xyproto/randomstring v1.0.5
	golang.org/x/crypto v0.25.0
	golang.org/x/net v0.27.0
	golang.org/x/sys v0.26.0
)

replace github.com/klauspost/compress v1.17.11 => github.com/newacorn/compress v0.0.0-20241020002001-be411bf4ca21

require (
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/gookit/color v1.5.4 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/text v0.18.0 // indirect
)
