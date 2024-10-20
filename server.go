package fasthttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/newacorn/goutils"
	pbufio "github.com/newacorn/goutils/bufio"
	"github.com/newacorn/goutils/compress"
	"github.com/puzpuzpuz/xsync/v3"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var ErrCertAndKeyMustProvided = errors.New("cert and key must be both provided")

// ServeConn serves HTTP requests from the given connection
// using the given handler.
//
// ServeConn returns nil if all requests from the c are successfully served.
// It returns non-nil error otherwise.
//
// Connection c must immediately propagate all the data passed to Write()
// to the client. Otherwise requests' processing may hang.
//
// ServeConn closes c before returning.
func ServeConn(c net.Conn, handler RequestHandler) (err error) {
	v := serverPool.Get()
	if v == nil {
		v = &Server{}
	}
	s := v.(*Server)
	s.Handler = handler
	err = s.ServeConn(c)
	s.Handler = nil
	serverPool.Put(v)
	return
}

var serverPool sync.Pool

// Serve serves incoming connections from the given listener
// using the given handler.
//
// Serve blocks until the given listener returns permanent error.
func Serve(ln net.Listener, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.Serve(ln)
}

// ServeTLS serves HTTPS requests from the given net.Listener
// using the given handler.
//
// certFile and keyFile are paths to TLS certificate and key files.
func ServeTLS(ln net.Listener, certFile, keyFile string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ServeTLS(ln, certFile, keyFile)
}

// ServeTLSEmbed serves HTTPS requests from the given net.Listener
// using the given handler.
//
// certData and keyData must contain valid TLS certificate and key data.
func ServeTLSEmbed(ln net.Listener, certData, keyData []byte, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ServeTLSEmbed(ln, certData, keyData)
}

// ListenAndServe serves HTTP requests from the given TCP addr
// using the given handler.
func ListenAndServe(addr string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServe(addr)
}

// ListenAndServeUNIX serves HTTP requests from the given UNIX addr
// using the given handler.
//
// The function deletes existing file at addr before starting serving.
//
// The server sets the given file mode for the UNIX addr.
func ListenAndServeUNIX(addr string, mode os.FileMode, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServeUNIX(addr, mode)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr
// using the given handler.
//
// certFile and keyFile are paths to TLS certificate and key files.
func ListenAndServeTLS(addr, certFile, keyFile string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServeTLS(addr, certFile, keyFile)
}

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP addr
// using the given handler.
//
// certData and keyData must contain valid TLS certificate and key data.
func ListenAndServeTLSEmbed(addr string, certData, keyData []byte, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServeTLSEmbed(addr, certData, keyData)
}

// RequestHandler must process incoming requests.
//
// RequestHandler must call ctx.TimeoutError() before returning
// if it keeps references to ctx and/or its members after the return.
// Consider wrapping RequestHandler into TimeoutHandler if response time
// must be limited.
type RequestHandler func(ctx *RequestCtx)

// ServeHandler must process tls.Config.NextProto negotiated requests.
type ServeHandler func(c net.Conn) error

// Server implements HTTP server.
//
// Default Server settings should satisfy the majority of Server users.
// Adjust Server settings only if you really understand the consequences.
//
// It is forbidden copying Server instances. Create new Server instances
// instead.
//
// It is safe to call Server methods from concurrently running goroutines.
type Server struct {
	noCopy noCopy

	// Handler for processing incoming requests.
	//
	// Take into account that no `panic` recovery is done by `fasthttp` (thus any `panic` will take down the entire server).
	// Instead, the user should use `recover` to handle these situations.
	Handler RequestHandler

	// ErrorHandler for Errors encountered while receiving requests, parsing requests, and sending responses
	// may sometimes make it impossible to determine the connection status or the
	// state of data consumption.
	//
	// The following is a non-exhaustive list of errors that can be expected as argument:
	//
	//   * io.ErrUnexpectedEOF
	//   * ErrGetOnly
	//   * HeaderBufferSmallErr
	//   * ErrUnexpectedReqBodyEOF
	//   * ErrUnexpectedRespBodyEOF
	//   * ErrBodyTooLarge
	//   * ErrBrokenChunk
	//   * HeaderParseErr
	// There are also some network errors such as timeouts, connection resets, and unexpected disconnections.
	ErrorHandler func(ctx *RequestCtx, err error)

	// HeaderReceived is called after receiving the header.
	//
	// Non-zero RequestConfig field values will overwrite the default configs
	// This function is called after reading the request headers and before reading the request body.
	HeaderReceived func(header *RequestHeader) RequestConfig

	// ContinueHandler is called after receiving the Expect 100 Continue Header.
	//
	// https://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html#sec8.2.3
	// https://www.w3.org/Protocols/rfc2616/rfc2616-sec10.html#sec10.1.1
	// Using ContinueHandler a server can make decisioning on whether
	// to read a potentially large request body based on the headers.
	//
	// If this field is not set or its value returns true after being called,
	// a 100 Continue response will be automatically sent upon receiving an
	// Expect 100 Continue request, and then it will block while waiting
	// for the request body to arrive.
	//
	// If return false sending  417 Expectation Failed and close connection.
	ContinueHandler func(header *RequestHeader) bool

	// Server name for sending in response headers. default value is fasthttp
	//
	// The Server header is determined by the following priority:
	// the value set in RequestHeader, the value of this field, and the global default value.
	Name string

	// The maximum number of concurrent connections the server may serve.
	// Does not include links that have already been hijacked
	//
	// DefaultConcurrency is used if not set. Default value is  256 * 1024.
	//
	// Concurrency only works if you either call Serve once, or only ServeConn multiple times.
	// It works with ListenAndServe as well.
	// A zero or negative value indicates the default value.
	Concurrency int

	// Per-connection buffer size for requests' reading.
	// This also limits the maximum header size.
	//
	// Default buffer size is used if not set. Default value is 4096.
	//
	// Since the `readRawHeaders` function first reads the entire request header into the
	// `RequestHeader.rawHeaders` field before parsing it line by line, this reading
	// process initially buffers the entire header into the `bufio.Reader`'s buffer.
	// The size of this buffer is determined by the `ReadBufferSize` field.
	//
	// If the value exceeds this field, the server will send a 413 response.
	// 431 Request Header Fields Too Large.
	// A zero or negative value indicates the default value.
	ReadBufferSize int

	// Per-connection buffer size for responses' writing.
	//
	// Default buffer size is used if not set. Default value is 4096.
	//
	// The response is typically buffered up to the size specified by the `WriteBufferSize`.
	// It is only when the buffer is full or a flush is triggered that the data is sent to
	// the underlying connection.
	//
	// It does not affect the correctness of the program. A larger buffer can reduce the number of system calls.
	// A zero or negative value indicates the default value.
	WriteBufferSize int

	// ReadTimeout is the amount of time allowed to read
	// the full request's Header and Body.
	//
	// If the `HeaderReceived` field is also set with `ReadTimeout`, this field will only
	// apply to the reading of the request headers.
	//
	// By default, request read timeout is unlimited.
	//
	// When a connection is set to be hijacked, any timeouts related to the hijacking will be reset
	// after RequestHandler return.
	// A zero or negative value indicates no limit.
	ReadTimeout time.Duration

	// WriteTimeout The timeout for the completion of the response begins counting from the moment
	// the `RequestHandler` returns.
	//
	// By default, response write timeout is unlimited.
	// When a connection is set to be hijacked, any timeouts related to the hijacking will be reset
	// after RequestHandler return.
	// A zero or negative value indicates no limit.
	WriteTimeout time.Duration

	// IdleTimeout The timeout starts immediately after the previous request is completed and
	// continues until the first byte of the next request is read.
	// This field is only effective when the `TCPKeepalive` field is set to `true`.
	// A zero or negative value indicates no limit.
	IdleTimeout time.Duration

	// Maximum number of concurrent client connections allowed per IP.
	//
	// By default, unlimited number.
	// A zero or negative value indicates no limit.
	MaxConnsPerIP int
	// MaxIdleWorkerDuration is the maximum idle time of a single worker in the underlying
	// worker pool of the Server. Idle workers beyond this time will be cleared.
	// The resources associated with it are just a channel and a goroutine.
	// An idle worker means a worker that does not have any connections currently being served.
	MaxIdleWorkerDuration time.Duration

	// The return value of this field determines how many of the idle workers in the list will be removed.
	CleanThreshold CleanThresholdFunc

	// TCPKeepalivePeriod is the duration the connection needs to
	// remain idle before TCP starts sending keepalive probes.
	//
	// This field is only effective when TCPKeepalive is set to true.
	// A zero value indicates that the operating system's default setting will be used.
	TCPKeepalivePeriod time.Duration

	// Maximum request body size.
	// The size does not include the request headers.
	// If the value exceeds this field, the server will send a 431 response
	// and close connection.
	// Request body size is limited by DefaultMaxRequestBodySize(4MB) by default.
	// A zero or negative value indicates the default value.
	MaxRequestBodySize int64

	// Determines whether a connection can only serve a single request.
	// If this field is set to `true`, it will send `Connection: close` and
	// close the connection after sending the response.
	DisableKeepalive bool

	// Whether the operating system should send tcp keep-alive messages on the tcp connection.
	//
	// By default, tcp keep-alive connections are disabled.
	//
	// This option is used only if default TCP dialer is used,
	// i.e. if Dial and DialTimeout are blank.
	TCPKeepalive bool

	// Aggressively reduces memory usage at the cost of higher CPU usage
	// if set to true.
	//
	// Try enabling this option only if the server consumes too much memory
	// serving mostly idle keep-alive connections. This may reduce memory
	// usage by more than 50%.
	//
	// Aggressive memory usage reduction is disabled by default.
	//
	// Release the read buffer immediately after completing request reading,
	// and release the write buffer after completing response writing.
	// Release the `RequestCtx` before reading the first byte of the next request.
	ReduceMemoryUsage bool

	// Rejects all non-GET requests if set to true.
	//
	// This option is useful as anti-DoS protection for servers
	// accepting only GET requests and HEAD requests.
	//
	// Server accepts all the requests by default.
	GetOnly bool

	// Will not pre parse Multipart Form data if set to true.
	//
	// This option is useful for servers that desire to treat
	// multipart form data as a binary blob, or choose when to parse the data.
	//
	// Server pre parses multipart form data by default.
	DisablePreParseMultipartForm bool

	// Logs all errors, including the most frequent
	// 'connection reset by peer', 'broken pipe' and 'connection timeout'
	// errors. Such errors are common in production serving real-world
	// clients.
	//
	// By default, the most frequent errors such as
	// 'connection reset by peer', 'broken pipe' and 'connection timeout'
	// are suppressed in order to limit output log traffic.
	LogAllErrors bool

	// Will not log potentially sensitive content in error logs
	//
	// This option is useful for servers that handle sensitive data
	// in the request/response.
	//
	// Server logs all full errors by default.
	// Deprecated
	SecureErrorLogMessage bool

	// Regardless of how this field is set, the header names are always
	// formatted during HTTP header parsing.
	// This field only affects the response headers.
	//
	// Header names are passed as-is without normalization
	// if this option is set.
	//
	// Disabled header names' normalization may be useful only for proxying
	// incoming requests to other servers expecting case-sensitive
	// header names. See https://github.com/valyala/fasthttp/issues/57
	// for details.
	//
	// By default, request and response header names are normalized, i.e.
	// The first letter and the first letters following dashes
	// are uppercased, while all the other letters are lowercased.
	// Examples:
	//
	//     * HOST -> Host
	//     * content-type -> Content-Type
	//     * cONTENT-lenGTH -> Content-Length
	//
	// In the `Server.serveConn` method, set the `RequestHeader.
	// disableNormalizing` and `ResponseHeader.disableNormalizing` fields
	// each time a new request arrives.
	DisableHeaderNamesNormalizing bool

	// SleepWhenConcurrencyLimitsExceeded is a duration to be slept of if
	// the concurrency limit in exceeded (default [when is 0]: don't sleep
	// and accept new connections immediately).
	//
	// This field mainly prevents the scenario where, due to concurrent
	// connections exceeding the limit, new connections are immediately
	// closed because they cannot be serviced even if they start to be received.
	SleepWhenConcurrencyLimitsExceeded time.Duration

	// NoDefaultServerHeader, when set to true, causes the default Server header
	// to be excluded from the Response, When the `ResponseHeader` does not have
	// the `Server` header set and the `Name` field is also not set.
	NoDefaultServerHeader bool

	// NoDefaultDate, when set to true, causes the default Date
	// header to be excluded from the Response.
	//
	// The default Date header value is the current date value. When
	// set to true, the Date will not be present.
	//
	// For optimization purposes, internal time is cached, so even
	// if the response sets its own `Date` header, it will be ignored.
	NoDefaultDate bool

	// NoDefaultContentType, When the `Content-Type` header is not set
	// in the `ResponseHeader`, should the default value be used or not?
	// Default value is text/plain; charset=utf-8
	NoDefaultContentType bool

	// KeepHijackedConns After a connection is hijacked,
	// should the lifecycle of the connection continue to be managed by Server?
	KeepHijackedConns bool

	// CloseOnShutdown when true adds a `Connection: close` header when the server is shutting down.
	CloseOnShutdown bool

	// StreamRequestBody enables request body streaming,
	// Unless it is Chunked encoding, a portion of the request body will still
	// be pre-read before calling the handler.
	//
	// When `StreamRequestBody` is set to `true`, the size of the request body
	// is not limited by the `MaxRequestBodySize` field value.
	StreamRequestBody bool
	// When closing the request stream, is the unread content
	// read into `io.Discard` to avoid interfering with the reading of the next request?
	// In `net/http`, after the `Handler` returns, it always attempts to drain any unread request body.
	DiscardUnReadRequestBodyStream bool

	// ConnState specifies an optional callback function that is
	// called when a client connection changes state. See the
	// ConnState type and associated constants for details.
	//
	// StateNew StateClosed StateHijacked StateIdle StateActiveR
	// StateActiveW
	// Setting these statuses will all invoke this field's value.
	ConnState func(net.Conn, ConnState)

	// Logger, which is used by RequestCtx.Logger().
	//
	// By default standard logger from log package is used.
	Logger Logger

	// TLSConfig optionally provides a TLS configuration for use
	// by ServeTLS, ServeTLSEmbed, ListenAndServeTLS, ListenAndServeTLSEmbed,
	// AppendCert, AppendCertEmbed and NextProto.
	//
	// Note that this value is cloned by ServeTLS, ServeTLSEmbed, ListenAndServeTLS
	// and ListenAndServeTLSEmbed, so it's not possible to modify the configuration
	// with methods like tls.Config.SetSessionTicketKeys.
	// To use SetSessionTicketKeys, use Server.Serve with a custom TLS Listener
	// instead.
	TLSConfig *tls.Config
	// This field value will be invoked when the concurrency exceeds the
	// limit or the `perIPConnCounter` exceeds the limit.
	//
	// The returned string will be immediately written to the client as the response,
	// and it must include both the response headers and body. The connection will
	// be closed after the response is sent.
	//
	// If this field is omitted, a 503 status code will be sent for the `MaxConCurrencyLimit`
	// limit, and a 429 status code will be sent for the `MaxConPerIpLimit` limit. The response
	// body will contain the corresponding status information.
	ResourceLimitError func(lt ResourceLimitType) (body string)

	// During the TLS handshake, if there is protocol information, it will be looked up
	// in the mapping represented by this field, and if a corresponding handler is found,
	// it will be called.
	//
	// Timeout settings associated with the connection will be cleared before calling
	// `ServerHandler`. After that, the connection will not be processed until the
	// `ServerHandler` call returns.
	nextProtos map[string]ServeHandler

	// The capacity of this channel is set to the value of the Concurrency field, and it
	// serves as a semaphore to limit the number of connections being handled concurrently.
	// only used in ServeConn method.
	//concurrencyCh chan struct{}

	// It operates according to the configuration set by the MaxConnsPerIP field.
	// IP-to-request-count mapping container
	perIPConnCounter perIPConnCounter

	// `RequestCtx` object pool
	// Acquire `RequestCtx` before handling the request and release it after
	// the `for` loop that processes the request ends.
	ctxPool sync.Pool // RequestCtx

	// When parsing a request, the data in the connection is first read into a `bufio.Reader`.
	// This field serves as a buffer pool for `bufio.Reader` objects.
	readerPool sync.Pool // bufio.Reader

	// When sending a response, the response body is first written to a `bufio.Writer` and
	// then flushed to the underlying connection. This field is used as a buffer pool for
	// bufio.Writer objects.
	writerPool sync.Pool // bufio.Writer

	// hijackConn object pool
	hijackConnPool sync.Pool

	// We need to know our listeners and idle connections so we can close them in Shutdown().
	// The `net.Listener` object associated with this service instance.
	ln []net.Listener // Shutdown must close all listener it had seen and their accept connections.

	// This field is used to maintain connections information.
	// Added after `Accept` and removed after `Close` and `Hijacked`.
	conns xsync.MapOf[net.Conn, *ConnStatus]

	// This field maintains modifications to certain fields of the service instance.
	mu sync.Mutex
	// The number of connections opened by this service, including `Listener` connections.
	// The `Shutdown` function will only return when the value of this field becomes 0
	// or timeout.
	open int32
	// Is the server in the shutdown phase?
	//
	// When this field is set to `true`, connections being served will not
	// continue to handle the next connection after completing the current request,
	stop int32
	// When `ShutDown` is called, close all `net.Listener` objects first,
	// and then close the channel represented by this field.
	// Set ResponseHeader's Connection to close.
	done chan struct{}

	// The number of connections rejected due to the `Concurrency` field value limit
	rejectedByConcurrencyLimitCount uint32
	// The number of connections rejected due to the `Concurrency` field value limit
	rejectedByPerIpLimitCount uint32
	// Maintain the initialization of the `conns` field to avoid duplicate
	// initialization or use before initialization.
	connsInit uint32

	// This field will be used to count the number of connections currently being served.
	concurrency uint32
}

// ConnStatus is used to store the status of a connection throughout its entire
// lifecycle.
//
// conns status may is one of StateNew StateIdle StateClosed StateActiveR StateActiveW StateHijacked
// at some a time point.
type ConnStatus struct {
	// status is one of StateIdle StateClosed StateActiveR StateActiveW StateHijacked
	//
	// After the connection is accepted, this field is set to `StateIdle`,
	// but `lastActive` is set to the current time plus 5 seconds.
	status      atomic.Int32
	closeReason atomic.Int32
	// The time is set to the current time each time the first byte of a request is read.
	lastActive atomic.Int64
	// The time when the connection is accepted.
	createTime int64
}

// ResourceLimitType Used to represent the type of server resource limitation.
// Currently, it includes two types: the maximum number of simultaneous connections
// the server can handle and the maximum number of connections that can be maintained
// simultaneously per IP.
type ResourceLimitType int

const (
	// MaxConCurrencyLimit The server's current number of connections has exceeded the value
	// specified by the `Server.Concurrency` field.
	MaxConCurrencyLimit ResourceLimitType = iota
	// MaxConPerIpLimit The number of simultaneous connections opened by a single IP address has
	// exceeded the value specified by the `Server.MaxConnsPerIP` field.
	MaxConPerIpLimit
)

// When server resources exceed the limit, how should the server respond to the client?
var defaultResourceLimitError = func(lt ResourceLimitType) (responseStr string) {
	if lt == MaxConCurrencyLimit {
		responseStr = concurrencyLimitErr
	} else {
		responseStr = conPerIpLimitErr
	}
	return
}

// RequestConfig configure the per request deadline and body limits.
type RequestConfig struct {
	// It has the same meaning as the `Server.ReadTimeout` field value,
	// but it is applicable to a single connection.
	// Additionally, the timeout set only applies to reading the request body.
	// Zero and negative values will be ignored.
	ReadTimeout time.Duration
	// It has the same meaning as the Server.WriteTimeout field value,
	// but it is applicable to a single connection.
	// Additionally, the timeout set only applies to reading the request body.
	// Zero and negative values will be ignored.
	WriteTimeout time.Duration
	// It has the same meaning as the `Server.MaxRequestBodySize` field value,
	// but it is applicable to a single connection.
	MaxRequestBodySize int64
}

var defaultCompressConfig = CompressConfig{
	Algs: [4]CompressAlg{Br, Gzip, Deflate, Zstd},
	Levels: [4]int8{
		CompressDefaultCompression,
		CompressDefaultCompression,
		CompressDefaultCompression,
		CompressDefaultCompression,
	},
}

// CompressHandler returns RequestHandler that transparently compresses
// response body generated by h if the request contains 'gzip', 'deflate', br or zstd
// 'Accept-Encoding' header.
func CompressHandler(h RequestHandler) RequestHandler {
	return CompressHandlerLevel(h, defaultCompressConfig)
}

type CompressAlg int8

// CompressAlg is a compression algorithm
// Zero value means no compression.
const (
	_ CompressAlg = iota
	Gzip
	Br
	Deflate
	Zstd
)

var compressAlgNames = [5]string{"gzip", "gzip", "br", "deflate", "zstd"}

// CompressConfig Mapping of compression algorithms to levels
// Support gzip, deflate, brotli, zstd
type CompressConfig struct {
	Algs   [4]CompressAlg
	Levels [4]int8
}

var CompressFuncs = [5]func(response *Response, level int){
	(*Response).gzipBody, (*Response).gzipBody, (*Response).brotliBody, (*Response).deflateBody, (*Response).zstdBody,
}

func CompressHandlerLevel(h RequestHandler, cfg CompressConfig) RequestHandler {
	return func(ctx *RequestCtx) {
		h(ctx)
		ae := ctx.Request.Header.Peek(HeaderAcceptEncoding)
		if len(ae) == 0 {
			return
		}
		for i, alg := range cfg.Algs {
			if alg < 0 || alg > 4 {
				continue
			}
			if HasAcceptEncodingBytes(ae, s2b(compressAlgNames[alg])) {
				CompressFuncs[alg](&ctx.Response, int(cfg.Levels[i]))
				return
			}

		}
	}
}

// RequestCtx contains incoming request and manages outgoing response.
//
// It is forbidden copying RequestCtx instances.
//
// RequestHandler should avoid holding references to incoming RequestCtx and/or
// its members after the return.
// If holding RequestCtx references after the return is unavoidable
// (for instance, ctx is passed to a separate goroutine and ctx lifetime cannot
// be controlled), then the RequestHandler MUST call ctx.TimeoutError()
// before return.
//
// It is unsafe modifying/reading RequestCtx instance from concurrently
// running goroutines. The only exception is TimeoutError*, which may be called
// while other goroutines accessing RequestCtx.
type RequestCtx struct {
	noCopy noCopy

	// Incoming request.
	//
	// Copying Request by value is forbidden. Use pointer to Request instead.
	Request Request

	// Outgoing response.
	//
	// Copying Response by value is forbidden. Use pointer to Response instead.
	Response Response

	// This field represents the storage container associated with the `RequestCtx` object.
	userValues userData

	// Represents an ID for a connection. Each `Server` instance shares a global incrementing
	// counter, with the first connection ID starting at 1.
	connID uint64
	// The ID for each request within a specific connection, with the first request ID starting at 1.
	connRequestNum uint64
	// The time when the connection is first processed in the `Server.serveConn` method.
	connTime time.Time
	// The IP address of the remote end of the TCP connection.
	remoteAddr net.Addr

	// The time before passing `RequestCtx` to the handler, at which point the request has already been parsed.
	time time.Time

	// The `ctxLogger.logger` field inherits the value from the `Server.Logger` field.
	// ctxLogger.logger
	//
	// The value of `ctxLogger.ctx` cannot be configured and always points
	// to the `RequestCtx` instance to which `ctxLogger` belongs.
	logger ctxLogger
	// The connection associated with `RequestCtx` originates from this server.
	s *Server
	// The underlying connection associated with `RequestCtx`.
	c net.Conn

	// If this field is set, the response content will use this field instead of
	// the `Response` field after the `Handler` returns.
	//
	// After setting this field in `Ctx` and after the `Handler` function returns,
	// it is safe to access any fields other than this one.
	//timeoutResponse *Response
	// When the timeout series functions are called, this field value is set
	// and cached in `RequestCtx`. When the `Reset` method is called, this timer will be stopped.
	timeoutTimer *time.Timer
	// Hijack the `net.Conn` connection. After the `Handler` function returns,
	// the value of this field will be invoked in a separate goroutine with the
	// underlying `net.Conn` object as the parameter.
	hijackHandler HijackHandler

	// After setting this field, when the handler returns and the response is sent, this field
	// will be called. The content written internally will be sent using Chunked encoding.
	//
	// After setting this field, you do not need to set the `Transfer-Encoding` header yourself.
	w func(w WriterFlusherCloser) error
	// When the `Server.SaveMemoryUsage` field is set, this field will be used as
	// a temporary replacement for the `bufio.Reader` object.
	// Used for reading the first byte of the request. After reading,
	// the `bufio.Reader` object is retrieved again.
	fbr firstByteReader
	bw  *bufio.Writer
	// This field is only effective if the `hijackHandler` field is set. It controls whether the response
	// in `Response` is sent to the client instead of being handled by the hijack function.
	hijackNoResponse bool
	// If this field is set to `true`, the values of `hijackHandler` and `hijackNoResponse` will be ignored.
	// You need to copy the required resources within the handler, as only the `net.Conn` can be retained
	// after the handler returns and
	// resources associated with `RequestCtx` will be reclaimed.
	//
	// The response in the `Response` field will not be sent if exits.
	// So, in the handler, you should not set any response-related content through `RequestCtx.Response`.
	// Instead, you need to send the response directly through `net.Conn`.
	//
	// When this field is set to `true`, `Server.KeepHijackedConns` is automatically set to `true`.
	// After setting this field to `true`, it is safe to use the `net.Conn` object returned
	// by the `RequestCtx.Conn` method.
	//
	// The server will not interfere with the state of the hijacked `net.Conn` after hijacking.
	hijacked bool
	isTLS    bool
}

// HijackHandler must process the hijacked connection c.
//
// If KeepHijackedConns is disabled, which is by default,
// the connection c is automatically closed after returning from HijackHandler.
//
// The connection c must not be used after returning from the handler, if KeepHijackedConns is disabled.
//
// When KeepHijackedConns enabled, fasthttp will not Close() the connection,
// you must do it when you need it. You must not use c in any way after calling Close().
type HijackHandler func(c net.Conn)

// Hijack registers the given handler for connection hijacking.
//
// The handler is called after returning from RequestHandler
// and sending http response. The current connection is passed
// to the handler. The connection is automatically closed after
// returning from the handler.
//
// The server skips calling the handler in the following cases:
//
//   - 'Connection: close' header exists in either request or response.
//   - Unexpected error during response writing to the connection.
//
// The server stops processing requests from hijacked connections.
//
// Server limits such as Concurrency, ReadTimeout, WriteTimeout, etc.
// aren't applied to hijacked connections.
//
// The handler must not retain references to ctx members.
//
// Arbitrary 'Connection: Upgrade' protocols may be implemented
// with HijackHandler. For instance,
//
//   - WebSocket ( https://en.wikipedia.org/wiki/WebSocket )
//   - HTTP/2.0 ( https://en.wikipedia.org/wiki/HTTP/2 )
func (ctx *RequestCtx) Hijack(handler HijackHandler) {
	ctx.hijackHandler = handler
}

// HijackSetNoResponse changes the behavior of hijacking a request.
// If HijackSetNoResponse is called with false fasthttp will send a response
// to the client before calling the HijackHandler (default). If HijackSetNoResponse
// is called with true no response is sent back before calling the
// HijackHandler supplied in the Hijack function.
func (ctx *RequestCtx) HijackSetNoResponse(noResponse bool) {
	ctx.hijackNoResponse = noResponse
}

// Hijacked returns true after Hijack is called.
func (ctx *RequestCtx) Hijacked() bool {
	return ctx.hijackHandler != nil
}

// SetUserValue stores the given value (arbitrary object)
// under the given key in ctx.
//
// The value stored in ctx may be obtained by UserValue*.
//
// This functionality may be useful for passing arbitrary values between
// functions involved in request processing.
//
// All the values are removed from ctx after returning from the top
// RequestHandler. Additionally, Close method is called on each value
// implementing io.Closer before removing the value from ctx.
func (ctx *RequestCtx) SetUserValue(key, value any) {
	ctx.userValues.Set(key, value)
}

func (ctx *RequestCtx) HijackConn() {
	ctx.hijacked = true
	return
}

// SetUserValueBytes stores the given value (arbitrary object)
// under the given key in ctx.
//
// The value stored in ctx may be obtained by UserValue*.
//
// This functionality may be useful for passing arbitrary values between
// functions involved in request processing.
//
// All the values stored in ctx are deleted after returning from RequestHandler.
func (ctx *RequestCtx) SetUserValueBytes(key []byte, value any) {
	ctx.userValues.SetBytes(key, value)
}

// UserValue returns the value stored via SetUserValue* under the given key.
func (ctx *RequestCtx) UserValue(key any) any {
	return ctx.userValues.Get(key)
}

// UserValueBytes returns the value stored via SetUserValue*
// under the given key.
func (ctx *RequestCtx) UserValueBytes(key []byte) any {
	return ctx.userValues.GetBytes(key)
}

// VisitUserValues calls visitor for each existing userValue with a key that is a string or []byte.
//
// visitor must not retain references to key and value after returning.
// Make key and/or value copies if you need storing them after returning.
func (ctx *RequestCtx) VisitUserValues(visitor func([]byte, any)) {
	for i, n := 0, len(ctx.userValues); i < n; i++ {
		kv := &ctx.userValues[i]
		if _, ok := kv.key.(string); ok {
			visitor(s2b(kv.key.(string)), kv.value)
		}
	}
}

// VisitUserValuesAll calls visitor for each existing userValue.
//
// visitor must not retain references to key and value after returning.
// Make key and/or value copies if you need storing them after returning.
func (ctx *RequestCtx) VisitUserValuesAll(visitor func(any, any)) {
	for i, n := 0, len(ctx.userValues); i < n; i++ {
		kv := &ctx.userValues[i]
		visitor(kv.key, kv.value)
	}
}

// ResetUserValues allows to reset user values from Request Context.
func (ctx *RequestCtx) ResetUserValues() {
	ctx.userValues.Reset()
}

// RemoveUserValue removes the given key and the value under it in ctx.
func (ctx *RequestCtx) RemoveUserValue(key any) {
	ctx.userValues.Remove(key)
}

// RemoveUserValueBytes removes the given key and the value under it in ctx.
func (ctx *RequestCtx) RemoveUserValueBytes(key []byte) {
	ctx.userValues.RemoveBytes(key)
}

type connTLSer interface {
	Handshake() error
	ConnectionState() tls.ConnectionState
}

// IsTLS returns true if the underlying connection is tls.Conn.
//
// tls.Conn is an encrypted connection (aka SSL, HTTPS).
func (ctx *RequestCtx) IsTLS() (ok bool) {
	return ctx.isTLS
}

// TLSConnectionState returns TLS connection state.
//
// The function returns nil if the underlying connection isn't tls.Conn.
//
// The returned state may be used for verifying TLS version, client certificates,
// etc.
func (ctx *RequestCtx) TLSConnectionState() *tls.ConnectionState {
	tlsConn, ok := ctx.c.(connTLSer)
	if !ok {
		return nil
	}
	state := tlsConn.ConnectionState()
	return &state
}

// Conn returns a reference to the underlying net.Conn.
//
// WARNING: Only use this method if you know what you are doing!
//
// Reading from or writing to the returned connection will end badly!
func (ctx *RequestCtx) Conn() net.Conn {
	return ctx.c
}

func (ctx *RequestCtx) reset() {
	ctx.userValues.Reset()
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.fbr.reset()

	ctx.connID = 0
	ctx.connRequestNum = 0
	ctx.connTime = zeroTime
	ctx.remoteAddr = nil
	ctx.time = zeroTime
	ctx.c = nil
	ctx.isTLS = false

	// Don't reset ctx.s!
	// We have a pool per server so the next time this ctx is used it
	// will be assigned the same value again.
	// ctx might still be in use for context.Done() and context.Err()
	// which are safe to use as they only use ctx.s and no other value.
	if ctx.timeoutTimer != nil {
		stopTimer(ctx.timeoutTimer)
	}

	ctx.hijackHandler = nil
	ctx.hijackNoResponse = false
	ctx.hijacked = false
}

// When `Server.SaveMemoryUsage` is set to `true`, this type is used to
// read the first byte from the connection.
// This allows for temporarily releasing the `bufio.Reader`.
type firstByteReader struct {
	c        net.Conn
	ch       byte
	byteRead bool
}

func (r *firstByteReader) reset() {
	r.c = nil
	r.ch = 0
	r.byteRead = false
}

func (r *firstByteReader) Read(b []byte) (int, error) {
	nn := 0
	if !r.byteRead {
		b[0] = r.ch
		b = b[1:]
		r.byteRead = true
		if len(b) == 0 {
			return 1, nil
		}
		nn = 1
	}
	n, err := r.c.Read(b)
	return n + nn, err
}

// Logger is used for logging formatted messages.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...any)
	Write(b []byte)
}

var ctxLoggerLock sync.Mutex

// The `ctxLogger.logger` field inherits the value from the `Server.Logger` field.
// ctxLogger.logger
//
// The value of `ctxLogger.ctx` cannot be configured and always points
// to the `RequestCtx` instance to which `ctxLogger` belongs.
type ctxLogger struct {
	ctx    *RequestCtx
	logger Logger
}

func (cl *ctxLogger) Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ctxLoggerLock.Lock()
	cl.logger.Printf("%.3f %s - %s", time.Since(cl.ctx.ConnTime()).Seconds(), cl.ctx.String(), msg)
	ctxLoggerLock.Unlock()
}
func (cl *ctxLogger) Write(b []byte) {
	cl.Printf(b2s(b))
}

var zeroTCPAddr = &net.TCPAddr{
	IP: net.IPv4zero,
}

// String returns unique string representation of the ctx.
//
// The returned value may be useful for logging.
func (ctx *RequestCtx) String() string {
	return fmt.Sprintf("#%016X - %s<->%s - %s %s", ctx.ID(), ctx.LocalAddr(), ctx.RemoteAddr(),
		ctx.Request.Header.Method(), ctx.URI().FullURI())
}

// ID returns unique ID of the request.
func (ctx *RequestCtx) ID() uint64 {
	return (ctx.connID << 32) | ctx.connRequestNum
}

// ConnID returns unique connection ID.
//
// This ID may be used to match distinct requests to the same incoming
// connection.
func (ctx *RequestCtx) ConnID() uint64 {
	return ctx.connID
}

// Time returns RequestHandler call time.
func (ctx *RequestCtx) Time() time.Time {
	return ctx.time
}

// ConnTime returns the time the server started serving the connection
// the current request came from.
func (ctx *RequestCtx) ConnTime() time.Time {
	return ctx.connTime
}

// ConnRequestNum returns request sequence number
// for the current connection.
//
// Sequence starts with 1.
func (ctx *RequestCtx) ConnRequestNum() uint64 {
	return ctx.connRequestNum
}

// SetConnectionClose sets 'Connection: close' response header and closes
// connection after the RequestHandler returns.
func (ctx *RequestCtx) SetConnectionClose() {
	ctx.Response.SetConnectionClose()
}

// SetStatusCode sets response status code.
func (ctx *RequestCtx) SetStatusCode(statusCode int) {
	ctx.Response.SetStatusCode(statusCode)
}

// SetContentType sets response Content-Type.
func (ctx *RequestCtx) SetContentType(contentType string) {
	ctx.Response.Header.SetContentType(contentType)
}

// SetContentTypeBytes sets response Content-Type.
//
// It is safe modifying contentType buffer after function return.
func (ctx *RequestCtx) SetContentTypeBytes(contentType []byte) {
	ctx.Response.Header.SetContentTypeBytes(contentType)
}

// RequestURI returns RequestURI.
//
// The returned bytes are valid until your request handler returns.
func (ctx *RequestCtx) RequestURI() []byte {
	return ctx.Request.Header.RequestURI()
}

// URI returns requested uri.
//
// This uri is valid until your request handler returns.
func (ctx *RequestCtx) URI() *URI {
	return ctx.Request.URI()
}

// Referer returns request referer.
//
// The returned bytes are valid until your request handler returns.
func (ctx *RequestCtx) Referer() []byte {
	return ctx.Request.Header.Referer()
}

// UserAgent returns User-Agent header value from the request.
//
// The returned bytes are valid until your request handler returns.
func (ctx *RequestCtx) UserAgent() []byte {
	return ctx.Request.Header.UserAgent()
}

// Path returns requested path.
//
// The returned bytes are valid until your request handler returns.
func (ctx *RequestCtx) Path() []byte {
	return ctx.URI().Path()
}

// Host returns requested host.
//
// The returned bytes are valid until your request handler returns.
func (ctx *RequestCtx) Host() []byte {
	return ctx.URI().Host()
}

// QueryArgs returns query arguments from RequestURI.
//
// It doesn't return Posted arguments - use PostArgs() for this.
//
// See also PostArgs, FormValue and FormFile.
//
// These args are valid until your request handler returns.
func (ctx *RequestCtx) QueryArgs() *Args {
	return ctx.URI().QueryArgs()
}

// PostArgs returns POST arguments.
//
// It doesn't return query arguments from RequestURI - use QueryArgs for this.
//
// See also QueryArgs, FormValue and FormFile.
//
// These args are valid until your request handler returns.
func (ctx *RequestCtx) PostArgs() *Args {
	return ctx.Request.PostArgs()
}

// MultipartForm returns request's multipart form.
//
// Returns ErrNoMultipartForm if request's content-type
// isn't 'multipart/form-data'.
//
// All uploaded temporary files are automatically deleted after
// returning from RequestHandler. Either move or copy uploaded files
// into new place if you want retaining them.
//
// Use SaveMultipartFile function for permanently saving uploaded file.
//
// The returned form is valid until your request handler returns.
//
// See also FormFile and FormValue.
func (ctx *RequestCtx) MultipartForm() (*multipart.Form, error) {
	return ctx.Request.MultipartForm()
}

// FormFile returns uploaded file associated with the given multipart form key.
//
// The file is automatically deleted after returning from RequestHandler,
// so either move or copy uploaded file into new place if you want retaining it.
//
// Use SaveMultipartFile function for permanently saving uploaded file.
//
// The returned file header is valid until your request handler returns.
func (ctx *RequestCtx) FormFile(key string) (fh *multipart.FileHeader, err error) {
	mf, err := ctx.MultipartForm()
	if mf == nil || mf.File == nil {
		return
	}
	fhs := mf.File[key]
	if fhs == nil {
		return
	}
	fh = fhs[0]
	return
}

// SaveMultipartFile saves multipart file fh under the given filename path.
func SaveMultipartFile(fh *multipart.FileHeader, path string) (err error) {
	var (
		f  multipart.File
		ff *os.File
	)
	f, err = fh.Open()
	if err != nil {
		return
	}

	var ok bool
	if ff, ok = f.(*os.File); ok {
		// Windows can't rename files that are opened.
		if err = f.Close(); err != nil {
			return
		}

		// If renaming fails we try the normal copying method.
		// Renaming could fail if the files are on different devices.
		if os.Rename(ff.Name(), path) == nil {
			return
		}

		// Reopen f for the code below.
		if f, err = fh.Open(); err != nil {
			return
		}
	}

	if ff, err = os.Create(path); err != nil {
		//goland:noinspection GoUnhandledErrorResult
		f.Close()
		return
	}
	_, err = copyZeroAlloc(ff, f)
	//
	er1 := f.Close()
	er2 := ff.Close()
	if err == nil {
		err = er1
		if err == nil {
			err = er2
		}
	}
	return
}

// FormValue returns form value associated with the given key.
//
// The value is searched in the following places:
//
//   - Query string.
//   - POST or PUT body.
//
// There are more fine-grained methods for obtaining form values:
//
//   - QueryArgs for obtaining values from query string.
//   - PostArgs for obtaining values from POST or PUT body.
//   - MultipartForm for obtaining values from multipart form.
//   - FormFile for obtaining uploaded files.
//
// The returned value is valid until your request handler returns.
func (ctx *RequestCtx) FormValue(key string) (v []byte) {
	v = ctx.PostArgs().Peek(key)
	if len(v) > 0 {
		return
	}
	mf, _ := ctx.MultipartForm()
	if mf != nil && mf.Value != nil {
		vv := mf.Value[key]
		if len(vv) > 0 {
			v = s2b(vv[0])
			return
		}
	}
	v = ctx.QueryArgs().Peek(key)
	return
}

type FormValueFunc func(*RequestCtx, string) []byte

// IsGet returns true if request method is GET.
func (ctx *RequestCtx) IsGet() bool {
	return ctx.Request.Header.IsGet()
}

// IsPost returns true if request method is POST.
func (ctx *RequestCtx) IsPost() bool {
	return ctx.Request.Header.IsPost()
}

// IsPut returns true if request method is PUT.
func (ctx *RequestCtx) IsPut() bool {
	return ctx.Request.Header.IsPut()
}

// IsDelete returns true if request method is DELETE.
func (ctx *RequestCtx) IsDelete() bool {
	return ctx.Request.Header.IsDelete()
}

// IsConnect returns true if request method is CONNECT.
func (ctx *RequestCtx) IsConnect() bool {
	return ctx.Request.Header.IsConnect()
}

// IsOptions returns true if request method is OPTIONS.
func (ctx *RequestCtx) IsOptions() bool {
	return ctx.Request.Header.IsOptions()
}

// IsTrace returns true if request method is TRACE.
func (ctx *RequestCtx) IsTrace() bool {
	return ctx.Request.Header.IsTrace()
}

// IsPatch returns true if request method is PATCH.
func (ctx *RequestCtx) IsPatch() bool {
	return ctx.Request.Header.IsPatch()
}

// IsHead returns true if request method is HEAD.
func (ctx *RequestCtx) IsHead() bool {
	return ctx.Request.Header.IsHead()
}

// Method return request method.
//
// Returned value is valid until your request handler returns.
func (ctx *RequestCtx) Method() []byte {
	return ctx.Request.Header.Method()
}

// RemoteAddr returns client address for the given request.
//
// Always returns non-nil result.
func (ctx *RequestCtx) RemoteAddr() net.Addr {
	if ctx.remoteAddr != nil {
		return ctx.remoteAddr
	}
	if ctx.c == nil {
		return zeroTCPAddr
	}
	addr := ctx.c.RemoteAddr()
	if addr == nil {
		return zeroTCPAddr
	}
	return addr
}

// SetRemoteAddr sets remote address to the given value.
//
// Set nil value to restore default behaviour for using
// connection remote address.
func (ctx *RequestCtx) SetRemoteAddr(remoteAddr net.Addr) {
	ctx.remoteAddr = remoteAddr
}

// LocalAddr returns server address for the given request.
//
// Always returns non-nil result.
func (ctx *RequestCtx) LocalAddr() net.Addr {
	if ctx.c == nil {
		return zeroTCPAddr
	}
	addr := ctx.c.LocalAddr()
	if addr == nil {
		return zeroTCPAddr
	}
	return addr
}

// RemoteIP returns the client ip the request came from.
//
// Always returns non-nil result.
func (ctx *RequestCtx) RemoteIP() net.IP {
	return addrToIP(ctx.RemoteAddr())
}

// LocalIP returns the server ip the request came to.
//
// Always returns non-nil result.
func (ctx *RequestCtx) LocalIP() net.IP {
	return addrToIP(ctx.LocalAddr())
}

func addrToIP(addr net.Addr) net.IP {
	x, ok := addr.(*net.TCPAddr)
	if !ok {
		return net.IPv4zero
	}
	return x.IP
}

// Error sets response status code to the given value and sets response body
// to the given message.
//
// Warning: this will reset the response headers and body already set!
func (ctx *RequestCtx) Error(msg string, statusCode int) {
	//ctx.Response.Reset()
	ctx.Response.resetForKeepAlive()
	ctx.SetStatusCode(statusCode)
	ctx.SetContentTypeBytes(defaultContentType)
	ctx.SetBodyString(msg)
}

// Success sets response Content-Type and body to the given values.
func (ctx *RequestCtx) Success(contentType string, body []byte) {
	ctx.SetContentType(contentType)
	ctx.SetBody(body)
}

// SuccessString sets response Content-Type and body to the given values.
func (ctx *RequestCtx) SuccessString(contentType, body string) {
	ctx.SetContentType(contentType)
	ctx.SetBodyString(body)
}

// Redirect sets 'Location: uri' response header and sets the given statusCode.
//
// statusCode must have one of the following values:
//
//   - StatusMovedPermanently (301)
//   - StatusFound (302)
//   - StatusSeeOther (303)
//   - StatusTemporaryRedirect (307)
//   - StatusPermanentRedirect (308)
//
// All other statusCode values are replaced by StatusFound (302).
//
// The redirect uri may be either absolute or relative to the current
// request uri. Fasthttp will always send an absolute uri back to the client.
// To send a relative uri you can use the following code:
//
//	strLocation = []byte("Location") // Put this with your top level var () declarations.
//	ctx.Response.Header.SetCanonical(strLocation, "/relative?uri")
//	ctx.Response.SetStatusCode(fasthttp.StatusMovedPermanently)
func (ctx *RequestCtx) Redirect(uri string, statusCode int) {
	u := AcquireURI()
	ctx.URI().CopyTo(u)
	u.Update(uri)
	ctx.redirect(u.FullURI(), statusCode)
	ReleaseURI(u)
}

// RedirectBytes sets 'Location: uri' response header and sets
// the given statusCode.
//
// statusCode must have one of the following values:
//
//   - StatusMovedPermanently (301)
//   - StatusFound (302)
//   - StatusSeeOther (303)
//   - StatusTemporaryRedirect (307)
//   - StatusPermanentRedirect (308)
//
// All other statusCode values are replaced by StatusFound (302).
//
// The redirect uri may be either absolute or relative to the current
// request uri. Fasthttp will always send an absolute uri back to the client.
// To send a relative uri you can use the following code:
//
//	strLocation = []byte("Location") // Put this with your top level var () declarations.
//	ctx.Response.Header.SetCanonical(strLocation, "/relative?uri")
//	ctx.Response.SetStatusCode(fasthttp.StatusMovedPermanently)
func (ctx *RequestCtx) RedirectBytes(uri []byte, statusCode int) {
	ctx.Redirect(b2s(uri), statusCode)
}

func (ctx *RequestCtx) redirect(uri []byte, statusCode int) {
	ctx.Response.Header.setNonSpecial(strLocation, uri)
	statusCode = getRedirectStatusCode(statusCode)
	ctx.Response.SetStatusCode(statusCode)
}

func getRedirectStatusCode(statusCode int) int {
	// Only limited corrections are made.
	if statusCode < StatusMovedPermanently || statusCode > (StatusPermanentRedirect) {
		return StatusFound
	}
	/*
		if statusCode == StatusMovedPermanently || statusCode == StatusFound ||
			statusCode == StatusSeeOther || statusCode == StatusTemporaryRedirect ||
			statusCode == StatusPermanentRedirect {
			return statusCode
		}
	*/
	return statusCode
}

// SetBody sets response body to the given value.
//
// It is safe re-using body argument after the function returns.
func (ctx *RequestCtx) SetBody(body []byte) {
	ctx.Response.SetBody(body)
}

// SetBodyString sets response body to the given value.
func (ctx *RequestCtx) SetBodyString(body string) {
	ctx.Response.SetBodyString(body)
}

// ResetBody resets response body contents.
func (ctx *RequestCtx) ResetBody() {
	ctx.Response.ResetBody()
}

// SendFile sends local file contents from the given path as response body.
//
// This is a shortcut to ServeFile(ctx, path).
//
// SendFile logs all the errors via ctx.Logger.
//
// See also ServeFile, FSHandler and FS.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
//
// After this function returns, it does not mean that the file has been
// completely sent; for large files, streaming is used to send the data.
func (ctx *RequestCtx) SendFile(path string) {
	ServeFile(ctx, path)
}

// SendFileBytes sends local file contents from the given path as response body.
//
// This is a shortcut to ServeFileBytes(ctx, path).
//
// SendFileBytes logs all the errors via ctx.Logger.
//
// See also ServeFileBytes, FSHandler and FS.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
//
// After this function returns, it does not mean that the file has been
// completely sent; for large files, streaming is used to send the data.
func (ctx *RequestCtx) SendFileBytes(path []byte) {
	ServeFileBytes(ctx, path)
}

// IfModifiedSince returns true if lastModified exceeds 'If-Modified-Since'
// value from the request header.
//
// The function returns true also 'If-Modified-Since' request header is missing.
func (ctx *RequestCtx) IfModifiedSince(lastModified time.Time) (modified bool) {
	modified = true
	ifModStr := ctx.Request.Header.peek(strIfModifiedSince)
	if len(ifModStr) == 0 {
		return
	}
	ifMod, err := ParseHTTPDate(ifModStr)
	if err != nil {
		return
	}
	lastModified = lastModified.Truncate(time.Second)
	modified = ifMod.Before(lastModified)
	return
}

// NotModified resets response and sets '304 Not Modified' response status code.
func (ctx *RequestCtx) NotModified() {
	//ctx.Response.Reset()
	ctx.Response.resetForKeepAlive()
	ctx.SetStatusCode(StatusNotModified)
}

// NotFound resets response and sets '404 Not Found' response status code.
func (ctx *RequestCtx) NotFound() {
	//ctx.Response.Reset()
	ctx.Response.resetForKeepAlive()
	ctx.SetStatusCode(StatusNotFound)
	ctx.SetBodyString("404 Page not found")
}

// Write writes p into response body.
func (ctx *RequestCtx) Write(p []byte) (int, error) {
	ctx.Response.AppendBody(p)
	return len(p), nil
}

// WriteString appends s to response body.
func (ctx *RequestCtx) WriteString(s string) (int, error) {
	ctx.Response.AppendBodyString(s)
	return len(s), nil
}

// PostBody returns request body.
//
// The returned bytes are valid until your request handler returns.
// The request body, including the request body represented in any form.
//
// If the request body is represented as a stream and an error is encountered
// while reading, an error message string will be returned as the value.
func (ctx *RequestCtx) PostBody() []byte {
	return ctx.Request.Body()
}

// SetBodyStream sets response body stream and, optionally body size.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
//
// If bodySize is >= 0, then bodySize bytes must be provided by bodyStream
// before returning io.EOF.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// When `bodySize` is set to `-1`, it indicates that the response will be
// sent using Chunked encoding. Other negative values will result in
// setting the `Connection: close` header.
// See also SetBodyStreamWriter.
func (ctx *RequestCtx) SetBodyStream(bodyStream io.Reader, bodySize int64) {
	ctx.Response.SetBodyStream(bodyStream, bodySize)
}

// SetBodyStreamWriter registers the given stream writer for populating
// response body.
//
// Access to RequestCtx and/or its members is forbidden from sw.
//
// This function may be used in the following cases:
//
//   - if response body is too big (more than 10MB).
//   - if response body is streamed from slow external sources.
//   - if response body must be streamed to the client in chunks.
//     (aka `http server push`).
//
// The response body sent in this manner is always transmitted using Chunked encoding.
func (ctx *RequestCtx) SetBodyStreamWriter(sw StreamWriter) {
	ctx.Response.SetBodyStreamWriter(sw)
}

// IsBodyStream returns true if response body is set via SetBodyStream*.
// Is the `Response.bodyStream` field set?
func (ctx *RequestCtx) IsBodyStream() bool {
	return ctx.Response.IsBodyStream()
}

// Logger returns logger, which may be used for logging arbitrary
// request-specific messages inside RequestHandler.
//
// Each message logged via returned logger contains request-specific information
// such as request id, request duration, local address, remote address,
// request method and request url.
//
// It is safe re-using returned logger for logging multiple messages
// for the current request.
//
// The returned logger is valid until your request handler returns.
func (ctx *RequestCtx) Logger() Logger {
	if ctx.logger.ctx == nil {
		ctx.logger.ctx = ctx
	}
	if ctx.logger.logger == nil {
		ctx.logger.logger = ctx.s.logger()
	}
	return &ctx.logger
}

// NextProto adds nph to be processed when key is negotiated when TLS
// connection is established.
//
// This function can only be called before the server is started.
func (s *Server) NextProto(key string, nph ServeHandler) {
	if s.nextProtos == nil {
		s.nextProtos = make(map[string]ServeHandler)
	}

	s.configTLS()
	s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, key)
	s.nextProtos[key] = nph
}

func (s *Server) getNextProto(c net.Conn) (proto string, err error) {
	if tlsConn, ok := c.(connTLSer); ok {
		if s.ReadTimeout > 0 {
			if err = c.SetReadDeadline(time.Now().Add(s.ReadTimeout)); err != nil {
				return
			}
		}
		if s.WriteTimeout > 0 {
			if err = c.SetWriteDeadline(time.Now().Add(s.WriteTimeout)); err != nil {
				return
			}
		}
		err = tlsConn.Handshake()
		if err == nil {
			proto = tlsConn.ConnectionState().NegotiatedProtocol
		}
	}
	return
}

// ListenAndServe serves HTTP requests from the given TCP4 addr.
//
// Pass custom listener to Serve if you need listening on non-TCP4 media
// such as IPv6.
//
// Accepted connections are configured to enable TCP keep-alive.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := createTCPListener("tcp4", addr, s.TCPKeepalivePeriod, s.TCPKeepalive)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func (s *Server) ListenAndServeWithNotify(addr string, cancel context.CancelCauseFunc) error {
	ln, err := createTCPListener("tcp4", addr, s.TCPKeepalivePeriod, s.TCPKeepalive)
	cancel(err)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// ListenAndServeUNIX serves HTTP requests from the given UNIX addr.
//
// The function deletes existing file at addr before starting serving.
//
// The server sets the given file mode for the UNIX addr.
// Recommended mode is 0600.
func (s *Server) ListenAndServeUNIX(addr string, mode os.FileMode) error {
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unexpected error when trying to remove unix socket file %q: %w", addr, err)
	}
	ln, err := createTCPListener("unix", addr, s.TCPKeepalivePeriod, s.TCPKeepalive)
	if err != nil {
		return err
	}
	if err = os.Chmod(addr, mode); err != nil {
		return fmt.Errorf("cannot chmod %#o for %q: %w", mode, addr, err)
	}
	return s.Serve(ln)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP4 addr.
//
// certFile and keyFile are paths to TLS certificate and key files.
//
// Pass custom listener to Serve if you need listening on non-TCP4 media
// such as IPv6.
//
// If the certFile or keyFile has not been provided to the server structure,
// the function will use the previously added TLS configuration.
//
// Accepted connections are configured to enable TCP keep-alive.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	ln, err := createTCPListener("tcp4", addr, s.TCPKeepalivePeriod, s.TCPKeepalive)
	if err != nil {
		return err
	}
	return s.ServeTLS(ln, certFile, keyFile)
}

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP4 addr.
//
// certData and keyData must contain valid TLS certificate and key data.
//
// Pass custom listener to Serve if you need listening on arbitrary media
// such as IPv6.
//
// If the certFile or keyFile has not been provided the server structure,
// the function will use previously added TLS configuration.
//
// Accepted connections are configured to enable TCP keep-alive.
func (s *Server) ListenAndServeTLSEmbed(addr string, certData, keyData []byte) error {
	ln, err := createTCPListener("tcp4", addr, s.TCPKeepalivePeriod, s.TCPKeepalive)
	if err != nil {
		return err
	}
	return s.ServeTLSEmbed(ln, certData, keyData)
}

var ErrMissCertificate = errors.New("missing TLS certificates")

// ServeTLS serves HTTPS requests from the given listener.
// If `Server.TLSConfig` is not configured, both `certFile` and `keyFile` must be non-empty.
// certFile and keyFile are paths to TLS certificate and key files.
//
// If the certFile or keyFile has not been provided the server structure,
// the function will use previously added TLS configuration.
// After the server has started, changes to `Server.TLSConfig` will not take effect.
func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	s.mu.Lock()
	s.configTLS()
	configHasCert := len(s.TLSConfig.Certificates) > 0 || s.TLSConfig.GetCertificate != nil
	hasCertFiles := certFile != "" && keyFile != ""
	if !configHasCert {
		if !hasCertFiles {
			s.mu.Unlock()
			return ErrMissCertificate
		}
		//
		if err := s.AppendCert(certFile, keyFile); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	// BuildNameToCertificate has been deprecated since 1.14.
	// But since we also support older versions we'll keep this here.
	s.TLSConfig.BuildNameToCertificate() //nolint:staticcheck
	s.mu.Unlock()

	return s.Serve(
		tls.NewListener(ln, s.TLSConfig.Clone()),
	)
}

// ServeTLSEmbed serves HTTPS requests from the given listener.
//
// certData and keyData must contain valid TLS certificate and key data.
//
// If the certFile or keyFile has not been provided the server structure,
// the function will use previously added TLS configuration.
func (s *Server) ServeTLSEmbed(ln net.Listener, certData, keyData []byte) error {
	s.mu.Lock()
	s.configTLS()
	configHasCert := len(s.TLSConfig.Certificates) > 0 || s.TLSConfig.GetCertificate != nil
	if !configHasCert || len(certData) != 0 || len(keyData) != 0 {
		if err := s.AppendCertEmbed(certData, keyData); err != nil {
			s.mu.Unlock()
			return err
		}
	}

	// BuildNameToCertificate has been deprecated since 1.14.
	// But since we also support older versions we'll keep this here.
	s.TLSConfig.BuildNameToCertificate() //nolint:staticcheck

	s.mu.Unlock()

	return s.Serve(
		tls.NewListener(ln, s.TLSConfig.Clone()),
	)
}

// AppendCert appends certificate and keyfile to TLS Configuration.
//
// This function allows programmer to handle multiple domains
// in one server structure. See examples/multidomain.
func (s *Server) AppendCert(certFile, keyFile string) error {
	if certFile == "" && keyFile == "" {
		return ErrCertAndKeyMustProvided
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("cannot load TLS key pair from certFile=%q and keyFile=%q: %w", certFile, keyFile, err)
	}

	s.configTLS()
	s.TLSConfig.Certificates = append(s.TLSConfig.Certificates, cert)

	return nil
}

// AppendCertEmbed does the same as AppendCert but using in-memory data.
// Both `certData` and `keyData` must be provided and valid, otherwise
// `ErrCertAndKeyMustProvided` will be returned.
func (s *Server) AppendCertEmbed(certData, keyData []byte) error {
	if len(certData) == 0 && len(keyData) == 0 {
		return ErrCertAndKeyMustProvided
	}

	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return fmt.Errorf("cannot load TLS key pair from the provided certData(%d) and keyData(%d): %w",
			len(certData), len(keyData), err)
	}

	s.configTLS()
	s.TLSConfig.Certificates = append(s.TLSConfig.Certificates, cert)

	return nil
}

func (s *Server) configTLS() {
	if s.TLSConfig == nil {
		s.TLSConfig = &tls.Config{}
	}
}

// DefaultConcurrency is the maximum number of concurrent connections
// the Server may serve by default (i.e. if Server.Concurrency isn't set).
const DefaultConcurrency = 256 * 1024

// Serve serves incoming connections from the given listener.
//
// Serve blocks until the given listener returns permanent error.
func (s *Server) Serve(ln net.Listener) error {
	// The time when the Concurrency resource limit was last reached.
	// The minimum logging interval is one minute.
	var lastOverflowErrorTime time.Time
	// The time when the `MaxConPerIP` resource overflow last occurred.
	// The minimum logging interval is one minute.
	var lastPerIPErrorTime time.Time

	maxWorkersCount := s.getConcurrency()
	s.mu.Lock()
	s.ln = append(s.ln, ln)
	if s.done == nil {
		s.done = make(chan struct{})
	}
	if s.ResourceLimitError == nil {
		s.ResourceLimitError = defaultResourceLimitError
	}
	if atomic.LoadUint32(&s.connsInit) != 2 {
		if atomic.CompareAndSwapUint32(&s.connsInit, 0, 1) {
			s.conns = *xsync.NewMapOf[net.Conn, *ConnStatus](xsync.WithPresize(maxWorkersCount >> 4))
			atomic.StoreUint32(&s.connsInit, 2)
		} else {
			for {
				if atomic.LoadUint32(&s.connsInit) == 2 {
					// here s.conns must initialize.
					break
				}
			}
		}
	}
	// open count increased for this net.Listener
	//
	// The purpose of the open count for `net.Listener` within a mutex is to ensure that once the
	// `Shutdown` method observes this `net.Listener`, it can close all the connections it has opened.
	atomic.AddInt32(&s.open, 1)
	s.mu.Unlock()

	wp := &workerPool{
		WorkerFunc:            s.serveConn,
		MaxWorkersCount:       maxWorkersCount,
		LogAllErrors:          s.LogAllErrors,
		MaxIdleWorkerDuration: s.MaxIdleWorkerDuration,
		cleanThresholdFunc:    s.CleanThreshold,
		Logger:                s.logger(),
		connState:             s.setState,
		open:                  &s.open,
		concurrency:           &s.concurrency,
	}
	if wp.cleanThresholdFunc == nil {
		wp.cleanThresholdFunc = DefaultCleanThresholdFunc
	}
	wp.Start()
	// Count our waiting to accept a connection as an open connection.
	// This way we can't get into any weird state where just after accepting
	// a connection Shutdown is called which reads open as 0 because it isn't
	// incremented yet.
	//
	// open count decreased for net.Listener closed.
	defer atomic.AddInt32(&s.open, -1)

	for {
		// ---------accept-con----------
		c, err := ln.Accept()
		if err != nil {
			if //goland:noinspection GoTypeAssertionOnErrors
			netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.logger().Write(s2b("Timeout error when accepting new connections: " + netErr.Error()))
				time.Sleep(time.Second)
				continue
			}
			// Other errors stop workerPool.
			wp.Stop()
			// TODO here  can safely use net.ErrClosed?
			//
			// ErrClosed is the error returned by an I/O call on a network
			// connection that has already been closed, or that is closed by
			// another goroutine before the I/O is completed. This may be wrapped
			// in another error, and should normally be tested using
			// var ErrClosed error = errClosed
			errMsg := err.Error()
			if err != io.EOF && !strings.Contains(errMsg, "use of closed network connection") {
				s.logger().Write(s2b("Permanent error when accepting new connections: " + errMsg))
				return err
			}
			// Treat net.ErrClosed and io.EOF as nil error.
			return nil
		}
		// here net.Conn is a healthy net.Conn, so we should increment the open count.
		atomic.AddInt32(&s.open, 1)
		// here c is a healthy net.Conn
		// next we first check resource limit.
		// 1. check PerIpMaxConn limit.
		if s.MaxConnsPerIP > 0 {
			pic := wrapPerIPConn(s, c)
			if pic == nil {
				atomic.AddUint32(&s.rejectedByPerIpLimitCount, 1)
				atomic.AddInt32(&s.open, -1)
				if time.Since(lastPerIPErrorTime) > time.Minute {
					s.logger().Write(s2b("The number of connections from " + getConnIP4(c).String() + " exceeds MaxConnsPerIP=" + strconv.Itoa(s.MaxConnsPerIP)))
					lastPerIPErrorTime = time.Now()
				}
				continue
			}
			c = pic
		}
		// ---------net.Conn is ok----------
		// open count increased for each new connection
		//
		// We are uncertain about the implementation of `workerPool.Serve`, so we must increment the open
		// count first. This ensures that the `Shutdown` method can observe all connections
		// opened by the `net.Listener` instances it has already detected.

		if !wp.Serve(c) {
			// here situation, c cant server only because resource limit.
			atomic.AddUint32(&s.rejectedByConcurrencyLimitCount, 1)
			// Due to resource limitations, send an error message to
			// the peer and immediately close the connection afterward.
			//
			//goland:noinspection GoUnhandledErrorResult
			c.Write(s2b(s.ResourceLimitError(MaxConCurrencyLimit)))
			//goland:noinspection GoUnhandledErrorResult
			c.Close()
			// The minimum interval for logging Concurrency resource overflow is one minute.
			if time.Since(lastOverflowErrorTime) > time.Minute {
				s.logger().Write(s2b("The incoming connection cannot be served, because " + strconv.Itoa(maxWorkersCount) + " concurrent connections are served currently. " +
					"Try increasing Server.Concurrency"))
				lastOverflowErrorTime = time.Now()
			}
			// open count decreased for each new connection
			atomic.AddInt32(&s.open, -1)
			// The current server reached concurrency limit,
			// so give other concurrently running servers a chance
			// accepting incoming connections on the same address.
			//
			// There is a hope other servers didn't reach their
			// concurrency limits yet :)
			//
			// See also: https://github.com/valyala/fasthttp/pull/485#discussion_r239994990
			// Even without sleeping, it is possible to reject an accepted `net.Conn` due to a lack of resources.
			//
			// Attempt to sleep to avoid encountering the same error repeatedly.
			// This can also prevent other servers listening on the same port,
			// with unsaturated resources, from idling.
			if s.SleepWhenConcurrencyLimitsExceeded > 0 {
				time.Sleep(s.SleepWhenConcurrencyLimitsExceeded)
			}
		}
	}
}

// Shutdown gracefully shuts down the server without interrupting any active connections.
// Shutdown works by first closing all open listeners and then waiting indefinitely for all connections
// to return to idle and then shut down.
//
// When Shutdown is called, Serve, ListenAndServe, and ListenAndServeTLS immediately return nil.
// Make sure the program doesn't exit and waits instead for Shutdown to return.
//
// To prevent the method from potentially blocking indefinitely due to dead
// connections, it is recommended to use it in conjunction with a timeout mechanism.
//
// If a connection never enters the `StateIdle` state, this method will not return.
// If you want to return decisively after a certain period, refer to `ShutdownWithContext`.
func (s *Server) Shutdown() error {
	return s.ShutdownWithContext(context.Background())
}

// ShutdownWithContext gracefully shuts down the server without interrupting any active connections.
// ShutdownWithContext works by first closing all open listeners and then waiting for all connections to return to idle
// or context timeout and then shut down.
//
// When ShutdownWithContext is called, Serve, ListenAndServe, and ListenAndServeTLS immediately return nil.
// Make sure the program doesn't exit and waits instead for Shutdown to return.
//
// When `ctx` is canceled, this method will return immediately and will not wait for all active connections to be closed.
func (s *Server) ShutdownWithContext(ctx context.Context) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// stop to 1 will make serveConn() break out of its loop.
	atomic.StoreInt32(&s.stop, 1)
	defer atomic.StoreInt32(&s.stop, 0)

	if s.ln == nil {
		return
	}
	// First, close all entries in the `net.Listener` list.
	// Closing the listener will make Serve() method call worker pool's Stop method.
	for _, ln := range s.ln {
		// TODO here return ok?
		if err = ln.Close(); err != nil {
			return
		}
	}
	//
	if s.done != nil {
		close(s.done)
	}

	// Now, wait for all existing connections to be closed.
	// Or until the ctx is canceled.
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()
END:
	for {
		s.closeIdleConns()

		if open := atomic.LoadInt32(&s.open); open == 0 {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break END
		case <-ticker.C:
			continue
		}
	}

	s.done = nil
	s.ln = nil
	return err
}

func wrapPerIPConn(s *Server, c net.Conn) net.Conn {
	ip := getUint32IP(c)
	if ip == 0 {
		return c
	}
	n := s.perIPConnCounter.Register(ip)
	if n > s.MaxConnsPerIP {
		s.perIPConnCounter.Unregister(ip)
		if s.ResourceLimitError == nil {
			//goland:noinspection GoUnhandledErrorResult
			c.Write(s2b(defaultResourceLimitError(MaxConPerIpLimit)))
		} else {
			//goland:noinspection GoUnhandledErrorResult
			c.Write(s2b(s.ResourceLimitError(MaxConPerIpLimit)))
		}
		//goland:noinspection GoUnhandledErrorResult
		c.Close()
		return nil
	}
	return acquirePerIPConn(c, ip, &s.perIPConnCounter)
}

type DefaultLogger struct {
	*log.Logger
}

func (l DefaultLogger) Write(b []byte) {
	l.Println(b2s(b))
}

var defaultLogger = DefaultLogger{log.New(os.Stderr, "", log.LstdFlags)}

func (s *Server) logger() Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return defaultLogger
}

var (
	// ErrPerIPConnLimit may be returned from ServeConn if the number of connections
	// per ip exceeds Server.MaxConnsPerIP.
	ErrPerIPConnLimit = errors.New("too many connections per ip")

	// ErrConcurrencyLimit may be returned from ServeConn if the number
	// of concurrently served connections exceeds Server.Concurrency.
	ErrConcurrencyLimit = errors.New("cannot serve the connection because Server.Concurrency concurrent connections are served")
)

// ServeConn serves HTTP requests from the given connection.
//
// ServeConn returns nil if all requests from the c are successfully served.
// It returns non-nil error otherwise.
//
// Connection c must immediately propagate all the data passed to Write()
// to the client. Otherwise requests' processing may hang.
//
// ServeConn closes c before returning.
func (s *Server) ServeConn(c net.Conn) error {
	atomic.AddInt32(&s.open, 1)
	if atomic.LoadUint32(&s.connsInit) != 2 {
		if atomic.CompareAndSwapUint32(&s.connsInit, 0, 1) {
			s.conns = *xsync.NewMapOf[net.Conn, *ConnStatus](xsync.WithPresize(s.Concurrency >> 4))
			atomic.StoreUint32(&s.connsInit, 2)
		} else {
			for {
				if atomic.LoadUint32(&s.connsInit) == 2 {
					break
				}
			}
		}
	}
	if s.MaxConnsPerIP > 0 {
		pic := wrapPerIPConn(s, c)
		if pic == nil {
			atomic.AddInt32(&s.open, -1)
			return ErrPerIPConnLimit
		}
		c = pic
	}

	n := atomic.AddUint32(&s.concurrency, 1)
	if n > uint32(s.getConcurrency()) {
		atomic.AddUint32(&s.concurrency, ^uint32(0))
		if s.ResourceLimitError == nil {
			//goland:noinspection GoUnhandledErrorResult
			c.Write(s2b(defaultResourceLimitError(MaxConCurrencyLimit)))
		} else {
			//goland:noinspection GoUnhandledErrorResult
			c.Write(s2b(s.ResourceLimitError(MaxConCurrencyLimit)))
		}
		atomic.AddInt32(&s.open, -1)
		//goland:noinspection GoUnhandledErrorResult
		c.Close()
		return ErrConcurrencyLimit
	}
	s.setState(c, StateNew)
	err := s.serveConn(c)
	atomic.AddInt32(&s.open, -1)
	atomic.AddUint32(&s.concurrency, ^uint32(0))

	if err != errHijacked {
		errc := c.Close()
		s.setState(c, StateClosed)
		if err == nil {
			err = errc
		}
	} else {
		err = nil
		s.setState(c, StateHijacked)
	}
	return err
}

var errHijacked = errors.New("connection has been hijacked")

// GetCurrentConcurrency returns a number of currently served
// connections.
//
// This function is intended be used by monitoring systems.
// This does not include connections that were rejected due to resource limitations.
func (s *Server) GetCurrentConcurrency() uint32 {
	return atomic.LoadUint32(&s.concurrency)
}

// GetOpenConnectionsCount returns a number of opened connections.
// This includes connections that were rejected due to resource limitations.
// This does not include the `net.Listener` count.
// This function is intended be used by monitoring systems.
func (s *Server) GetOpenConnectionsCount() int32 {
	var listenerCount int32
	// Make every effort to minimize the impact of the `net.Listener` count.
	if s.mu.TryLock() {
		listenerCount = int32(len(s.ln))
		s.mu.Unlock()
	}
	return atomic.LoadInt32(&s.open) - listenerCount
}

// RejectedByConcurrencyLimitCount returns a number of rejected connections by Concurrency.
//
// This function is intended be used by monitoring systems.
func (s *Server) RejectedByConcurrencyLimitCount() uint32 {
	return atomic.LoadUint32(&s.rejectedByConcurrencyLimitCount)
}

// RejectedByPerIpLimitCount returns a number of rejected connections by MaxConnsPerIP.
//
// This function is intended be used by monitoring systems.
func (s *Server) RejectedByPerIpLimitCount() uint32 {
	return atomic.LoadUint32(&s.rejectedByPerIpLimitCount)
}

func (s *Server) getConcurrency() int {
	n := s.Concurrency
	if n <= 0 {
		n = DefaultConcurrency
	}
	return n
}

var globalConnID uint64

func nextConnID() uint64 {
	return atomic.AddUint64(&globalConnID, 1)
}

// DefaultMaxRequestBodySize is the maximum request body size the server
// reads by default.
//
// See Server.MaxRequestBodySize for details.
const DefaultMaxRequestBodySize = 4 * 1024 * 1024

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout != 0 {
		return s.IdleTimeout
	}
	return s.ReadTimeout
}

func (s *Server) serveConn(c net.Conn) (err error) {
	// load net.Conn state
	// StateIdle StateActiveR and StateActiveW handled by this method.
	cs, _ := s.conns.Load(c)

	var proto string
	if proto, err = s.getNextProto(c); err != nil {
		// The logic for the judgment comes from the `net/http` package.
		if //goland:noinspection GoTypeAssertionOnErrors
		re, ok := err.(tls.RecordHeaderError); ok && re.Conn != nil && tlsRecordHeaderLooksLikeHTTP(re.RecordHeader) {
			//goland:noinspection GoUnhandledErrorResult
			re.Conn.Write(s2b(httpToHttpsErr))
			//goland:noinspection GoUnhandledErrorResult
			re.Conn.Close()
		}
		return
	}
	if handler, ok := s.nextProtos[proto]; ok {
		// Remove read or write deadlines that might have previously been set in
		// above getNextProto method.
		// The next handler is responsible for setting its own deadlines.
		if s.ReadTimeout > 0 || s.WriteTimeout > 0 {
			if err = c.SetDeadline(zeroTime); err != nil {
				return
			}
		}
		return handler(c)
	}
	var (
		writeTimeout       = s.WriteTimeout
		readTimeout        = s.ReadTimeout
		maxRequestBodySize = s.MaxRequestBodySize
		serverName         = s.getServerName()
	)
	var (
		// In the loop below, this variable is incremented.
		connRequestNum uint64
		// This variable does not change in the loop below.
		connID = nextConnID()
		// This variable does not change in the loop below.
		connTime = time.Now()
	)
	var (
		ctx = s.acquireCtx(c)
	)
	ctx.connTime = connTime
	if maxRequestBodySize <= 0 {
		maxRequestBodySize = DefaultMaxRequestBodySize
	}
	var (
		// Read the cache, and pre-read the connection content into `br`.
		br *bufio.Reader
		// Write the cache, and first send the response to `bw`.
		bw            *bufio.Writer
		hijackHandler HijackHandler
		// lastReadTimeout is used to cache the previous
		// read timeout to avoid recalculating it and to determine
		// whether the read timeout should be cleared.
		//lastReadTimeout  time.Duration
		responseStr      string
		hijackNoResponse bool
		// Should the `Connection: close` header be sent?
		connectionClose bool
		// For handling buggy POST requests.
		prevMethodIsPost bool
		// Used to determine whether to clear the write timeout.
		//previousWriteTimeout bool
		// Should this error message be sent to the client?
		// Should `Expect: 100-continue` requests be handled?
		continueReadingRequest = true
		// Is this `net.Conn` a `tls.Conn`?
		isTLS bool
	)
	_, isTLS = c.(connTLSer)
	ctx.Request.isTLS = isTLS
	ctx.isTLS = isTLS
	for {
		// The order of each request within a connection, with the first request having an order of 1.
		connRequestNum++
		// A non-keep-alive connection doesn't have the concept of idle timeout because
		// it only serves a single request. If this is a keep-alive connection set the idle timeout.
		if connRequestNum > 1 {
			if d := s.idleTimeout(); d > 0 {
				// The maximum time interval between the end of the previous request and
				// the arrival of the first byte of the next request.
				// `idleTimeout` is set here.
				if err = c.SetReadDeadline(time.Now().Add(d)); err != nil {
					break
				}
			}
		}
		// When `ReduceMemoryUsage` is not enabled, `br` is definitely not nil.
		// However, when `ReduceMemoryUsage` is enabled, `br` may or may not be nil.
		if !s.ReduceMemoryUsage || br != nil {
			// Normal execution path
			if br == nil {
				// The path taken during the first execution of the loop.
				br = acquireReader(ctx)
			}
			// If this is a keep-alive connection we want to try and read the first bytes
			// within the idle time.
			if connRequestNum > 1 {
				// The IdleTimeout configuration takes effect here.
				// `idleTimeout` takes effect.
				_, err = br.Peek(1)
			}
		} else {
			if connRequestNum == 1 && readTimeout > 0 {
				// Make `ReadTimeout` effective for the first request in
				// the case of `ReduceMemoryUsage`.
				err = c.SetReadDeadline(time.Now().Add(readTimeout))
				if err != nil {
					return
				}
			}
			// When s.ReduceMemoryUsage = true and br = nil
			//
			// If this is a keep-alive connection acquireByteReader will try to peek
			// a couple of bytes already so the idle timeout will already be used.
			//
			// Attempt to release the ctx before reading the first byte from net.
			// Conn within the IdleTimeout interval. Reallocate the ctx after the byte is read,
			// in order to respect the ReduceMemoryUsing configuration.
			//
			// err is when read first byte from net.Conn
			// `idleTimeout` takes effect.
			br, err = acquireByteReader(&ctx)
			// `ctx` might be initialized to the zero value during the call to the `acquireByteReader`
			// function, so it needs to be checked here.
			if ctx != nil {
				// `ctx` might be reset in the `acquireByteReader` function.
				ctx.isTLS = isTLS
				ctx.Request.isTLS = isTLS
			}
		}

		// Err check path 1
		// Errors that occur when reading the first byte from `net.Conn`.
		// This error is unrelated to the HTTP protocol.
		if err != nil {
			// If the connection is closed or times out, err will not be nil.
			break
		}

		// Next attempt Read Request Head ============
		//
		// Whenever a new request arrives, the state of net.Conn transitions from idle to active.
		// It's important to note that there's a window of time during this state transition.
		// ShutDown may be called during this window of time.
		cs.lastActive.Store(absoluteNano())
		cs.status.Store(int32(StateActiveR))
		if s.ConnState != nil {
			s.setState(c, StateActiveR)
		}

		if readTimeout > 0 {
			// In the case of `ReduceMemoryUsage` and first request, lastReadTimeout != 0.
			if !s.ReduceMemoryUsage || connRequestNum != 1 {
				// Here, in addition to clearing the read timeout, it also serves to clear the `IdleTimeout`.
				if err = c.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
					break
				}
			}
		} else if s.IdleTimeout > 0 && connRequestNum > 1 {
			// If this was an idle connection and the server has an IdleTimeout but
			// no ReadTimeout then we should remove the IdleTimout.
			if err = c.SetReadDeadline(zeroTime); err != nil {
				break
			}
		}
		if s.DisableHeaderNamesNormalizing {
			ctx.Request.Header.DisableNormalizing()
			ctx.Response.Header.DisableNormalizing()
		}
		// Attempt Read Request Head +++++++++++++++++
		// In the normal execution path, block and read the full request headers for this request.
		// The error could be due to a network issue or an incorrect format in the HTTP request headers.
		err = ctx.Request.Header.readLoop(br, true, prevMethodIsPost, true)
		// Err check path
		if err != nil {
			// Although there are two possible errors, we choose to handle it as
			// a format error and respond to the client accordingly.
			if err == HeaderBufferSmallErr {
				responseStr = statusRequestHeaderFieldsTooLargeErr
				break
			}
			//goland:noinspection GoTypeAssertionOnErrors
			_, ok := err.(*unsupportedTEError)
			if ok {
				responseStr = statusNotImplementedErr
				break
			}
			//goland:noinspection GoTypeAssertionOnErrors
			_, ok = err.(*HeaderParseErr)
			if ok && strings.Contains(err.Error(), "unsupported HTTP version") {
				responseStr = statusHTTPVersionNotSupportedErr
				break
			}
			responseStr = commonRequestErr
			break
		}

		// Get Only Check ================
		if s.GetOnly && !ctx.Request.Header.IsGet() && !ctx.Request.Header.IsHead() {
			responseStr = statusMethodNotAllowedErr
			err = ErrGetOnly
			break
		}
		// Configuration according Header ================
		if onHdrRecv := s.HeaderReceived; onHdrRecv != nil {
			reqConf := onHdrRecv(&ctx.Request.Header)
			// The read timeout set by HeaderReceived should be cleared at the end of
			// the loop to avoid affecting the reading of the next request when
			// Server.ReadTimeout is not set.
			if reqConf.ReadTimeout > 0 {
				// The read timeout set by HeaderReceived will only
				// affect the complete or partial reading of the response body.
				readTimeout = reqConf.ReadTimeout
				if err = c.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
					break
				}
			} else {
				readTimeout = s.ReadTimeout
			}
			//
			switch {
			case reqConf.MaxRequestBodySize > 0:
				maxRequestBodySize = reqConf.MaxRequestBodySize
			case s.MaxRequestBodySize > 0:
				maxRequestBodySize = s.MaxRequestBodySize
			default:
				maxRequestBodySize = DefaultMaxRequestBodySize
			}
			//
			if reqConf.WriteTimeout > 0 {
				writeTimeout = reqConf.WriteTimeout
			} else {
				writeTimeout = s.WriteTimeout
			}
		}
		// Read Expect Continue Request ================
		// 'Expect: 100-continue' request handling.
		// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html#sec8.2.3 for details.
		if ctx.Request.MayContinue() {
			// Allow the ability to deny reading the incoming request body
			if s.ContinueHandler != nil {
				continueReadingRequest = s.ContinueHandler(&ctx.Request.Header)
			}
			if continueReadingRequest {
				responseStr = strResponseContinue
			} else {
				responseStr = strResponseContinueFail
			}
			//
			if writeTimeout > 0 {
				err = c.SetWriteDeadline(time.Now().Add(writeTimeout))
				if err != nil {
					responseStr = ""
					break
				}
			}
			// Send 'HTTP/1.1 100 Continue' response.
			_, err = c.Write(s2b(responseStr))
			responseStr = ""
			if err != nil {
				break
			}
			if writeTimeout > 0 {
				err = c.SetWriteDeadline(zeroTime)
				if err != nil {
					break
				}
			}
		}
		// If the client wanted a 100-continue, but we never sent it to
		// them (or, more strictly: we never finished reading their
		// request body), don't reuse this connection.
		//
		// This behavior was first added on the theory that we don't know
		// if the next bytes on the wire are going to be the remainder of
		// the request body or the subsequent request (see issue 11549),
		// but that's not correct: If we keep using the connection,
		// the client is required to send the request body whether we
		// asked for it or not.
		//
		// We probably do want to skip reusing the connection in most cases,
		// however. If the client is offering a large request body that we
		// don't intend to use, then it's better to close the connection
		// than to read the body. For now, assume that if we're sending
		// headers, the handler is done reading the body, and we should
		// drop the connection if we haven't seen EOF.
		// if ecr, ok := w.req.Body.(*expectContinueReader); ok && !ecr.sawEOF.Load() {
		//	  w.closeAfterReply = true
		// }
		if !continueReadingRequest {
			break
		}
		ctx.Request.Header.noDefaultContentType = s.NoDefaultContentType
		// Read Request Body  ================
		if s.StreamRequestBody {
			ctx.Request.StreamBody = s.StreamRequestBody
			ctx.Request.DiscardUnReadBodyStream = s.DiscardUnReadRequestBodyStream
			ctx.Request.maxBodySize = maxRequestBodySize
			err = ctx.Request.ReadBodyStream(br, maxRequestBodySize, !s.DisablePreParseMultipartForm)
		} else {
			err = ctx.Request.ReadBody(br, maxRequestBodySize, !s.DisablePreParseMultipartForm)
		}

		if err != nil {
			if err == ErrBodyTooLarge || err == multipart.ErrMessageTooLarge {
				responseStr = statusRequestEntityTooLargeErr
				break
			}
			responseStr = commonRequestErr
			break
		}

		// Read Request Body +++++++++++++++
		// When Server.StreamRequestBody is set to true, the return value of br.Buffered()
		// at this point cannot be guaranteed to be zero, although the
		// likelihood of this is very low.
		if br != nil && (s.ReduceMemoryUsage && !s.StreamRequestBody && br.Buffered() == 0) {
			// When an error is encountered while reading the request body,
			// you can safely release br because we will immediately exit the loop.
			releaseReader(s, br)
			br = nil
		}
		// store req.ConnectionClose so even if it was changed inside of handler
		connectionClose = s.DisableKeepalive || ctx.Request.Header.ConnectionClose()

		if serverName != "" {
			ctx.Response.Header.SetServer(serverName)
		}
		ctx.connID = connID
		ctx.connRequestNum = connRequestNum
		ctx.time = time.Now()

		// Information from Server for handler.
		ctx.Response.Header.noDefaultContentType = s.NoDefaultContentType
		ctx.Response.Header.noDefaultDate = s.NoDefaultDate

		s.Handler(ctx)
		if ctx.Request.needFinish() {
			// Resetting the `ReadTimeout` cannot rely on the previous settings because the
			// `Handler` call occurred in the meantime.
			if readTimeout > 0 {
				err = c.SetReadDeadline(time.Now().Add(readTimeout))
				if err != nil {
					break
				}
			}
			err = ctx.Request.closeBodyStream()
			if err != nil {
				break
			}
			if readTimeout > 0 {
				err = c.SetReadDeadline(zeroTime)
				if err != nil {
					break
				}
			}
		}
		if ctx.hijacked {
			// After the handler function returns, only net.Conn can be retained. Other resources,
			// if needed, must be copied within the handler.
			//
			// TODO Is there a need to provide an option to retain reqCtx?
			//
			// Users can obtain the underlying connection using the ctx.Conn method in the handler.
			// But is this connection healthy?
			// At this point, there's no way to see the status of any net.Conn.
			s.KeepHijackedConns = true
			if readTimeout > 0 {
				//goland:noinspection GoUnhandledErrorResult
				c.SetReadDeadline(zeroTime)
			}
			err = errHijacked
			break
		}
		//
		cs.lastActive.Store(absoluteNano())
		cs.status.Store(int32(StateActiveW))
		if s.ConnState != nil {
			s.setState(c, StateActiveW)
		}

		hijackHandler = ctx.hijackHandler
		ctx.hijackHandler = nil
		hijackNoResponse = ctx.hijackNoResponse && hijackHandler != nil
		ctx.hijackNoResponse = false
		if ctx.IsHead() {
			ctx.Response.Header.headMethod = true
			ctx.Response.SkipBody = true
		}

		// Set the write timeout because the actual response will be sent next.
		if writeTimeout > 0 {
			if err = c.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				break
			}
		}

		shouldStop := atomic.LoadInt32(&s.stop) == 1
		// 1. Server.KeepAlive=false
		// 2. The request header is set with Connection: close.
		// 3. The number of requests already handled on the same connection is equal to or greater than Server.MaxRequestsPerConn.
		// 4. The Response.Header explicitly sets the Connection: close response header.
		// 5. The Server.ShutDown method has been called.
		if ctx.Response.Header.ConnectionClose() {
			connectionClose = true
		} else {
			if connectionClose || s.CloseOnShutdown && shouldStop {
				ctx.Response.Header.SetConnectionClose()
				connectionClose = true
			} else if !ctx.Request.Header.IsHTTP11() {
				// Set 'Connection: keep-alive' response header for HTTP/1.0 request.
				// There is no need in setting this header for http/1.1, since in http/1.1
				// connections are keep-alive by default.
				ctx.Response.Header.setNonSpecial(strConnection, strKeepAlive)
			}
		}

		if len(ctx.Response.Header.Server()) == 0 && serverName != "" {
			ctx.Response.Header.SetServer(serverName)
		}

		if !hijackNoResponse {
			if writeTimeout > 0 {
				err = c.SetWriteDeadline(time.Now().Add(writeTimeout))
				if err != nil {
					break
				}
			}
			// If the connection has been hijacked and the net.Conn is retained,
			// it is best to set HijackNoResponse to true because the user code in the handler
			// cannot observe whether the response was successfully sent.
			if bw == nil {
				bw = acquireWriter(ctx)
			}
			if err = writeResponse(ctx, bw); err != nil {
				break
			}
			err = bw.Flush()
			if err != nil {
				break
			}
			// If Connection: Close is set, hijacking net.Conn is pointless
			// because the other end will definitely close the connection next.
			if connectionClose {
				break
			}
			if s.ReduceMemoryUsage {
				releaseWriter(s, bw)
				bw = nil
			}
			if ctx.IsPost() && br != nil {
				// RFC 7230 section 3 tolerance for old buggy clients.
				// copy net/http
				n := br.Buffered()
				if n > 4 {
					n = 4
				}
				peek, _ := br.Peek(n) // tryRead will get err below
				//goland:noinspection GoUnhandledErrorResult
				br.Discard(numLeadingCRorLF(peek))
			}
		}

		// It's possible that this net.Conn is not a keep-alive connection because
		// setting hijackNoResponse to true bypasses the connection close check above.
		// However, it doesn't matter.
		if hijackHandler != nil {
			var hjr io.Reader = c
			if br != nil {
				// Hijack will retain br, so set br to nil to avoid releasing
				// it multiple times after exiting the loop.
				hjr = br
				br = nil
			}
			if writeTimeout > 0 || readTimeout > 0 {
				err = c.SetDeadline(zeroTime)
			}
			if err != nil {
				break
			}
			// In many cases, users only want the connection and do not want to handle the retained net.Conn in a separate goroutine.
			// In this case, starting a goroutine to execute hijackConnHandler seems unnecessary.
			go hijackConnHandler(ctx, hjr, c, s, hijackHandler)
			err = errHijacked
			break
		}
		if shouldStop {
			break
		}
		// Cleaning up resources and timers at the end of the loop has the lowest cost.
		if writeTimeout > 0 {
			if err = c.SetWriteDeadline(zeroTime); err != nil {
				break
			}
		}
		if readTimeout > 0 {
			if err = c.SetReadDeadline(zeroTime); err != nil {
				break
			}
		}
		//
		cs.status.Store(int32(StateIdle))
		if s.ConnState != nil {
			s.setState(c, StateIdle)
		}
		prevMethodIsPost = ctx.IsPost()
		ctx.userValues.Reset()
		ctx.Request.Reset()
		ctx.Response.Reset()
		ctx.Request.isTLS = isTLS
		responseStr = ""
	}
	if err == io.EOF {
		err = nil
	}
	//
	if len(responseStr) > 0 {
		// 读取请求体时发生错误，对端关闭了连接、读超时或者请求体/头过大引发的错误。
		// TODO add custom err handle for this.
		// TODO Is the error handling here too complex?
		var er error
		if writeTimeout > 0 {
			er = c.SetWriteDeadline(time.Now().Add(writeTimeout))
		}
		if er == nil {
			_, er = c.Write(s2b(responseStr))
		}
		if writeTimeout > 0 && er == nil {
			//goland:noinspection GoUnhandledErrorResult
			c.SetWriteDeadline(zeroTime)
		}
	}
	//
	if s.ErrorHandler != nil && ctx != nil {
		s.ErrorHandler(ctx, err)
	}
	// Checking whether request-related resources are released before
	// the function returns ensures that resources are not leaked.
	if br != nil {
		releaseReader(s, br)
	}
	if bw != nil {
		releaseWriter(s, bw)
	}
	if hijackHandler == nil && ctx != nil {
		s.releaseCtx(ctx)
	}
	return
}

func (s *Server) setState(nc net.Conn, state ConnState) {
	if state == StateNew || state > StateIdle {
		// only tracking StateNew StateClosed StateHijacked
		s.trackConn(nc, state)
	}
	if hook := s.ConnState; hook != nil {
		hook(nc, state)
	}
}

func hijackConnHandler(ctx *RequestCtx, r io.Reader, c net.Conn, s *Server, h HijackHandler) {
	hjc := s.acquireHijackConn(r, c)
	h(hjc)

	if br, ok := r.(*bufio.Reader); ok {
		releaseReader(s, br)
	}
	if !s.KeepHijackedConns {
		//goland:noinspection GoUnhandledErrorResult
		c.Close()
		s.releaseHijackConn(hjc)
	}
	s.releaseCtx(ctx)
}

func (s *Server) acquireHijackConn(r io.Reader, c net.Conn) *hijackConn {
	v := s.hijackConnPool.Get()
	if v == nil {
		hjc := &hijackConn{
			Conn: c,
			r:    r,
			s:    s,
		}
		return hjc
	}
	hjc := v.(*hijackConn)
	hjc.Conn = c
	hjc.r = r
	return hjc
}

func (s *Server) releaseHijackConn(hjc *hijackConn) {
	hjc.Conn = nil
	hjc.r = nil
	s.hijackConnPool.Put(hjc)
}

type hijackConn struct {
	net.Conn
	r io.Reader
	s *Server
}

func (c *hijackConn) UnsafeConn() net.Conn {
	return c.Conn
}

func (c *hijackConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *hijackConn) Close() (err error) {
	if !c.s.KeepHijackedConns {
		// when we do not keep hijacked connections,
		// it's net.Conn closed and release in hijackConnHandler.
		return
	}
	err = c.Conn.Close()
	c.s.releaseHijackConn(c)
	return
}

func writeResponse(ctx *RequestCtx, w *bufio.Writer) error {
	return ctx.Response.Write(w)
}

// ChunkWriter Write content to the underlying `Writer` using chunked encoding.
type ChunkWriter struct {
	*bufio.Writer
}

// Write the parameter `p` to the underlying `Writer` using chunked
// encoding. If the length of `p` is 0, write nothing.
func (w ChunkWriter) Write(b []byte) (n int, err error) {
	n = len(b)
	if n == 0 {
		return
	}
	err = writeChunkWithoutFlush(w.Writer, b)
	return
}

// BufferedCompressWriter Some compression algorithms produce very small output blocks. In such cases, these small
// blocks are cached and only flushed to the underlying `Writer` when `Flush` is called or
// when the cache is full.
type BufferedCompressWriter struct {
	*pbufio.Writer
}

func (bw BufferedCompressWriter) Write(b []byte) (n int, err error) {
	return bw.Writer.Write(b)
}
func (bw BufferedCompressWriter) Flush() (err error) {
	err = bw.Writer.Flush()
	return
}

// ResponseStreamWriter Send the response in Chunked encoding format.
type ResponseStreamWriter struct {
	// The response is cached here first. It is written to `cw` when
	// the cache is full or when a flush occurs.
	WriterFlusher
	// Flush the response to the underlying `net.Conn` connection as needed.
	cw ChunkWriter
}

// Flush the cached response to `cw`, which encodes it as Chunked. Then flush
// the contents of `cw` to the `net.Conn`.
func (r ResponseStreamWriter) Flush() (err error) {
	err = r.WriterFlusher.Flush()
	err2 := r.cw.Flush()
	if err == nil {
		err = err2
	}
	return
}

// Close the `WriterFlusher` to release resources or flush the contents in the `WriterFlusher`.
// Here, the contents of `cw` are not flushed.
// Delay sending the chunked encoding's final chunk and the trailers headers until the end.
//
// In the `Close` method, we do not flush the contents of `bw` because there are still Chunked
// encoding's trailing content and possibly trailers headers that need to be written.
func (r ResponseStreamWriter) Close() (err error) {
	if c, ok := r.WriterFlusher.(io.Closer); ok {
		return c.Close()
	}
	return r.WriterFlusher.Flush()
}

// CompressWriter Output the content using the `compress.Writer` algorithm to `wf`.
// This structure is only applicable to compression algorithms that require caching.
type CompressWriter struct {
	compress.Writer
	// The destination for the output of compressed content is `wf`.
	// The contents in `wf` ultimately need to be flushed to `cw`.
	wf WriterFlusherCloser
}

// Flush the contents in the compressor and also flush the contents in the target cache.
// The target attribute is `cw`, which will encode these contents in Chunked format.
func (cw CompressWriter) Flush() (err error) {
	err = cw.Writer.Flush()
	err2 := cw.wf.Flush()
	if err == nil {
		err = err2
	}
	return
}

// Close the compressor and flush the contents in the underlying cache.
func (cw CompressWriter) Close() (err error) {
	err = cw.Writer.Close()
	err2 := cw.wf.Flush()
	if err == nil {
		err = err2
	}
	return
}

const (
	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 4096
)

// This function is used only when `SaveMemoryUsage` is set to `true`.
func acquireByteReader(ctxP **RequestCtx) (r *bufio.Reader, err error) {
	ctx := *ctxP
	s := ctx.s
	c := ctx.c
	var (
		b  [1]byte
		n  int
		tc *net.TCPConn
	)
	// quick path
	tlc, ok := c.(*tls.Conn)
	if ok {
		tc, ok = tlc.NetConn().(*net.TCPConn)
	} else {
		tc, ok = c.(*net.TCPConn)
	}
	if ok {
		n, err = goutils.NoBlockingRead(tc, b[:])
		if err != nil && err != syscall.EAGAIN {
			return
		}
		err = nil
		if n == 1 {
			// Since the content has been successfully read, there is no need to release
			// `RequestCtx` and its associated resources.
			ctx.fbr.c = c
			ctx.fbr.ch = b[0]
			ctx.fbr.byteRead = false
			r = acquireReader(ctx)
			r.Reset(&ctx.fbr)
			return
		}
	}
	// Slow path
	// Here err is syscall.EAGAIN, so blocking
	s.releaseCtx(ctx)
	ctx = nil
	*ctxP = nil
	n, err = c.Read(b[:])
	if err != nil {
		return nil, err
	}
	// Since the content has been successfully read, there is no
	// need to release `RequestCtx` and its associated resources.
	ctx = s.acquireCtx(c)
	*ctxP = ctx
	if n != 1 {
		panic("BUG: Reader must return at least one byte")
	}
	ctx.fbr.c = c
	ctx.fbr.ch = b[0]
	ctx.fbr.byteRead = false
	r = acquireReader(ctx)
	r.Reset(&ctx.fbr)
	return
}

func acquireReader(ctx *RequestCtx) *bufio.Reader {
	v := ctx.s.readerPool.Get()
	if v == nil {
		n := ctx.s.ReadBufferSize
		if n <= 0 {
			n = defaultReadBufferSize
		}
		return bufio.NewReaderSize(ctx.c, n)
	}
	r := v.(*bufio.Reader)
	r.Reset(ctx.c)
	return r
}

func releaseReader(s *Server, r *bufio.Reader) {
	s.readerPool.Put(r)
}

func acquireWriter(ctx *RequestCtx) *bufio.Writer {
	v := ctx.s.writerPool.Get()
	if v == nil {
		n := ctx.s.WriteBufferSize
		if n <= 0 {
			n = defaultWriteBufferSize
		}
		return bufio.NewWriterSize(ctx.c, n)
	}
	w := v.(*bufio.Writer)
	w.Reset(ctx.c)
	return w
}

func releaseWriter(s *Server, w *bufio.Writer) {
	s.writerPool.Put(w)
}

func (s *Server) acquireCtx(c net.Conn) (ctx *RequestCtx) {
	v := s.ctxPool.Get()
	if v == nil {
		keepBodyBuffer := !s.ReduceMemoryUsage
		ctx = new(RequestCtx)
		ctx.Request.keepBodyBuffer = keepBodyBuffer
		ctx.Response.keepBodyBuffer = keepBodyBuffer
		ctx.s = s
	} else {
		ctx = v.(*RequestCtx)
	}
	ctx.c = c
	return ctx
}

// Init2 prepares ctx for passing to RequestHandler.
//
// conn is used only for determining local and remote addresses.
//
// This function is intended for custom Server implementations.
// See https://github.com/valyala/httpteleport for details.
func (ctx *RequestCtx) Init2(conn net.Conn, logger Logger, reduceMemoryUsage bool) {
	ctx.c = conn
	ctx.remoteAddr = nil
	ctx.logger.logger = logger
	ctx.connID = nextConnID()
	ctx.s = fakeServer
	ctx.connRequestNum = 0
	ctx.connTime = time.Now()

	keepBodyBuffer := !reduceMemoryUsage
	ctx.Request.keepBodyBuffer = keepBodyBuffer
	ctx.Response.keepBodyBuffer = keepBodyBuffer
}

// Init prepares ctx for passing to RequestHandler.
//
// remoteAddr and logger are optional. They are used by RequestCtx.Logger().
//
// This function is intended for custom Server implementations.
func (ctx *RequestCtx) Init(req *Request, remoteAddr net.Addr, logger Logger) {
	if remoteAddr == nil {
		remoteAddr = zeroTCPAddr
	}
	c := &fakeAddrer{
		laddr: zeroTCPAddr,
		raddr: remoteAddr,
	}
	if logger == nil {
		logger = defaultLogger
	}
	ctx.Init2(c, logger, true)
	req.CopyTo(&ctx.Request)
}

// Deadline returns the time when work done on behalf of this context
// should be canceled. Deadline returns ok==false when no deadline is
// set. Successive calls to Deadline return the same results.
//
// This method always returns 0, false and is only present to make
// RequestCtx implement the context interface.
func (ctx *RequestCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

// Done returns a channel that's closed when work done on behalf of this
// context should be canceled. Done may return nil if this context can
// never be canceled. Successive calls to Done return the same value.
//
// Note: Because creating a new channel for every request is just too expensive, so
// RequestCtx.s.done is only closed when the server is shutting down.
func (ctx *RequestCtx) Done() <-chan struct{} {
	// fix use new variables to prevent panic caused by modifying the original done chan to nil.
	done := ctx.s.done
	if done == nil {
		done = make(chan struct{}, 1)
		done <- struct{}{}
		return done
	}
	return done
}

// Err returns a non-nil error value after Done is closed,
// successive calls to Err return the same error.
// If Done is not yet closed, Err returns nil.
// If Done is closed, Err returns a non-nil error explaining why:
// Canceled if the context was canceled (via server Shutdown)
// or DeadlineExceeded if the context's deadline passed.
//
// Note: Because creating a new channel for every request is just too expensive, so
// RequestCtx.s.done is only closed when the server is shutting down.
func (ctx *RequestCtx) Err() error {
	select {
	case <-ctx.Done():
		return context.Canceled
	default:
		return nil
	}
}

// Value returns the value associated with this context for key, or nil
// if no value is associated with key. Successive calls to Value with
// the same key returns the same result.
//
// This method is present to make RequestCtx implement the context interface.
// This method is the same as calling ctx.UserValue(key).
func (ctx *RequestCtx) Value(key any) any {
	return ctx.UserValue(key)
}

func (ctx *RequestCtx) SetResponseWriter(rw func(w WriterFlusherCloser) error) {
	ctx.w = rw
}

var fakeServer = &Server{}

type fakeAddrer struct {
	net.Conn
	laddr net.Addr
	raddr net.Addr
}

func (fa *fakeAddrer) RemoteAddr() net.Addr {
	return fa.raddr
}

func (fa *fakeAddrer) LocalAddr() net.Addr {
	return fa.laddr
}

func (fa *fakeAddrer) Read(_ []byte) (int, error) {
	// developer sanity-check
	panic("BUG: unexpected Read call")
}

func (fa *fakeAddrer) Write(_ []byte) (int, error) {
	// developer sanity-check
	panic("BUG: unexpected Write call")
}

func (fa *fakeAddrer) Close() error {
	// developer sanity-check
	panic("BUG: unexpected Close call")
}

func (s *Server) releaseCtx(ctx *RequestCtx) {
	ctx.reset()
	s.ctxPool.Put(ctx)
}

func (s *Server) getServerName() string {
	serverName := s.Name
	if serverName == "" {
		if !s.NoDefaultServerHeader {
			serverName = defaultServerName
		}
	}
	return serverName
}

/*
func defaultErrorHandler(ctx *RequestCtx, err error) {
		if err == HeaderBufferSmallErr {
			ctx.Error("Too big request header", StatusRequestHeaderFieldsTooLarge)
		} else if //goland:noinspection GoTypeAssertionOnErrors
		netErr, ok := err.(*net.OpError); ok && netErr.Timeout() {
			ctx.Error("Request timeout", StatusRequestTimeout)
		} else {
			ctx.Error("Error when parsing request", StatusBadRequest)
		}
}
*/
/*
func (s *Server) writeErrorResponse(bw *bufio.Writer, ctx *RequestCtx, serverName string, err error) *bufio.Writer {
	errorHandler := defaultErrorHandler
	if s.ErrorHandler != nil {
		errorHandler = s.ErrorHandler
	}
	errorHandler(ctx, err)

	if serverName != "" {
		ctx.Response.Header.SetServer(serverName)
	}
	ctx.SetConnectionClose()
	if bw == nil {
		bw = acquireWriter(ctx)
	}
	//goland:noinspection GoUnhandledErrorResult
	writeResponse(ctx, bw) //nolint:errcheck
	ctx.Response.Reset()
	//goland:noinspection GoUnhandledErrorResult
	bw.Flush()
	return bw
}
*/

// trackConn here only handle StateNew \ StateHijacked \ StateClosed
func (s *Server) trackConn(c net.Conn, state ConnState) {
	switch state {
	case StateNew:
		cs := ConnStatus{createTime: absoluteNano()}
		cs.status.Store(int32(StateIdle))
		cs.lastActive.Store(absoluteNano() + int64(time.Second*5))
		s.conns.Store(c, &cs)
	default:
		// StateHijacked StateClosed delete it from s.conns.
		s.conns.Delete(c)
	}
}

func (s *Server) closeIdleConns() {
	now := absoluteNano()
	s.conns.Range(func(c net.Conn, cs *ConnStatus) bool {
		if cs.status.Load() == int32(StateIdle) {
			if now-cs.lastActive.Load() >= 0 {
				_ = c.Close()
				s.conns.Delete(c)
			}
		}
		return true
	})
}

type CloseReason int32

const (
	_ CloseReason = iota
	ReadTimeout
	WriteTimeout
	IdleTimeout
	ShutDownTimeout
	ShutDownNormal
)

// A ConnState represents the state of a client connection to a server.
// It's used by the optional Server.ConnState hook.
type ConnState int32

const (
	// StateNew represents a new connection that is expected to
	// send a request immediately. Connections begin at this
	// state and then transition to either StateActive or
	// StateClosed.
	StateNew ConnState = iota

	// StateActiveR  represents a connection that has read 1 or more
	// bytes of a request. The Server.ConnState hook for
	// StateActive fires before the request has entered a handler
	// and doesn't fire again until the request has been
	// handled. After the request is handled, the state
	// transitions to StateClosed, StateHijacked, or StateIdle.
	// For HTTP/2, StateActive fires on the transition from zero
	// to one active request, and only transitions away once all
	// active requests are complete. That means that ConnState
	// cannot be used to do per-request work; ConnState only notes
	// the overall state of the connection.
	StateActiveR
	StateActiveW

	// StateIdle represents a connection that has finished
	// handling a request and is in the keep-alive state, waiting
	// for a new request. Connections transition from StateIdle
	// to either StateActive or StateClosed.
	StateIdle

	// StateHijacked represents a hijacked connection.
	// This is a terminal state. It does not transition to StateClosed.
	StateHijacked

	// StateClosed represents a closed connection.
	// This is a terminal state. Hijacked connections do not
	// transition to StateClosed.
	StateClosed
)

var stateName = map[ConnState]string{
	StateNew:      "new",
	StateActiveR:  "active read",
	StateActiveW:  "active write",
	StateIdle:     "idle",
	StateHijacked: "hijacked",
	StateClosed:   "closed",
}

func (c ConnState) String() string {
	return stateName[c]
}

func createTCPListener(network, addr string, keepAlivePeriod time.Duration, keepAlive bool) (ln net.Listener, err error) {
	var lc net.ListenConfig
	if keepAlive {
		if keepAlivePeriod < 0 {
			keepAlivePeriod = 0
		}
		lc.KeepAlive = keepAlivePeriod
	} else {
		lc.KeepAlive = -1
	}
	ln, err = lc.Listen(context.Background(), network, addr)
	return
}

// isCommonNetReadError reports whether err is a common error
// encountered during reading a request off the network when the
// client has gone away or had its read fail somehow. This is used to
// determine which logs are interesting enough to log about.
//
//goland:noinspection GoTypeAssertionOnErrors
func isCommonNetReadError(err error) bool {
	if err == io.EOF {
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}
	if oe, ok := err.(*net.OpError); ok && oe.Op == "read" {
		return true
	}
	if oe, ok := err.(*net.OpError); ok && oe.Op == "write" {
		return true
	}
	return false
}

const errorHeaders = "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n"

func errHTTPResponseStr(statusCode int, body string, extraHeaders []string) (resp string) {
	if len(body) == 0 {
		body = http.StatusText(statusCode)
	}
	var (
		extraHeadersStr string
	)
	if len(extraHeaders) != 0 {
		extraHeadersStr = "\r\n" + strings.Join(extraHeaders, "\r\n")
	}
	resp = fmt.Sprintf("HTTP/1.1 %d %s%s%s%d %s", statusCode, http.StatusText(statusCode), extraHeadersStr, errorHeaders, statusCode, body)
	return
}

var concurrencyLimitErr = errHTTPResponseStr(StatusServiceUnavailable, "The server is currently temporary overloading", []string{"Retry-After: 10"})
var conPerIpLimitErr = errHTTPResponseStr(StatusTooManyRequests, "The number of connections from your ip over limit", nil)
var commonRequestErr = errHTTPResponseStr(StatusBadRequest, "", nil)
var httpToHttpsErr = errHTTPResponseStr(StatusBadRequest, "Client sent an HTTP request to an HTTPS server.", nil)

var statusHTTPVersionNotSupportedErr = errHTTPResponseStr(StatusHTTPVersionNotSupported, "", nil)
var statusNotImplementedErr = errHTTPResponseStr(StatusNotImplemented, "Unsupported transfer encoding", nil)
var statusRequestEntityTooLargeErr = errHTTPResponseStr(StatusRequestEntityTooLarge, "", nil)
var statusRequestHeaderFieldsTooLargeErr = errHTTPResponseStr(StatusRequestHeaderFieldsTooLarge, "", nil)
var statusMethodNotAllowedErr = errHTTPResponseStr(StatusMethodNotAllowed, "", nil)

// tlsRecordHeaderLooksLikeHTTP reports whether a TLS record header
// looks like it might've been a misdirected plaintext HTTP request.
func tlsRecordHeaderLooksLikeHTTP(hdr [5]byte) bool {
	switch string(hdr[:]) {
	case "GET /", "HEAD ", "POST ", "PUT /", "OPTIO":
		return true
	}
	return false
}
