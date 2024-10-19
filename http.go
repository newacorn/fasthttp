package fasthttp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	pbufio "github.com/newacorn/goutils/bufio"
	pio "github.com/newacorn/goutils/io"
	pool "github.com/newacorn/simple-bytes-pool"
	"github.com/valyala/bytebufferpool"
	"io"
	"mime/multipart"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	ucompress "utils/compress"
)

var (
	// When performing the `Reset` operation, if the capacity of the underlying byte
	// slice in `Request.body` exceeds this value, the slice will not be recycled
	// but will be set to `nil`.
	//
	// It is only effective when the value of this field is greater than 0.
	requestBodyPoolSizeLimit = -1
	// It functions the same as the above field but applies to the response.
	responseBodyPoolSizeLimit = -1
)

// For the server, this field is used when the request body size is 0 and the
// request is being read in a streaming manner.
//
// For the client, this field is used when the response body size is 0 and the
// response is being read in a streaming manner.
var emptyRequestBodyStream = strings.NewReader("")

// SetBodySizePoolLimit set the max body size for bodies to be returned to the pool.
// If the body size is larger it will be released instead of put back into the pool for reuse.
// When the Body of a Request and Response is reset, if the underlying slice's capacity exceeds
// the set value, it will not be returned to the cache pool but will instead be set to nil.
//
// A value of 0 or negative indicates that it will always be released to the cache pool.
func SetBodySizePoolLimit(reqBodyLimit, respBodyLimit int) {
	requestBodyPoolSizeLimit = reqBodyLimit
	responseBodyPoolSizeLimit = respBodyLimit
}

// This type is used only for the client.
// cliInfo is used to configure read/write timeouts and retry mechanisms for a single request.
//
// Request writeTimeout
// if readTimeout == 0, use HostClient.WriteTimeout
//
// Response readTimeout
// if writeTimeout == 0, use HostClient.WriteTimeout
//
// TLS handshake is Response readTimeout
type cliInfo struct {
	// A negative value indicates no timeout when reading the response.
	// Zero value indicates using the value of the corresponding field from `HostClient` or `Client`.
	readTimeout time.Duration
	// A negative value indicates no timeout when sending the request.
	// Zero value indicates using the value of the corresponding field from `HostClient` or `Client`.
	writeTimeout time.Duration
	// Zero value or negative value indicates using the corresponding value from `HostClient` or `Client`.
	// Including the first request
	// If maxAttempts is set to 1, the request will only be executed once, regardless of any other conditions.
	maxAttempts int
	// This function will be called when a request encounters an error and there are remaining attempts,
	// to determine whether to retry.
	retryIf RetryIfErrFunc
}

// Request represents HTTP request.
// It can be used to represent a request both on the server side and on the client side.
//
// It is forbidden copying Request instances. Create new instances
// and use CopyTo instead.
//
// Request instance MUST NOT be used from concurrently running goroutines.
//
// Client:
// To send a `multipart/form-data` request, only set `multipartForm` and its fields.
// To send an `application/x-www-form-urlencoded` request, only set the `postArgs` field.
// For the above two request types, the `Content-Type` request header will be set automatically.
// Other request type's Content-Type must be set manually.
type Request struct {
	noCopy noCopy

	// Request header.
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header RequestHeader

	// Typically includes the full URL.
	// may only contain uri: path+query
	uri URI

	// Priority of sending content: bodyStream bodyRaw body multipartForm postArgs
	//
	// Set through the `SetBodyStream` method in Client.
	//
	// Server: The server reads the request body as a stream, when StreamBody is true or Server.StreamRequestBody is true.
	//
	// Client: if this field is not nil and Content-Length is -1, the client sends the request body in a streaming manner.
	bodyStream io.Reader

	// Set through the `SetBodyRaw` method, with the same reference as the method's parameter.
	// Other send request body methods will copy content.
	//
	// This field is not used by the server.
	bodyRaw []byte

	// Append the `AppendBody`; Set through `SetBody` methods.
	// For the server, the request body will be partially or fully stored in this field.
	//
	// Body parameter is appended this filed.
	body *bytebufferpool.ByteBuffer

	// Used to store the body of `multipart/form-data` request type.
	// Automatically set the request type(Content-Type) for this kind of request body in Client.
	multipartForm *multipart.Form

	// Used to store the body of `application/x-www-form-urlencoded` request type.
	// Automatically set the request type(Content-Type) for this kind of request body in Client.
	postArgs Args

	multipartFormBoundary string

	// Request timeout. Usually set by DoDeadline or DoTimeout
	// if <= 0, means not set
	// Control the time from the start to the end of a request(writing request's and reading response's body), including all retries.
	// Priority is lower than the corresponding fields in `cliInfo`, `Client`, and `HostClient`.
	timeout time.Duration

	// Used only for the server.
	// Inherit from `Server.MaxRequestBodySize`, set during request handling,
	//
	// Also to limit the length of converting the streamable request body to bytes slice content when using the `Body` method.
	//
	// If the request body is parsed in a streaming manner, request's body is not affected by this field.
	maxBodySize int64

	// Configure client request information
	cliInfo *cliInfo

	// Set in the `Server.serveConn` method, inheriting from `Server.SecureErrorLogMessage`.
	// Set in the `HostClient.doNonNilReqResp` method, inheriting from `HostClient.secureErrorLogMessage`.
	//
	// When an error is encountered while parsing the request headers,
	// the log will not include the request body content.
	secureErrorLogMessage bool

	// Each time the SetRequestURI method is called, this field is set to false.
	//
	// When setting the uri field through the SetURI method, this field's value is set to true.
	//
	// Each time the uri is obtained through URI, if this field is `false`,
	// it will parse the `uri` using the `RequestHeader.host` and `RequestHeader.requestURI` fields.
	// After completion, this field is set to `true`.
	parsedURI bool

	// Used only for the server. When the request type is `application/x-www-form-urlencoded`
	// and has been parsed, this field is set to `true`.
	//
	// This field prevents duplicate parsing.
	parsedPostArgs bool

	// When `Request`'s Body is reset, whether to return the underlying byte slices of the `body`
	// field to the cache pool.
	//
	// For the server, this field is `true` only when the `Server.ReduceMemoryUsage` field is set to `true`.
	keepBodyBuffer bool

	// Used by Server to indicate the request was received on an HTTPS endpoint.
	//
	// Client/HostClient shouldn't use this field but should depend on the uri.scheme instead.
	// Set in the `Server.serveConn` method, configured based on whether the transport protocol is TLS.
	//
	// The client infers from `RequestURI`.
	isTLS bool

	// Use Host header (request.Header.SetHost) instead of the host from SetRequestURI, SetHost, or URI().SetHost
	//
	// When this field is `true` and `RequestHeader.host` is not empty and parsedURI is true,
	// use `RequestHeader.host` as the value for the `Host` request header.
	//
	// When `RequestHeader.host` is empty and parsedURI is true, use uri's Host.
	//
	// Otherwise, use RequestHeader.host.
	UseHostHeader bool

	// Whether to format the request path when using the redirect path obtained from the `Location` header.
	// Used only for the client, only in doRequestFollowRedirects function,
	DisableRedirectPathNormalizing bool

	// Used only for the server. When closing the request's bodyStream, whether to discard the content
	// of the streaming request body to avoid disrupting keep-alive connections and
	// interfering with the reading of the next request.
	// Inherits the value from the `Server.DiscardUnReadRequestBodyStream` field.
	DiscardUnReadBodyStream bool

	// Whether the request body is read in a streaming manner is applicable only to the server side.
	StreamBody bool
}

// Response represents HTTP response.
//
// In the client, it represents the content of the response corresponding to the request.
// In the server, it represents the content of the response to the request.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo instead.
//
// Response instance MUST NOT be used from concurrently running goroutines.
type Response struct {
	noCopy noCopy

	// Response header.
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header ResponseHeader

	// When sending a response, obtain the response body content in the following order:
	// w bodyStream bodyRaw body
	bodyStream io.Reader
	// Referencing the response body parameter does not append (copy).
	bodyRaw []byte
	body    *bytebufferpool.ByteBuffer
	// Fill the response body using the io.Writer interface.
	// w responseBodyWriter
	//
	// The contents written to the parameter `w` are always sent to the client
	// using chunked encoding.
	chunkedW func(w WriterFlusherCloser) error
	// Client address.
	raddr net.Addr
	// Server address.
	laddr net.Addr
	// If this field is not empty, compress the response body using this compression algorithm.
	compressPool ucompress.Pooler
	// Used for the server.
	// After writing the response headers to the `bw` buffer, is `Flush` executed immediately?
	ImmediateHeaderFlush bool

	// Used only for the client.
	// Whether to read the response body as a stream
	StreamBody bool

	// Response.Read() skips reading body if set to true.
	// Use it for reading HEAD responses.
	//
	// Response.Write() skips writing body if set to true.
	// Use it for writing HEAD responses.
	//
	// For the server, if the request method is `HEAD`, this field will be set to `true`
	// in the `Server.serveConn` method.
	// For the client, if the request is a `HEAD` method, this field will automatically be set to `true`.
	// In transport.RoundTrip method.
	SkipBody bool

	// When resetting the `Response` body, whether to return the underlying byte slices to the cache pool.
	// Reset method will not set this field to false.
	keepBodyBuffer bool
	// Set in the `Server.serveConn` method, inheriting from `Server.SecureErrorLogMessage`.
	// Set in the `HostClient.doNonNilReqResp` method, inheriting from `HostClient.secureErrorLogMessage`.
	secureErrorLogMessage bool
}

// RetryIf Used by the client to determine whether to retry when an error occurs while sending a request.
// Subject to the remaining attempts included in `MaxAttempts`.
func (req *Request) RetryIf() RetryIfErrFunc {
	if req.cliInfo == nil {
		return nil
	}
	return req.cliInfo.retryIf
}

func (req *Request) SetRetryIf(f RetryIfErrFunc) {
	if req.cliInfo == nil {
		req.cliInfo = &cliInfo{retryIf: f}
	}
	req.cliInfo.retryIf = f
}

// MaxAttempts The maximum number of attempts the client can make when sending a request,
// with the first send counting as one attempt.
func (req *Request) MaxAttempts() int {
	if req.cliInfo == nil {
		return 0
	}
	return req.cliInfo.maxAttempts
}

func (req *Request) SetMaxAttempts(attempts int) {
	if req.cliInfo == nil {
		req.cliInfo = &cliInfo{
			maxAttempts: attempts,
		}
		return
	}
	req.cliInfo.maxAttempts = attempts
}

func (req *Request) WriteTimeout() time.Duration {
	if req.cliInfo == nil {
		return 0
	}
	return req.cliInfo.writeTimeout
}

func (req *Request) ReadTimeout() time.Duration {
	if req.cliInfo == nil {
		return 0
	}
	return req.cliInfo.readTimeout
}

// SetWriteTimeout Timeout for the client when sending the response body.
// A negative value indicates no timeout, and a zero value indicates using the corresponding value
// from `HostClient` or `Client`.
func (req *Request) SetWriteTimeout(timeout time.Duration) {
	if req.cliInfo == nil {
		req.cliInfo = &cliInfo{
			writeTimeout: timeout,
		}
		return
	}
	req.cliInfo.writeTimeout = timeout
}

// SetReadTimeout Timeout for the client when reading the response body.
// A negative value indicates no timeout, and a zero value indicates using the corresponding value
// from `HostClient` or `Client`.
func (req *Request) SetReadTimeout(timeout time.Duration) {
	if req.cliInfo == nil {
		req.cliInfo = &cliInfo{
			readTimeout: timeout,
		}
		return
	}
	req.cliInfo.readTimeout = timeout
}

// SetHost sets host for the request.
func (req *Request) SetHost(host string) {
	req.URI().SetHost(host)
}

// SetHostBytes sets host for the request.
func (req *Request) SetHostBytes(host []byte) {
	req.URI().SetHostBytes(host)
}

// Host returns the host for the given request.
func (req *Request) Host() []byte {
	return req.URI().Host()
}

// SetRequestURI sets RequestURI.
// Only applicable to the client.
func (req *Request) SetRequestURI(requestURI string) {
	req.Header.SetRequestURI(requestURI)
	req.parsedURI = false
}

// SetRequestURIBytes sets RequestURI.
// Only applicable to the client.
func (req *Request) SetRequestURIBytes(requestURI []byte) {
	req.Header.SetRequestURIBytes(requestURI)
	req.parsedURI = false
}

// RequestURI returns request's URI.
func (req *Request) RequestURI() []byte {
	if req.parsedURI {
		requestURI := req.uri.RequestURI()
		req.SetRequestURIBytes(requestURI)
	}
	return req.Header.RequestURI()
}

// StatusCode returns response status code.
func (resp *Response) StatusCode() int {
	return resp.Header.StatusCode()
}

// SetStatusCode sets response status code.
func (resp *Response) SetStatusCode(statusCode int) {
	resp.Header.SetStatusCode(statusCode)
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (resp *Response) ConnectionClose() bool {
	return resp.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (resp *Response) SetConnectionClose() {
	resp.Header.SetConnectionClose()
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (req *Request) ConnectionClose() bool {
	return req.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (req *Request) SetConnectionClose() {
	req.Header.SetConnectionClose()
}

// SendFile registers file on the given path to be used as response body
// when Write is called.
//
// Note that SendFile doesn't set Content-Type, so set it yourself
// with Header.SetContentType.
func (resp *Response) SendFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		//goland:noinspection GoUnhandledErrorResult
		f.Close()
		return err
	}
	resp.Header.SetLastModified(fileInfo.ModTime())
	resp.SetBodyStream(f, fileInfo.Size())
	return nil
}

// SetBodyStream sets request body stream and, optionally body size.
//
// If bodySize is >= 0, then the bodyStream must provide exactly bodySize bytes
// before returning io.EOF.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
//
// Note that GET and HEAD requests cannot have body.
//
// See also SetBodyStreamWriter.
func (req *Request) SetBodyStream(bodyStream io.Reader, bodySize int64) {
	req.ResetBody()
	req.bodyStream = bodyStream
	req.Header.SetContentLength(bodySize)
}

// SetBodyStream sets response body stream and, optionally body size.
//
// If bodySize is >= 0, then the bodyStream must provide exactly bodySize bytes
// before returning io.EOF.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
//
// See also SetBodyStreamWriter.
func (resp *Response) SetBodyStream(bodyStream io.Reader, bodySize int64) {
	resp.ResetBody()
	resp.bodyStream = bodyStream
	resp.Header.SetContentLength(bodySize)
}

// IsBodyStream returns true if body is set via SetBodyStream*.
func (req *Request) IsBodyStream() bool {
	return req.bodyStream != nil
}

// IsBodyStream returns true if body is set via SetBodyStream*.
func (resp *Response) IsBodyStream() bool {
	return resp.bodyStream != nil
}

// SetBodyStreamWriter registers the given sw for populating request body.
//
// This function may be used in the following cases:
//
//   - if request body is too big (more than 10MB).
//   - if request body is streamed from slow external sources.
//   - if request body must be streamed to the server in chunks
//     (aka `http client push` or `chunked transfer-encoding`).
//
// Note that GET and HEAD requests cannot have body.
//
// See also SetBodyStream.
func (req *Request) SetBodyStreamWriter(sw StreamWriter) {
	sr := NewStreamReader(sw)
	req.SetBodyStream(sr, -1)
}

// SetBodyStreamWriter registers the given sw for populating response body.
//
// This function may be used in the following cases:
//
//   - if response body is too big (more than 10MB).
//   - if response body is streamed from slow external sources.
//   - if response body must be streamed to the client in chunks
//     (aka `http server push` or `chunked transfer-encoding`).
//
// See also SetBodyStream.
func (resp *Response) SetBodyStreamWriter(sw StreamWriter) {
	sr := NewStreamReader(sw)
	resp.SetBodyStream(sr, -1)
}

// SetBodyStreamChunkedWriter We will use chunked encoding no matter what.
func (resp *Response) SetBodyStreamChunkedWriter(sw StreamChunkedWriter) {
	resp.ResetBody()
	resp.Header.SetContentLength(-1)
	resp.chunkedW = sw
}

func (resp *Response) WriteHeader(w *bufio.Writer) (err error) {
	err = resp.Header.Write(w)
	if err == nil {
		if resp.ImmediateHeaderFlush {
			err = w.Flush()
		}
	}
	return
}

// WriteChunkedStream We will use chunked encoding no matter what.
func (resp *Response) WriteChunkedStream(w *bufio.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &WriteBodyStreamPanic{
				error: fmt.Errorf("panic while writing chunked stream: %+v", r),
			}
		}
	}()
	// We will set ContentLength to -1 no matter what.
	resp.Header.SetContentLength(-1)
	err = resp.WriteHeader(w)
	dontSendBody := resp.mustSkipBody()
	if err != nil || dontSendBody {
		return
	}
	// Next, it will be the path where the response body is sent.
	// user data -> bufW -> bw -> net.Conn
	var (
		cw   = ChunkWriter{Writer: w}
		bufW = pbufio.NewWriter(cw)
		// wf cache written data, when flush a chunked generate.
		wf           WriterFlusher = bufW
		compressW    ucompress.Writer
		compressPool ucompress.Pooler
	)
	if resp.needCompress() {
		compressPool = resp.compressPool
		compressW = compressPool.Get()
		if compressPool.NeedBuffer() {
			// For Gzip compression, because the output compressed blocks are relatively small,
			// caching is used here for performance reasons.
			bcw := BufferedCompressWriter{bufW}
			compressW.Reset(bcw)
			wf = CompressWriter{Writer: compressW, wf: bcw}
		} else {
			compressW.Reset(cw)
			wf = compressW
		}
	}
	//
	rw := ResponseStreamWriter{
		WriterFlusher: wf, // user data write here. data in wf is flushed into cw.
		cw:            cw, // used for flush chunked data to net.conn
	}
	err = resp.chunkedW(rw)
	// flush buffer writer and close compress writer if exists.
	err2 := rw.Close()
	// now only bw has unflushed data.
	if err == nil {
		err = err2
	}
	// At this point, all responses are fully encoded in Chunked format in `bw`,
	// and there are no remaining residues in any intermediate cache.
	// recycle resource.

	// whatever recycle resource.
	if compressPool != nil {
		compressPool.Put(compressW)
	}
	if bufW != nil {
		// Recycle the resources occupied by the compression algorithms.
		// In the case of no compression, release the cache used to store the user response.
		bufW.RecycleItems()
	}
	if err != nil {
		return
	}
	// write zero chunked to bw.
	_, err = w.Write(chunkedEnd)
	if err != nil {
		return
	}
	err = resp.Header.writeTrailer(w)
	if err != nil {
		return
	}
	// Flush the contents from the cache(bw) to net.Conn.
	err = w.Flush()
	return
}

// BodyWriter returns writer for populating response body.
//
// If used inside RequestHandler, the returned writer must not be used
// after returning from RequestHandler. Use RequestCtx.Write
// or SetBodyStreamWriter in this case.
func (resp *Response) BodyWriter() io.Writer {
	return responseBodyWriter{r: resp}
}

// BodyStream returns io.Reader.
//
// You must CloseBodyStream or ReleaseRequest after you use it.
func (req *Request) BodyStream() io.Reader {
	return req.bodyStream
}

func (req *Request) CloseBodyStream() error {
	return req.closeBodyStream()
}

// BodyStream returns io.Reader.
//
// You must CloseBodyStream or ReleaseResponse after you use it.
func (resp *Response) BodyStream() io.Reader {
	return resp.bodyStream
}

func (resp *Response) CloseBodyStream() error {
	return resp.closeBodyStream(nil)
}

// ReadCloserWithError Used by the client, when processing a response as a stream.
// If an error is detected when closing the stream, the `CloseWithError` method is
// called to determine whether to release or close the underlying connection.
type ReadCloserWithError interface {
	io.Reader
	CloseWithError(err error) error
}

type closeReader struct {
	io.Reader
	closeFunc func(err error) error
}

func newCloseReaderWithError(r io.Reader, closeFunc func(err error) error) ReadCloserWithError {
	if r == nil {
		panic(`BUG: reader is nil`)
	}
	return &closeReader{Reader: r, closeFunc: closeFunc}
}

func (c *closeReader) CloseWithError(err error) error {
	if c.closeFunc == nil {
		return nil
	}
	return c.closeFunc(err)
}

// BodyWriter Write the request body into the `Request.body`
// field using the `io.Writer` interface.
func (req *Request) BodyWriter() io.Writer {
	return requestBodyWriter{req}
}

type responseBodyWriter struct {
	r *Response
}

func (w responseBodyWriter) Write(p []byte) (int, error) {
	w.r.AppendBody(p)
	return len(p), nil
}

type requestBodyWriter struct {
	r *Request
}

func (w requestBodyWriter) Write(p []byte) (int, error) {
	w.r.AppendBody(p)
	return len(p), nil
}

func (resp *Response) parseNetConn(conn net.Conn) {
	resp.raddr = conn.RemoteAddr()
	resp.laddr = conn.LocalAddr()
}

// RemoteAddr returns the remote network address. The Addr returned is shared
// by all invocations of RemoteAddr, so do not modify it.
func (resp *Response) RemoteAddr() net.Addr {
	return resp.raddr
}

// LocalAddr returns the local network address. The Addr returned is shared
// by all invocations of LocalAddr, so do not modify it.
func (resp *Response) LocalAddr() net.Addr {
	return resp.laddr
}

// Body returns response body.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to returned value. Make copies instead.
//
// Return the request body in the following order: `bodyStream`, `bodyRaw`, `body` field.
// The returned body is a reference and remains valid until the body in the `resp` is modified.
// If an error occurs while closing the body stream, the error is returned as the content of the body.
//
// The `bodyStream` will always be closed, so it is recommended to handle `bodyStream` separately.
// it may result in a memory overflow.
//
// Each time the result is the same, it will be cached in the `body` field.
func (resp *Response) Body() (body []byte) {
	body, err := resp.BodyWithError()
	if err != nil {
		//goland:noinspection GoUnhandledErrorResult
		resp.bodyBuffer().WriteString(err.Error())
		body = resp.body.B
	}
	return
}

// BodyWithError Return the request body in the following order: `bodyStream`, `bodyRaw`, `body` field.
// The returned body is a reference and remains valid until the body in the `resp` is modified.
// If an error occurs while closing the body stream
//
// The `bodyStream` will always be closed, so it is recommended to handle `bodyStream` separately.
// it may result in a memory overflow.
//
// Each time the result is the same, it will be cached in the `body` field.
func (resp *Response) BodyWithError() (data []byte, err error) {
	if resp.bodyStream != nil {
		bodyBuf := resp.bodyBuffer()
		bodyBuf.Reset()
		_, err = copyZeroAlloc(bodyBuf, resp.bodyStream)
		er := resp.closeBodyStream(err) //nolint:errcheck
		if err == nil {
			err = er
		}
		if err != nil {
			return
		}
	}
	data = resp.bodyBytes()
	return
}

// Exclude body stream.
func (resp *Response) bodyBytes() []byte {
	if resp.bodyRaw != nil {
		return resp.bodyRaw
	}
	if resp.body == nil {
		return nil
	}
	return resp.body.B
}

// Body returns request body.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
// Return the request body in the following order: `bodyStream`, `bodyRaw`, `body` field.
// Include multipart/form-data body and x-www-form-urlencoded/application form body.
// 与Server.SetRequestBodyStream搭配使用时，Body()最多返回Server.MaxRequestBodySize个字节。
//
// The returned body is a reference and remains valid until the body in the `Request` is modified.
// If an error occurs while closing the body stream, the error is returned as the content of the body.
//
// The `bodyStream` will always be closed, so it is recommended to handle `bodyStream` separately.
// If `maxBodySize` is not set, it may result in a memory overflow.
//
// Each time the result is the same, it will be cached in the `body` field.
func (req *Request) Body() (body []byte) {
	body, err := req.BodyWithError()
	if err != nil {
		//goland:noinspection GoUnhandledErrorResult
		req.bodyBuffer().WriteString(err.Error())
		body = req.body.B
	}
	return
}

// BodyWithError Body returns request body.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
// Return the request body in the following order: `bodyStream`, `bodyRaw`, `body` field.
// Include multipart/form-data body and x-www-form-urlencoded/application form body.
// 与Server.SetRequestBodyStream搭配使用时，Body()最多返回Server.MaxRequestBodySize个字节。
//
// The returned body is a reference and remains valid until the body in the `Request` is modified.
// If an error occurs while closing the body stream.
//
// If the `bodyStream` field is not empty, the size of the content read is limited by the `maxBodySize` field.
//
// The `bodyStream` will always be closed, so it is recommended to handle `bodyStream` separately.
// If `maxBodySize` is not set, it may result in a memory overflow.
//
// Each time the result is the same, it will be cached in the `body` field.
func (req *Request) BodyWithError() (body []byte, err error) {
	if req.bodyStream != nil {
		bodyBuf := req.bodyBuffer()
		bodyBuf.Reset()
		s := req.bodyStream
		if req.maxBodySize > 0 {
			s = io.LimitReader(req.bodyStream, req.maxBodySize)
		}
		_, err = copyZeroAlloc(bodyBuf, s)
		er := req.closeBodyStream() //nolint:errcheck
		if err == nil {
			err = er
		}
		body = req.bodyBytes()
		return
	}
	//
	body = req.bodyBytes()
	if len(body) != 0 {
		return
	}
	//
	if req.onlyMultipartForm() {
		bodyBuf := requestBodyPool.Get()
		err = WriteMultipartForm(bodyBuf, req.multipartForm, req.multipartFormBoundary)
		body = bodyBuf.B
		return
	}
	body = req.PostArgs().QueryString()
	return
}

func (req *Request) bodyBytes() []byte {
	if req.bodyRaw != nil {
		return req.bodyRaw
	}
	if req.body == nil {
		return nil
	}
	return req.body.B
}

func (resp *Response) bodyBuffer() *bytebufferpool.ByteBuffer {
	if resp.body == nil {
		resp.body = responseBodyPool.Get()
	}
	resp.bodyRaw = nil
	return resp.body
}

func (req *Request) bodyBuffer() *bytebufferpool.ByteBuffer {
	if req.body == nil {
		req.body = requestBodyPool.Get()
	}
	req.bodyRaw = nil
	return req.body
}

var (
	responseBodyPool bytebufferpool.Pool
	requestBodyPool  bytebufferpool.Pool
)

// BodyGunzip returns un-gzipped body data.
//
// This method may be used if the request header contains
// 'Content-Encoding: gzip' for reading un-gzipped body.
//
// Use Body for reading gzipped request body.
// The `body` content will always be decompressed, regardless of the `Content-Encoding`.
func (req *Request) BodyGunzip() ([]byte, error) {
	return gunzipData(req.Body())
}

// BodyGunzip returns un-gzipped body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: gzip' for reading un-gzipped body.
// Use Body for reading gzipped response body.
//
// The `body` content will always be decompressed, regardless of the `Content-Encoding`.
func (resp *Response) BodyGunzip() ([]byte, error) {
	return gunzipData(resp.Body())
}

func gunzipData(p []byte) ([]byte, error) {
	var bb bytebufferpool.ByteBuffer
	_, err := WriteGunzip(&bb, p)
	if err != nil {
		return nil, err
	}
	return bb.B, nil
}

// BodyUnbrotli returns un-brotlied body data.
//
// This method may be used if the request header contains
// 'Content-Encoding: br' for reading un-brotlied body.
// Use Body for reading brotlied request body.
//
// The `body` content will always be decompressed, regardless of the `Content-Encoding`.
func (req *Request) BodyUnbrotli() ([]byte, error) {
	return unBrotliData(req.Body())
}

// BodyUnbrotli returns un-brotlied body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: br' for reading un-brotlied body.
// Use Body for reading brotlied response body.
//
// The `body` content will always be decompressed, regardless of the `Content-Encoding`.
func (resp *Response) BodyUnbrotli() ([]byte, error) {
	return unBrotliData(resp.Body())
}

func unBrotliData(p []byte) ([]byte, error) {
	var bb bytebufferpool.ByteBuffer
	_, err := WriteUnbrotli(&bb, p)
	if err != nil {
		return nil, err
	}
	return bb.B, nil
}

// BodyInflate returns inflated body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: deflate' for reading inflated request body.
// Use Body for reading deflated request body.
func (req *Request) BodyInflate() ([]byte, error) {
	return inflateData(req.Body())
}

// BodyInflate returns inflated body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: deflate' for reading inflated response body.
// Use Body for reading deflated response body.
func (resp *Response) BodyInflate() ([]byte, error) {
	return inflateData(resp.Body())
}

func (ctx *RequestCtx) RequestBodyStream() io.Reader {
	return ctx.Request.bodyStream
}

func (req *Request) BodyUnzstd() ([]byte, error) {
	return unzstdData(req.Body())
}

// BodyUnzstd The body content will always be decompressed, regardless of the Content-Encoding.
func (resp *Response) BodyUnzstd() ([]byte, error) {
	return unzstdData(resp.Body())
}

func unzstdData(p []byte) ([]byte, error) {
	var bb bytebufferpool.ByteBuffer
	_, err := WriteUnzstd(&bb, p)
	if err != nil {
		return nil, err
	}
	return bb.B, nil
}

func inflateData(p []byte) ([]byte, error) {
	var bb bytebufferpool.ByteBuffer
	_, err := WriteInflate(&bb, p)
	if err != nil {
		return nil, err
	}
	return bb.B, nil
}

var ErrContentEncodingUnsupported = errors.New("unsupported Content-Encoding")

// BodyUncompressed returns body data and if needed decompress it from gzip, deflate or Brotli.
//
// This method may be used if the response header contains
// 'Content-Encoding' for reading uncompressed request body.
// Use Body for reading the raw request body.
func (req *Request) BodyUncompressed() ([]byte, error) {
	switch string(req.Header.ContentEncoding()) {
	case "":
		return req.Body(), nil
	case "deflate":
		return req.BodyInflate()
	case "gzip":
		return req.BodyGunzip()
	case "br":
		return req.BodyUnbrotli()
	case "zstd":
		return req.BodyUnzstd()
	default:
		return nil, ErrContentEncodingUnsupported
	}
}

// BodyUncompressed returns body data and if needed decompress it from gzip, deflate or Brotli.
//
// This method may be used if the response header contains
// 'Content-Encoding' for reading uncompressed response body.
// Use Body for reading the raw response body.
func (resp *Response) BodyUncompressed() ([]byte, error) {
	switch string(resp.Header.ContentEncoding()) {
	case "":
		return resp.Body(), nil
	case "deflate":
		return resp.BodyInflate()
	case "gzip":
		return resp.BodyGunzip()
	case "br":
		return resp.BodyUnbrotli()
	case "zstd":
		return resp.BodyUnzstd()
	default:
		return nil, ErrContentEncodingUnsupported
	}
}

// BodyWriteTo writes request Body() to w.
// In bodyStream multipart/form-data rawBody body x-www-form-urlencoded/application orders.
func (req *Request) BodyWriteTo(w io.Writer) (err error) {
	if req.bodyStream != nil {
		_, err = copyZeroAlloc(w, req.bodyStream)
		er := req.closeBodyStream() //nolint:errcheck
		if err == nil {
			err = er
		}
		return
	}
	if req.onlyMultipartForm() {
		return WriteMultipartForm(w, req.multipartForm, req.multipartFormBoundary)
	}
	body := req.bodyBytes()
	if len(body) != 0 {
		_, err = w.Write(body)
		return
	}
	_, err = w.Write(req.postArgs.QueryString())
	return
}

// BodyWriteTo writes response body to w.
func (resp *Response) BodyWriteTo(w io.Writer) (err error) {
	if resp.bodyStream != nil {
		_, err = copyZeroAlloc(w, resp.bodyStream)
		er := resp.closeBodyStream(err) //nolint:errcheck
		if err == nil {
			err = er
		}
		return
	}
	_, err = w.Write(resp.bodyBytes())
	return
}

// AppendBody appends p to response body.
//
// It is safe re-using p after the function returns.
func (resp *Response) AppendBody(p []byte) {
	if resp.bodyStream != nil {
		//goland:noinspection GoUnhandledErrorResult
		resp.closeBodyStream(nil) //nolint:errcheck
	}
	resp.chunkedW = nil
	resp.bodyRaw = nil
	//goland:noinspection GoUnhandledErrorResult
	resp.bodyBuffer().Write(p) //nolint:errcheck
}

// AppendBodyString appends s to response body.
func (resp *Response) AppendBodyString(s string) {
	resp.AppendBody(s2b(s))
}

// SetBody sets response body.
//
// It is safe re-using body argument after the function returns.
func (resp *Response) SetBody(body []byte) {
	if resp.bodyStream != nil {
		//goland:noinspection GoUnhandledErrorResult
		resp.closeBodyStream(nil) //nolint:errcheck
	}
	resp.chunkedW = nil
	resp.bodyRaw = nil
	bodyBuf := resp.bodyBuffer()
	bodyBuf.Reset()
	//goland:noinspection GoUnhandledErrorResult
	bodyBuf.Write(body) //nolint:errcheck
}

// SetBodyString sets response body.
func (resp *Response) SetBodyString(body string) {
	resp.SetBody(s2b(body))
}

// ResetBody resets response body.
func (resp *Response) ResetBody() {
	if resp.chunkedW != nil {
		resp.chunkedW = nil
	}
	resp.bodyRaw = nil
	if resp.bodyStream != nil {
		//goland:noinspection GoUnhandledErrorResult
		resp.closeBodyStream(nil) //nolint:errcheck
	}
	if resp.body != nil {
		if resp.keepBodyBuffer {
			resp.body.Reset()
		} else {
			responseBodyPool.Put(resp.body)
			resp.body = nil
		}
	}
}

// ResetBody resets request body.
func (req *Request) ResetBody() {
	req.bodyRaw = nil
	req.postArgs.Reset()
	req.RemoveMultipartFormFiles()
	//goland:noinspection GoUnhandledErrorResult
	req.closeBodyStream() //nolint:errcheck
	if req.body != nil {
		if req.keepBodyBuffer {
			req.body.Reset()
		} else {
			requestBodyPool.Put(req.body)
			req.body = nil
		}
	}
}

// SetBodyRaw sets response body, but without copying it.
//
// From this point onward the body argument must not be changed.
func (resp *Response) SetBodyRaw(body []byte) {
	resp.ResetBody()
	resp.bodyRaw = body
}

// SetBodyRaw sets response body, but without copying it.
//
// From this point onward the body argument must not be changed.
func (req *Request) SetBodyRaw(body []byte) {
	req.ResetBody()
	req.bodyRaw = body
}

// ReleaseBody retires the response body if it is greater than "size" bytes.
//
// This permits GC to reclaim the large buffer.  If used, must be before
// ReleaseResponse.
//
// Use this method only if you really understand how it works.
// The majority of workloads don't need this method.
func (resp *Response) ReleaseBody(size int) {
	resp.bodyRaw = nil
	if resp.body == nil {
		return
	}
	if cap(resp.body.B) > size {
		//goland:noinspection GoUnhandledErrorResult
		resp.closeBodyStream(nil) //nolint:errcheck
		resp.body = nil
	}
}

// ReleaseBody retires the request body if it is greater than "size" bytes.
//
// This permits GC to reclaim the large buffer.  If used, must be before
// ReleaseRequest.
//
// Use this method only if you really understand how it works.
// The majority of workloads don't need this method.
func (req *Request) ReleaseBody(size int) {
	req.bodyRaw = nil
	if req.body == nil {
		return
	}
	if cap(req.body.B) > size {
		//goland:noinspection GoUnhandledErrorResult
		req.closeBodyStream() //nolint:errcheck
		req.body = nil
	}
}

// SwapBody swaps response body with the given body and returns
// the previous response body.
//
// It is forbidden to use the body passed to SwapBody after
// the function returns.
//
// The `bodyStream` will always be closed, and if an error occurs during the closing,
// the error will be returned as the value.
//
// The parameter `body` will exist as the underlying slice of the `body`.
// The return value is not a reference, and the parameter `body` cannot
// be used by the caller anymore.
func (resp *Response) SwapBody(body []byte) []byte {
	bb := resp.bodyBuffer()

	if resp.bodyStream != nil {
		bb.Reset()
		_, err := copyZeroAlloc(bb, resp.bodyStream)
		//goland:noinspection GoUnhandledErrorResult
		resp.closeBodyStream(err) //nolint:errcheck
		if err != nil {
			bb.Reset()
			bb.SetString(err.Error())
		}
	}

	resp.bodyRaw = nil

	oldBody := bb.B
	bb.B = body
	return oldBody
}

// SwapBody swaps request body with the given body and returns
// the previous request body.
//
// It is forbidden to use the body passed to SwapBody after
// the function returns.
//
// The `bodyStream` will always be closed, and if an error occurs during the closing,
// the error will be returned as the value.
//
// The parameter `body` will exist as the underlying slice of the `body`.
// The return value is not a reference, and the parameter `body` cannot
// be used by the caller anymore.
func (req *Request) SwapBody(body []byte) []byte {
	bb := req.bodyBuffer()

	if req.bodyStream != nil {
		bb.Reset()
		_, err := copyZeroAlloc(bb, req.bodyStream)
		//goland:noinspection GoUnhandledErrorResult
		req.closeBodyStream() //nolint:errcheck
		if err != nil {
			bb.Reset()
			bb.SetString(err.Error())
		}
	}

	req.bodyRaw = nil

	oldBody := bb.B
	bb.B = body
	return oldBody
}

// AppendBody appends p to request body.
//
// It is safe re-using p after the function returns.
func (req *Request) AppendBody(p []byte) {
	if req.multipartForm != nil {
		req.RemoveMultipartFormFiles()
	}
	if req.bodyStream != nil {
		//goland:noinspection GoUnhandledErrorResult
		req.closeBodyStream() //nolint:errcheck
	}
	req.bodyRaw = nil
	//goland:noinspection GoUnhandledErrorResult
	req.bodyBuffer().Write(p) //nolint:errcheck
}

// AppendBodyString appends s to request body.
func (req *Request) AppendBodyString(s string) {
	req.AppendBody(s2b(s))
}

// SetBody sets request body.
//
// It is safe re-using body argument after the function returns.
func (req *Request) SetBody(body []byte) {
	if req.multipartForm != nil {
		req.RemoveMultipartFormFiles()
	}
	if req.bodyStream != nil {
		//goland:noinspection GoUnhandledErrorResult
		req.closeBodyStream() //nolint:errcheck
	}
	req.bodyRaw = nil
	req.bodyBuffer().Set(body)
}

// SetBodyString sets request body.
func (req *Request) SetBodyString(body string) {
	req.SetBody(s2b(body))
}

// CopyTo copies req contents to dst except of body stream.
func (req *Request) CopyTo(dst *Request) {
	req.copyToSkipBody(dst)
	if req.bodyRaw != nil {
		dst.bodyRaw = append(dst.bodyRaw[:0], req.bodyRaw...)
	}
	if req.body != nil {
		dst.bodyBuffer().Set(req.body.B)
	} else {
		if dst.body != nil {
			requestBodyPool.Put(dst.body)
			dst.body = nil
		}
	}
}

func (req *Request) copyToSkipBody(dst *Request) {
	dst.Reset()
	req.Header.CopyTo(&dst.Header)
	req.uri.CopyTo(&dst.uri)
	req.postArgs.CopyTo(&dst.postArgs)
	dst.timeout = req.timeout
	dst.maxBodySize = req.maxBodySize
	if dst.cliInfo != nil {
		in := *req.cliInfo
		dst.cliInfo = &in
	}
	//
	dst.secureErrorLogMessage = req.secureErrorLogMessage
	dst.parsedURI = req.parsedURI
	dst.parsedPostArgs = req.parsedPostArgs
	dst.keepBodyBuffer = req.keepBodyBuffer
	dst.isTLS = req.isTLS
	dst.UseHostHeader = req.UseHostHeader
	dst.DisableRedirectPathNormalizing = req.DisableRedirectPathNormalizing
	dst.DiscardUnReadBodyStream = req.DiscardUnReadBodyStream
	dst.StreamBody = req.StreamBody
	// do not copy multipartForm - it will be automatically
	// re-created on the first call to MultipartForm.
}

// CopyTo copies resp contents to dst except of body stream and chunkedW.
func (resp *Response) CopyTo(dst *Response) {
	resp.copyToSkipBody(dst)
	if resp.bodyRaw != nil {
		dst.bodyRaw = append(dst.bodyRaw[:0], resp.bodyRaw...)
	}
	//
	if resp.body != nil {
		dst.bodyBuffer().Set(resp.body.B)
	} else {
		if dst.body != nil {
			responseBodyPool.Put(dst.body)
			dst.body = nil
		}
	}
}

func (resp *Response) copyToSkipBody(dst *Response) {
	dst.Reset()
	resp.Header.CopyTo(&dst.Header)
	dst.raddr = resp.raddr
	dst.laddr = resp.laddr
	dst.compressPool = resp.compressPool
	dst.ImmediateHeaderFlush = resp.ImmediateHeaderFlush
	dst.StreamBody = resp.StreamBody
	dst.SkipBody = resp.SkipBody
	dst.keepBodyBuffer = resp.keepBodyBuffer
	dst.secureErrorLogMessage = resp.secureErrorLogMessage
}

func swapRequestBody(a, b *Request) {
	a.body, b.body = b.body, a.body
	a.bodyRaw, b.bodyRaw = b.bodyRaw, a.bodyRaw
	a.bodyStream, b.bodyStream = b.bodyStream, a.bodyStream

	// This code assumes that if a requestStream was swapped the headers are also swapped or copied.
	if rs, ok := a.bodyStream.(*requestStream); ok {
		rs.header = &a.Header
	}
	if rs, ok := b.bodyStream.(*requestStream); ok {
		rs.header = &b.Header
	}
}

func swapResponseBody(a, b *Response) {
	a.body, b.body = b.body, a.body
	a.bodyRaw, b.bodyRaw = b.bodyRaw, a.bodyRaw
	a.bodyStream, b.bodyStream = b.bodyStream, a.bodyStream
}

// URI returns request URI.
func (req *Request) URI() *URI {
	//goland:noinspection GoUnhandledErrorResult
	req.parseURI() //nolint:errcheck
	return &req.uri
}

// URIWithErr URI returns request URI.
func (req *Request) URIWithErr() (u *URI, err error) {
	err = req.parseURI() //nolint:errcheck
	u = &req.uri
	return
}

// SetURI initializes request URI.
// Use this method if a single URI may be reused across multiple requests.
// Otherwise, you can just use SetRequestURI() and it will be parsed as new URI.
// The URI is copied and can be safely modified later.
func (req *Request) SetURI(newURI *URI) {
	if newURI != nil {
		newURI.CopyTo(&req.uri)
		req.parsedURI = true
		return
	}
	req.uri.Reset()
	req.parsedURI = false
}

func (req *Request) parseURI() error {
	if req.parsedURI {
		return nil
	}
	req.parsedURI = true

	return req.uri.parse(req.Header.Host(), req.Header.RequestURI(), req.isTLS)
}

// PostArgs returns POST arguments.
func (req *Request) PostArgs() *Args {
	req.parsePostArgs()
	return &req.postArgs
}

func (req *Request) parsePostArgs() {
	if req.parsedPostArgs {
		return
	}
	req.parsedPostArgs = true

	if !bytes.HasPrefix(req.Header.ContentType(), strPostArgsContentType) {
		return
	}
	req.postArgs.ParseBytes(req.Body())
}

// MultipartForm returns request's multipart form.
//
// Returns ErrNoMultipartForm if request's Content-Type
// isn't 'multipart/form-data'.
//
// RemoveMultipartFormFiles must be called after returned multipart form
// is processed.
func (req *Request) MultipartForm() (m *multipart.Form, err error) {
	if req.multipartForm != nil {
		m = req.multipartForm
		return
	}

	req.multipartFormBoundary = b2s(req.Header.MultipartFormBoundary())
	if req.multipartFormBoundary == "" {
		return
	}
	ce := req.Header.peek(strContentEncoding)
	if len(ce) != 0 {
		if !bytes.Equal(ce, strGzip) {
			err = errors.New(`unsupported Content-Encoding: "` + b2s(ce) + `"`)
			return
		}
	}
	if req.bodyStream != nil {
		bodyStream := req.bodyStream
		if len(ce) != 0 {
			bodyStream, err = gzip.NewReader(bodyStream)
			if err != nil {
				err = errors.New("cannot gunzip request body: " + err.Error())
				return
			}
		}
		mr := multipart.NewReader(bodyStream, req.multipartFormBoundary)
		req.multipartForm, err = mr.ReadForm(8 * 1024)
		if err != nil {
			err = errors.New(`cannot read multipart/form-data body: ` + err.Error())
			return
		}
	} else {
		body := req.bodyBytes()
		if len(ce) > 0 {
			if body, err = AppendGunzipBytes(nil, body); err != nil {
				err = errors.New("cannot gunzip request body: " + err.Error())
				return
			}
		}
		req.multipartForm, err = readMultipartForm(bytes.NewReader(body), req.multipartFormBoundary, int64(len(body)), int64(len(body)))
		if err != nil {
			err = errors.New(`cannot read multipart/form-data body: ` + err.Error())
			return
		}
	}
	m = req.multipartForm
	return
}

func marshalMultipartForm(f *multipart.Form, boundary string) ([]byte, error) {
	var buf bytebufferpool.ByteBuffer
	if err := WriteMultipartForm(&buf, f, boundary); err != nil {
		return nil, err
	}
	return buf.B, nil
}

// WriteMultipartForm writes the given multipart form f with the given
// boundary to w.
func WriteMultipartForm(w io.Writer, f *multipart.Form, boundary string) error {
	// Do not care about memory allocations here, since multipart
	// form processing is slow.
	if boundary == "" {
		return errors.New("form boundary cannot be empty")
	}

	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return fmt.Errorf("cannot use form boundary %q: %w", boundary, err)
	}

	// marshal values
	for k, vv := range f.Value {
		for _, v := range vv {
			if err := mw.WriteField(k, v); err != nil {
				return fmt.Errorf("cannot write form field %q value %q: %w", k, v, err)
			}
		}
	}

	// marshal files
	for k, fvv := range f.File {
		for _, fv := range fvv {
			vw, err := mw.CreatePart(fv.Header)
			if err != nil {
				return fmt.Errorf("cannot create form file %q (%q): %w", k, fv.Filename, err)
			}
			fh, err := fv.Open()
			if err != nil {
				return fmt.Errorf("cannot open form file %q (%q): %w", k, fv.Filename, err)
			}
			if _, err = copyZeroAlloc(vw, fh); err != nil {
				_ = fh.Close()
				return fmt.Errorf("error when copying form file %q (%q): %w", k, fv.Filename, err)
			}
			if err = fh.Close(); err != nil {
				return fmt.Errorf("cannot close form file %q (%q): %w", k, fv.Filename, err)
			}
		}
	}

	if err := mw.Close(); err != nil {
		return fmt.Errorf("error when closing multipart form writer: %w", err)
	}

	return nil
}

func readMultipartForm(r io.Reader, boundary string, size, maxInMemoryFileSize int64) (m *multipart.Form, err error) {
	// Do not care about memory allocations here, since they are tiny
	// compared to multipart data (aka multi-MB files) usually sent
	// in multipart/form-data requests.
	if size <= 0 {
		err = errors.New("form size must be greater than 0")
		return
	}
	lr := io.LimitReader(r, size)
	mr := multipart.NewReader(lr, boundary)
	m, err = mr.ReadForm(maxInMemoryFileSize)
	if err != nil {
		err = errors.New("cannot read multipart/form-data body: " + err.Error())
		return
	}
	return
}

func (req *Request) KeepBuffer() bool {
	return req.keepBodyBuffer
}

// SetKeepBuffer Whether to return the storage slices to the cache pool when the Request object is reset
func (req *Request) SetKeepBuffer(keep bool) {
	req.keepBodyBuffer = keep
}

func (resp *Response) KeepBuffer() bool {
	return resp.keepBodyBuffer
}

// SetKeepBuffer Whether to return the storage slices to the cache pool when the Response object is reset
func (resp *Response) SetKeepBuffer(keep bool) {
	resp.keepBodyBuffer = keep
}

// Reset clears request contents.
// Expect keepBodyBuffer unchanged.
func (req *Request) Reset() {
	if requestBodyPoolSizeLimit >= 0 && req.body != nil {
		req.ReleaseBody(requestBodyPoolSizeLimit)
	}
	// Discard requestBodyStream need content-length information.
	// So second reset head.
	req.resetSkipHeader()
	req.Header.Reset()
}
func (req *Request) needFinish() (need bool) {
	if req.bodyStream == nil {
		return
	}
	_, need = req.bodyStream.(*requestStream)
	return
}
func (req *Request) finishRequest(readTimeout time.Duration, c net.Conn) (err error) {
	if req.bodyStream == nil {
		return
	}
	_, ok := req.bodyStream.(*requestStream)
	if !ok {
		return
	}
	//
	// ----
	// If we are to discard RequestBodyStream and the handler has neither finished reading nor
	// closed it, then the following bw.Flush() will not be executed because of br.Buffered.
	if readTimeout > 0 {
		err = c.SetReadDeadline(time.Now().Add(readTimeout))
		if err != nil {
			return
		}
	}
	err = req.closeBodyStream()
	if err != nil {
		return
	}
	if readTimeout > 0 {
		err = c.SetReadDeadline(zeroTime)
		if err != nil {
			return
		}
	}
	return
}

func (req *Request) resetForKeepAlive() {
	if requestBodyPoolSizeLimit >= 0 && req.body != nil {
		req.ReleaseBody(requestBodyPoolSizeLimit)
	}
	req.ResetBody()
	req.uri.Reset()
	req.maxBodySize = 0
	req.parsedURI = false
	req.parsedPostArgs = false
	req.Header.resetSkipNormalize()
}

func (req *Request) resetSkipHeader() {
	req.ResetBody()
	req.uri.Reset()
	req.timeout = 0
	req.maxBodySize = 0
	req.cliInfo = nil
	req.secureErrorLogMessage = false
	req.parsedURI = false
	req.parsedPostArgs = false
	req.isTLS = false
	req.UseHostHeader = false
	req.DiscardUnReadBodyStream = false
	req.DisableRedirectPathNormalizing = false
	req.StreamBody = false
}

// RemoveMultipartFormFiles removes multipart/form-data temporary files
// associated with the request.
func (req *Request) RemoveMultipartFormFiles() {
	if req.multipartForm != nil {
		// Do not check for error, since these files may be deleted or moved
		// to new places by user code.
		//goland:noinspection GoUnhandledErrorResult
		req.multipartForm.RemoveAll() //nolint:errcheck
		req.multipartForm = nil
	}
	req.multipartFormBoundary = ""
}

// Reset clears response contents.
// Except keepBuffer field unchanged.
func (resp *Response) Reset() {
	if responseBodyPoolSizeLimit >= 0 && resp.body != nil {
		resp.ReleaseBody(responseBodyPoolSizeLimit)
	}
	resp.resetSkipHeader()
	resp.Header.Reset()
}
func (resp *Response) resetForKeepAlive() {
	if responseBodyPoolSizeLimit >= 0 && resp.body != nil {
		resp.ReleaseBody(responseBodyPoolSizeLimit)
	}
	resp.ResetBody()
	resp.raddr = nil
	resp.laddr = nil
	resp.compressPool = nil
	resp.ImmediateHeaderFlush = false
	resp.SkipBody = false
	resp.Header.headMethod = false
	resp.Header.resetSkipNormalize()
}

func (resp *Response) resetSkipHeader() {
	resp.ResetBody()
	resp.raddr = nil
	resp.laddr = nil
	resp.compressPool = nil
	resp.ImmediateHeaderFlush = false
	resp.StreamBody = false
	resp.SkipBody = false
	resp.secureErrorLogMessage = false
}

// Read reads request (including body) from the given r.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
//
// If MayContinue returns true, the caller must:
//
//   - Either send StatusExpectationFailed response if request headers don't
//     satisfy the caller.
//   - Or send StatusContinue response before reading request body
//     with ReadBody.
//   - Or close the connection.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (req *Request) Read(r *bufio.Reader) error {
	return req.ReadLimit(r, 0)
}

const defaultMaxInMemoryFileSize = 16 * 1024 * 1024

// ErrGetOnly is returned when server expects only GET requests,
// but some other type of request came (Server.GetOnly option is true).
var ErrGetOnly = errors.New("non-GET request received")

// ReadLimit reads request from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
//
// If MayContinue returns true, the caller must:
//
//   - Either send StatusExpectationFailed response if request headers don't
//     satisfy the caller.
//   - Or send StatusContinue response before reading request body
//     with ReadBody.
//   - Or close the connection.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (req *Request) ReadLimit(r *bufio.Reader, maxBodySize int64) (err error) {
	// req.uri.Reset() // ReadLimit method doesn't change this field
	// req.isTLS = false // ReadLimit method doesn't change this field
	// Reset fields that might be altered or affected during the RequestHeader.Read method call.
	req.parsedURI = false
	req.parsedPostArgs = false
	if err = req.Header.Read(r); err != nil {
		return
	}
	req.ResetBody()

	if req.MayContinue() {
		// 'Expect: 100-continue' header found. Let the caller deciding
		// whether to read request body or
		// to return StatusExpectationFailed.
		return
	}
	if req.StreamBody {
		return req.ReadBodyStream(r, maxBodySize, true)
	}
	return req.ReadBody(r, maxBodySize, true)
}

// MayContinue returns true if the request contains
// 'Expect: 100-continue' header.
//
// The caller must do one of the following actions if MayContinue returns true:
//
//   - Either send StatusExpectationFailed response if request headers don't
//     satisfy the caller.
//   - Or send StatusContinue response before reading request body
//     with ReadBody.
//   - Or close the connection.
func (req *Request) MayContinue() bool {
	return bytes.Equal(req.Header.peek(strExpect), str100Continue)
}

// ReadBody reads request's header and  body.
// If request header contains 'Expect: 100-continue'.
// The caller must send StatusContinue response before calling this method.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (req *Request) ReadBody(r *bufio.Reader, maxBodySize int64, preParseMultipartForm ...bool) (err error) {
	contentLength := req.Header.realContentLength()
	// Attempting to read the request body is meaningless if the Content-Length of the request is 0.
	if contentLength == 0 {
		return
	}
	if contentLength > 0 {
		if maxBodySize > 0 && contentLength > maxBodySize {
			return ErrBodyTooLarge
		}
		if len(preParseMultipartForm) == 0 || preParseMultipartForm[0] {
			// Pre-read multipart form data of known length.
			// This way we limit memory usage for large file uploads, since their contents
			// is streamed into temporary files if file size exceeds defaultMaxInMemoryFileSize.
			req.multipartFormBoundary = b2s(req.Header.MultipartFormBoundary())
			if req.multipartFormBoundary != "" && len(req.Header.peek(strContentEncoding)) == 0 {
				req.multipartForm, err = readMultipartForm(r, req.multipartFormBoundary, contentLength, defaultMaxInMemoryFileSize)
				return
			}
		}
	}

	// At this point, contentLength is guaranteed to be greater than 0 or equal to -1.
	if err = req.readBody(r, contentLength, maxBodySize); err != nil {
		return
	}
	// When contentLength == -1, we must attempt to read the Trailer header,
	// even if there isn't one, because the ReadBody method hasn't fully read the entire chunked content.
	// The final \r\n still remains unread.
	if contentLength == -1 {
		err = req.Header.ReadTrailer(r)
	}
	return
}

// ReadBody reads request body from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
//
// The ReadBody method only reads the request body; it doesn't parse the body based on the Content-Type.
func (req *Request) readBody(r *bufio.Reader, contentLength, maxBodySize int64) (err error) {
	if contentLength == 0 {
		return
	}
	bodyBuf := req.bodyBuffer()
	bodyBuf.Reset()
	switch {
	case contentLength > 0:
		bodyBuf.B, err = readBody(r, contentLength, maxBodySize, bodyBuf.B)
	case contentLength == -1:
		bodyBuf.B, err = readBodyChunked(r, maxBodySize, bodyBuf.B)
		// TODO need change Content-Length
		if err == nil && len(bodyBuf.B) == 0 {
			req.Header.SetContentLength(0)
		}
	default:
		bodyBuf.B, err = readBodyIdentity(r, maxBodySize, bodyBuf.B)
		req.Header.SetContentLength(int64(len(bodyBuf.B)))
	}
	if err == io.EOF {
		err = ErrUnexpectedReqBodyEOF
	}
	return
}

// ReadBodyStream reads request body if request header contains
// 'Expect: 100-continue'.
//
// The caller must send StatusContinue response before calling this method.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (req *Request) ReadBodyStream(r *bufio.Reader, maxBodySize int64, preParseMultipartForm ...bool) (err error) {
	contentLength := req.Header.realContentLength()
	// RFC 7230 neither explicitly permits nor forbids an
	// entity-body on a GET request so we permit one if
	// declared, but we default to 0 here (not -1 below)
	// if there's no mention of a body.
	// Likewise, all other request methods are assumed to have
	// no body if neither Transfer-Encoding chunked nor a
	// Content-Length are set.
	// See https://github.com/golang/go/blob/9e8ea567c838574a0f14538c0bbbd83c3215aa55/src/net/http/transfer.go#L729
	if contentLength == 0 {
		req.bodyStream = emptyRequestBodyStream
		return
	}

	if contentLength > 0 {
		if len(preParseMultipartForm) == 0 || preParseMultipartForm[0] {
			// Pre-read multipart form data of known length.
			// This way we limit memory usage for large file uploads, since their contents
			// is streamed into temporary files if file size exceeds defaultMaxInMemoryFileSize.
			req.multipartFormBoundary = b2s(req.Header.MultipartFormBoundary())
			if req.multipartFormBoundary != "" && len(req.Header.peek(strContentEncoding)) == 0 {
				req.multipartForm, err = readMultipartForm(r, req.multipartFormBoundary, contentLength, defaultMaxInMemoryFileSize)
				if err == io.EOF {
					err = ErrUnexpectedReqBodyEOF
				}
				return
			}
		}
	}

	bodyBuf := req.bodyBuffer()
	bodyBuf.Reset()
	b, allInBuf, err := readBodyWithStreaming(r, contentLength, maxBodySize, bodyBuf.B)
	if err != nil {
		if err == ErrBodyTooLarge || err == errChunkedStream {
			bodyBuf.B = b
			req.bodyStream = acquireRequestStream(bodyBuf, r, &req.Header)
			bodyBuf.B = bodyBuf.B[:0]
			// reset error
			err = nil
			return
		}
		if err == io.EOF {
			err = ErrUnexpectedReqBodyEOF
		}
		return
	}
	//req.body = bodyBuf
	if allInBuf {
		req.bodyStream = bytes.NewReader(b)
	} else {
		bodyBuf.B = b[:0]
		b2, _ := r.Peek(r.Size())
		//goland:noinspection GoUnhandledErrorResult
		r.Discard(r.Size())
		req.bodyStream = &TwoBytesReader{bytes: [2][]byte{b, b2}}
	}
	return
}

func (resp *Response) ReadBodyStream(r *bufio.Reader, maxBodySize int64) (err error) {
	contentLength := resp.Header.ContentLength()
	// RFC 7230 neither explicitly permits nor forbids an
	// entity-body on a GET request so we permit one if
	// declared, but we default to 0 here (not -1 below)
	// if there's no mention of a body.
	// Likewise, all other request methods are assumed to have
	// no body if neither Transfer-Encoding chunked nor a
	// Content-Length are set.
	// See https://github.com/golang/go/blob/9e8ea567c838574a0f14538c0bbbd83c3215aa55/src/net/http/transfer.go#L729
	if contentLength == 0 {
		resp.bodyStream = emptyRequestBodyStream
		return
	}
	if contentLength == -2 {
		resp.bodyStream = r
		return
	}
	bodyBuf := resp.bodyBuffer()
	bodyBuf.Reset()
	b, allInBuf, err := readBodyWithStreaming(r, contentLength, maxBodySize, bodyBuf.B)
	if err != nil {
		if err == ErrBodyTooLarge || err == errChunkedStream {
			bodyBuf.B = b
			resp.bodyStream = acquireRequestStream(bodyBuf, r, &resp.Header)
			bodyBuf.B = bodyBuf.B[:0]
			// reset err.
			err = nil
			return
		}
		if err == io.EOF {
			err = ErrUnexpectedRespBodyEOF
		}
		return
	}
	if allInBuf {
		resp.bodyStream = bytes.NewReader(b)
	} else {
		bodyBuf.B = b[:0]
		b2, _ := r.Peek(r.Size())
		//goland:noinspection GoUnhandledErrorResult
		r.Discard(r.Size())
		resp.bodyStream = &TwoBytesReader{bytes: [2][]byte{b, b2}}
	}
	return
}

type TwoBytesReader struct {
	bytes   [2][]byte
	i       int64
	current int
}

func (t *TwoBytesReader) Read(p []byte) (n int, err error) {
second:
	if t.current == 1 {
		if t.i >= int64(len(t.bytes[1])) {
			return 0, io.EOF
		}
		n = copy(p, t.bytes[1][t.i:])
		t.i += int64(n)
		return
	}
	n = copy(p, t.bytes[0][t.i:])
	t.i += int64(n)
	if t.i == int64(len(t.bytes[0])) {
		t.current = 1
		t.i = 0
		if n == 0 {
			goto second
		}
	}
	return
}

// Read reads response (including body) from the given r.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (resp *Response) Read(r *bufio.Reader) error {
	return resp.ReadLimit(r, 0)
}

// ReadLimit reads response headers from the given r,
// then reads the body using the ReadBody function and limiting the body size.
//
// If resp.SkipBody is true then it skips reading the response body.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (resp *Response) ReadLimit(r *bufio.Reader, maxBodySize int64) (err error) {
	err = resp.Header.Read(r)
	if err != nil {
		return err
	}
	resp.ResetBody()
	if resp.Header.StatusCode() == StatusContinue {
		// Read the next response according to http://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html .
		err = resp.Header.Read(r)
		if err != nil {
			return err
		}
	}
	if resp.mustSkipBody() || resp.Header.headMethod {
		return
	}
	if resp.StreamBody {
		return resp.ReadBodyStream(r, maxBodySize)
	}
	return resp.ReadBody(r, maxBodySize)
}

// ReadLimitIfClose If the response does not have a Content-Length header and the response body
// should not be skipped, the connection should be closed after the response has been fully read.
//
// This is because such a response requires waiting for the peer to close the connection in order
// to complete reading the response. However, the `io.EOF` triggered by the peer closing the
// connection cannot be observed in this context.
func (resp *Response) ReadLimitIfClose(r *bufio.Reader, maxBodySize int64) (closeConn bool, err error) {
	err = resp.Header.Read(r)
	if err != nil {
		return
	}
	resp.ResetBody()
	if resp.Header.StatusCode() == StatusContinue {
		// Read the next response according to http://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html .
		err = resp.Header.Read(r)
		if err != nil {
			return
		}
	}
	if resp.mustSkipBody() || resp.Header.headMethod {
		return
	}
	if resp.Header.contentLength == -2 {
		closeConn = true
	}
	if resp.StreamBody {
		err = resp.ReadBodyStream(r, maxBodySize)
		return
	}
	err = resp.ReadBody(r, maxBodySize)
	return
}

// ReadBody reads response body from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (resp *Response) ReadBody(r *bufio.Reader, maxBodySize int64) (err error) {
	contentLength := resp.Header.ContentLength()
	if contentLength == 0 {
		return
	}
	bodyBuf := resp.bodyBuffer()
	bodyBuf.Reset()

	switch {
	case contentLength >= 0:
		bodyBuf.B, err = readBody(r, contentLength, maxBodySize, bodyBuf.B)
	case contentLength == -1:
		bodyBuf.B, err = readBodyChunked(r, maxBodySize, bodyBuf.B)
	default:
		bodyBuf.B, err = readBodyIdentity(r, maxBodySize, bodyBuf.B)
		resp.Header.SetContentLength(int64(len(bodyBuf.B)))
	}
	if err != nil {
		if err == io.EOF {
			err = ErrUnexpectedRespBodyEOF
		}
		return
	}
	//  If body is skip, trailer can successfully read ??
	//  no client hang. Test show ReadTrailer method only can handle
	//  Response body with chunked encoding had been read situation.
	//  If body is not read, ReadTrailer hang over otherwise set readTimeout.
	//  So never set Skip body manual.
	if resp.Header.ContentLength() == -1 {
		err = resp.Header.ReadTrailer(r)
	}
	return
}

func (resp *Response) mustSkipBody() bool {
	return resp.SkipBody || resp.Header.mustSkipContentLength()
}

var errRequestHostRequired = errors.New("missing required Host header in request")

// WriteTo writes request to w. It implements io.WriterTo.
func (req *Request) WriteTo(w io.Writer) (int64, error) {
	return writeBufio(req, w)
}

// WriteTo writes response to w. It implements io.WriterTo.
func (resp *Response) WriteTo(w io.Writer) (int64, error) {
	return writeBufio(resp, w)
}

func writeBufio(hw httpWriter, w io.Writer) (int64, error) {
	sw := acquireStatsWriter(w)
	bw := acquireBufioWriter(sw)
	errw := hw.Write(bw)
	errf := bw.Flush()
	releaseBufioWriter(bw)
	n := sw.bytesWritten
	releaseStatsWriter(sw)

	err := errw
	if err == nil {
		err = errf
	}
	return n, err
}

type statsWriter struct {
	w            io.Writer
	bytesWritten int64
}

func (w *statsWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

func acquireStatsWriter(w io.Writer) *statsWriter {
	v := statsWriterPool.Get()
	if v == nil {
		return &statsWriter{
			w: w,
		}
	}
	sw := v.(*statsWriter)
	sw.w = w
	return sw
}

func releaseStatsWriter(sw *statsWriter) {
	sw.w = nil
	sw.bytesWritten = 0
	statsWriterPool.Put(sw)
}

var statsWriterPool sync.Pool

func acquireBufioWriter(w io.Writer) *bufio.Writer {
	v := bufioWriterPool.Get()
	if v == nil {
		return bufio.NewWriter(w)
	}
	bw := v.(*bufio.Writer)
	bw.Reset(w)
	return bw
}

func releaseBufioWriter(bw *bufio.Writer) {
	bufioWriterPool.Put(bw)
}

var bufioWriterPool sync.Pool

func (req *Request) onlyMultipartForm() bool {
	return req.multipartForm != nil && (req.body == nil || len(req.body.B) == 0)
}

// Write writes request to w.
//
// Write doesn't flush request to w for performance reasons.
//
// Send MultipartForm only when Request.Body is empty.
// Priority of Request Body(Mutually Exclusive): bodyStream > Request.Body > MultipartForm > PostArgs
//
// See also WriteTo.
func (req *Request) Write(w *bufio.Writer) (err error) {
	if len(req.Header.Host()) == 0 || req.parsedURI {
		uri := req.URI()
		host := uri.Host()
		headHost := req.Header.Host()
		if len(host) == 0 && len(headHost) == 0 {
			return errRequestHostRequired
		}
		if !req.UseHostHeader || len(headHost) == 0 {
			req.Header.SetHostBytes(host)
		}
		req.Header.SetRequestURIBytes(uri.RequestURI())

		if len(uri.username) > 0 {
			// RequestHeader.SetBytesKV only uses RequestHeader.bufKV.key
			// So we are free to use RequestHeader.bufKV.value as a scratch pad for
			// the base64 encoding.
			nl := len(uri.username) + len(uri.password) + 1
			nb := nl + len(strBasicSpace)
			tl := nb + base64.StdEncoding.EncodedLen(nl)
			if tl > cap(req.Header.bufV) {
				req.Header.bufV = make([]byte, 0, tl)
			}
			buf := req.Header.bufV[:0]
			buf = append(buf, uri.username...)
			buf = append(buf, strColon...)
			buf = append(buf, uri.password...)
			buf = append(buf, strBasicSpace...)
			base64.StdEncoding.Encode(buf[nb:tl], buf[:nl])
			req.Header.SetBytesKV(strAuthorization, buf[nl:tl])
		}
	}

	if req.bodyStream != nil {
		return req.writeBodyStream(w)
	}

	body := req.bodyBytes()
	if req.onlyMultipartForm() {
		body, err = marshalMultipartForm(req.multipartForm, req.multipartFormBoundary)
		if err != nil {
			err = errors.New("err when marshaling multipart form: " + err.Error())
			return
		}
		req.Header.SetMultipartFormBoundary(req.multipartFormBoundary)
	}

	hasBody := false
	if len(body) == 0 {
		body = req.postArgs.QueryString()
		if len(body) > 0 {
			req.Header.SetContentTypeBytes(strPostArgsContentType)
		}
	}

	if len(body) != 0 || !req.Header.ignoreBody() {
		hasBody = true
		req.Header.SetContentLength(int64(len(body)))
	}
	if err = req.Header.Write(w); err != nil {
		return
	}
	if hasBody && len(body) > 0 {
		_, err = w.Write(body)
	}
	return
}

// WriteGzip writes response with gzipped body to w.
//
// The method WriteGzip response body and sets 'Content-Encoding: gzip'
// header before writing response to w.
//
// WriteGzip doesn't flush response to w for performance reasons.
func (resp *Response) WriteGzip(w *bufio.Writer) error {
	return resp.WriteGzipLevel(w, CompressDefaultCompression)
}

// WriteGzipLevel writes response with gzipped body to w.
//
// Level is the desired compression level:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
//
// The method gZips response body and sets 'Content-Encoding: gzip'
// header before writing response to w.
//
// WriteGzipLevel doesn't flush response to w for performance reasons.
func (resp *Response) WriteGzipLevel(w *bufio.Writer, level int) error {
	resp.gzipBody(level)
	return resp.Write(w)
}

// WriteDeflate writes response with deflated body to w.
//
// The method deflates response body and sets 'Content-Encoding: deflate'
// header before writing response to w.
//
// WriteDeflate doesn't flush response to w for performance reasons.
func (resp *Response) WriteDeflate(w *bufio.Writer) error {
	return resp.WriteDeflateLevel(w, CompressDefaultCompression)
}

// WriteDeflateLevel writes response with deflated body to w.
//
// Level is the desired compression level:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
//
// The method deflates response body and sets 'Content-Encoding: deflate'
// header before writing response to w.
//
// WriteDeflateLevel doesn't flush response to w for performance reasons.
func (resp *Response) WriteDeflateLevel(w *bufio.Writer, level int) error {
	resp.deflateBody(level)
	return resp.Write(w)
}

func (resp *Response) brotliBody(level int) {
	if len(resp.Header.ContentEncoding()) > 0 {
		// It looks like the body is already compressed.
		// Do not compress it again.
		return
	}
	if !resp.Header.isCompressibleContentType() {
		return
	}
	if resp.bodyStream == nil && len(resp.bodyBytes()) < minCompressLen {
		return
	}
	resp.compressPool = ucompress.DefaultCBrotliCompressPools.Pool(level)

	resp.Header.SetContentEncodingBytes(strBr)
	resp.Header.addVaryBytes(strAcceptEncoding)
}

func (resp *Response) gzipBody(level int) {
	if len(resp.Header.ContentEncoding()) > 0 {
		// It looks like the body is already compressed.
		// Do not compress it again.
		return
	}

	if !resp.Header.isCompressibleContentType() {
		// The content-type cannot be compressed.
		return
	}
	if resp.bodyStream == nil && len(resp.bodyBytes()) < minCompressLen {
		return
	}
	resp.compressPool = ucompress.DefaultGzipCompressPools.Pool(level)
	resp.Header.SetContentEncodingBytes(strGzip)
	resp.Header.addVaryBytes(strAcceptEncoding)
}

func (resp *Response) deflateBody(level int) {
	if len(resp.Header.ContentEncoding()) > 0 {
		// It looks like the body is already compressed.
		// Do not compress it again.
		return
	}

	if !resp.Header.isCompressibleContentType() {
		// The content-type cannot be compressed.
		return
	}
	if resp.bodyStream == nil && len(resp.bodyBytes()) < minCompressLen {
		return
	}
	resp.compressPool = ucompress.DefaultDeflateCompressPools.Pool(level)
	resp.Header.SetContentEncodingBytes(strDeflate)
	resp.Header.addVaryBytes(strAcceptEncoding)
}

func (resp *Response) zstdBody(level int) {
	if len(resp.Header.ContentEncoding()) > 0 {
		return
	}

	if !resp.Header.isCompressibleContentType() {
		return
	}

	if resp.bodyStream == nil && len(resp.bodyBytes()) < minCompressLen {
		return
	}
	resp.compressPool = ucompress.DefaultZstdCompressPools.Pool(level)
	resp.Header.SetContentEncodingBytes(strZstd)
	resp.Header.addVaryBytes(strAcceptEncoding)
}

// Bodies with sizes smaller than minCompressLen aren't compressed at all.
const minCompressLen = 200

type writeFlusher interface {
	io.Writer
	Flush() error
}

type flushWriter struct {
	wf writeFlusher
	bw *bufio.Writer
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.wf.Write(p)
	if err != nil {
		return 0, err
	}
	if err = w.wf.Flush(); err != nil {
		return 0, err
	}
	if err = w.bw.Flush(); err != nil {
		return 0, err
	}
	return n, nil
}

// Write writes response to w.
//
// Write doesn't flush response to w for performance reasons.
//
// See also WriteTo.
// w.Flush method don't be called by this method.
func (resp *Response) Write(w *bufio.Writer) (err error) {
	if resp.chunkedW != nil {
		// chunked Transfer-Encoding, may be compressed.
		return resp.WriteChunkedStream(w)
	}
	//
	if resp.needCompress() {
		// compress response, maybe chunked Transfer-Encoding.
		return resp.WriteCompress(w)
	}
	//
	sendBody := !resp.mustSkipBody()

	if resp.bodyStream != nil {
		// no compress response, maybe chunked Transfer-Encoding.
		return resp.writeBodyStream(w, sendBody)
	}
	// no compress response, no chunked Transfer-Encoding.
	body := resp.bodyBytes()
	bodyLen := len(body)
	// for head response, if bodyLen>0, set it.
	if sendBody || bodyLen > 0 {
		if resp.Header.contentLength != -2 {
			// close connection as eof.
			resp.Header.SetContentLength(int64(bodyLen))
		}
	}
	//
	err = resp.WriteHeader(w)
	if err != nil {
		return
	}
	// without response body.
	if !sendBody {
		return
	}
	_, err = w.Write(body)
	return
}

func (req *Request) writeBodyStream(w *bufio.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &WriteBodyStreamPanic{
				error: fmt.Errorf("panic while writing request body stream: %+v", r),
			}
		}
	}()
	contentLength := req.Header.ContentLength()
	if contentLength < 0 {
		// bodyStream may contain Content-Length information.
		lrSize := limitedReaderSize(req.bodyStream)
		if lrSize >= 0 {
			contentLength = lrSize
			req.Header.SetContentLength(contentLength)
		}
	}
	//
	if contentLength >= 0 {
		if err = req.Header.Write(w); err == nil {
			if contentLength > 0 {
				// read all content from bodyStream in , then flush net.Conn at once.
				err = writeBodyFixedSize(w, req.bodyStream, contentLength)
			}
		}
	} else {
		// whatever set Content-Length=-1
		// for request must contain Content-Length or Transfer-Encoding.
		// or Server think Content-Length is zero obey net/http.
		req.Header.SetContentLength(-1)
		err = req.Header.Write(w)
		if err == nil {
			err = writeBodyChunked(w, req.bodyStream)
		}
		if err == nil {
			err = req.Header.writeTrailer(w)
		}
	}
	// Whatever close bloodStream.
	errc := req.closeBodyStream()
	if err == nil {
		err = errc
	}
	return
}

// WriteBodyStreamPanic is returned when panic happens during writing body stream.
type WriteBodyStreamPanic struct {
	error
}

func (resp *Response) writeBodyStream(w *bufio.Writer, sendBody bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &WriteBodyStreamPanic{
				error: fmt.Errorf("panic while writing response body stream: %+v", r),
			}
		}
	}()

	contentLength := resp.Header.ContentLength()
	if contentLength < 0 {
		// bodyStream may contain length information.
		lrSize := limitedReaderSize(resp.bodyStream)
		if lrSize >= 0 {
			if contentLength == -2 {
				// save Connection: close header
				// when contentLength!=-2, write header doesn't set Connection: close for us.
				resp.Header.SetConnectionClose()
			}
			contentLength = lrSize
			// update Content-Length
			resp.Header.SetContentLength(contentLength)
		}
	}
	err = resp.WriteHeader(w)
	// below code never update Response's header. so above can safely write header.
	if err == nil && sendBody && contentLength != 0 {
		if contentLength > 0 {
			err = writeBodyFixedSize(w, resp.bodyStream, contentLength)
		} else if contentLength == -1 {
			err = writeBodyChunked(w, resp.bodyStream)
			if err == nil {
				err = resp.Header.writeTrailer(w)
			}
		} else {
			// close content as eof signal.
			_, err = copyZeroAlloc(w, resp.bodyStream)
		}
	}
	// Regardless, we need to call the `closeBodyStream` method.
	errc := resp.closeBodyStream(err)
	if err == nil {
		err = errc
	}
	return err
}

func (req *Request) closeBodyStream() (err error) {
	if req.bodyStream == nil {
		return
	}
	if bsc, ok := req.bodyStream.(io.Closer); ok {
		err = bsc.Close()
	}
	//
	if rs, ok := req.bodyStream.(*requestStream); ok {
		if req.DiscardUnReadBodyStream {
			// pio.Discard support io.ReaderFrom interface
			// giving buf parameter is useless.
			_, err = io.CopyBuffer(pio.Discard, req.bodyStream, nil)
		}
		releaseRequestStream(rs)
	}
	//
	req.bodyStream = nil
	return
}

func (resp *Response) closeBodyStream(wErr error) (err error) {
	if resp.bodyStream == nil {
		return
	}
	if bsc, ok := resp.bodyStream.(io.Closer); ok {
		err = bsc.Close()
	} else if bsc2, ok2 := resp.bodyStream.(ReadCloserWithError); ok2 {
		err = bsc2.CloseWithError(wErr)
	}
	if bsr, ok := resp.bodyStream.(*requestStream); ok {
		releaseRequestStream(bsr)
	}
	resp.bodyStream = nil
	return
}

// String returns request representation include header and boy.
//
// Returns error message instead of request representation on error.
//
// Use Write instead of String for performance-critical code.
func (req *Request) String() string {
	return httpString(req)
}

// String returns response representation.
//
// Returns error message instead of response representation on error.
//
// Use Write instead of String for performance-critical code.
func (resp *Response) String() string {
	return httpString(resp)
}

func httpString(hw httpWriter) string {
	w := bytebufferpool.Get()
	defer bytebufferpool.Put(w)

	bw := bufio.NewWriter(w)
	if err := hw.Write(bw); err != nil {
		return err.Error()
	}
	if err := bw.Flush(); err != nil {
		return err.Error()
	}
	s := string(w.B)
	return s
}

type httpWriter interface {
	Write(w *bufio.Writer) error
}

func writeBodyChunked(w *bufio.Writer, r io.Reader) (err error) {
	pb := pool.Get(4096)
	pb.B = pb.B[:cap(pb.B)]
	buf := pb.B
	var n int
	for {
		n, err = r.Read(buf)
		if n == 0 {
			if err == nil {
				continue
			}
			if err == io.EOF {
				if _, err = w.Write(chunkedEnd); err != nil {
					break
				}
				err = nil
			}
			break
		}
		if err = writeChunk(w, buf[:n]); err != nil {
			break
		}
	}
	pb.RecycleToPool00()
	return
}

func limitedReaderSize(r io.Reader) int64 {
	lr, ok := r.(*io.LimitedReader)
	if !ok {
		return -1
	}
	return lr.N
}

type ErrStreamSendSizeNotMatch struct {
	sent int64
	want int64
}

func (es *ErrStreamSendSizeNotMatch) Error() string {
	return "copied " + strconv.FormatInt(es.sent, 10) + " bytes from body stream instead of " + strconv.FormatInt(es.want, 10) + " bytes"
}

func writeBodyFixedSize(w *bufio.Writer, r io.Reader, size int64) (err error) {
	var (
		n          int64
		earlyFlush bool
	)
	if size > MaxSmallFileSize {
		switch r := r.(type) {
		case *os.File:
			earlyFlush = true
		case *io.LimitedReader:
			_, earlyFlush = r.R.(*os.File)
		}
		if !earlyFlush {
			s, ok := r.(SendFiler)
			if ok {
				r = s.FileOrLimitedReader()
				earlyFlush = true
			}
		}

	}
	if earlyFlush {
		// w buffer must be empty for triggering
		// sendfile path in bufio.Writer.ReadFrom.
		if err = w.Flush(); err != nil {
			return
		}
		n, err = w.ReadFrom(r)
		if err != nil {
			return
		}
	} else {
		n, err = copyZeroAlloc(w, r)
	}
	if n != size && err == nil {
		// check size match.
		err = &ErrStreamSendSizeNotMatch{sent: n, want: size}
	}
	return
}

func copyZeroAlloc(w io.Writer, r io.Reader) (n int64, err error) {
	// Below repeat as io.CopyBuffer, because we can delay get buf from pool. if first two
	// assert stratify.
	if wt, ok := r.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}

	if rt, ok := w.(io.ReaderFrom); ok {
		return rt.ReadFrom(r)
	}

	buf := pool.Get(32 * 1024)
	buf.B = buf.B[:cap(buf.B)]
	n, err = pio.CopyBufferMust(w, r, buf.B)
	pool.Put(buf)
	return
}

func writeChunk(w *bufio.Writer, b []byte) error {
	n := len(b)
	if err := writeHexInt(w, n); err != nil {
		return err
	}
	if _, err := w.Write(strCRLF); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	// If is end chunk, write CRLF after writing trailer
	// len(b)==0 had been filtered.
	//if n > 0 {
	if _, err := w.Write(strCRLF); err != nil {
		return err
	}
	//}
	return w.Flush()
}

type HTTPError string

// ErrBodyTooLarge is returned if either request or response body exceeds
// the given limit.
var ErrBodyTooLarge = errors.New("body size exceeds the given limit")

func readBody(r *bufio.Reader, contentLength, maxBodySize int64, dst []byte) ([]byte, error) {
	if maxBodySize > 0 && contentLength > maxBodySize {
		return dst, ErrBodyTooLarge
	}
	return appendBodyFixedSize(r, dst, int(contentLength))
}

var errChunkedStream = errors.New("chunked stream")

func readBodyWithStreaming(r *bufio.Reader, contentLength, maxBodySize int64, dst []byte) (b []byte, allInBuf bool, err error) {
	if contentLength == -1 {
		// handled in requestStream.Read()
		err = errChunkedStream
		return
	}

	dst = dst[:0]

	readN := maxBodySize
	if readN > contentLength {
		readN = contentLength
	}
	// TODO Should this constant value be made configurable?
	if readN > 8*1024 {
		readN = 8 * 1024
	}
	cl := int(contentLength)
	rSize := r.Size()
	if readN+int64(rSize) >= contentLength {
		if cl <= rSize {
			allInBuf = true
			b, err = r.Peek(cl)
			if err == nil {
				_, err = r.Discard(cl)
			}
			return
		}
		newReadN := cl - rSize
		b, err = appendBodyFixedSize(r, dst, newReadN)
		if err != nil {
			return
		}
		_, er := r.Peek(rSize)
		err = er
		return
	}

	// A fixed-length pre-read function should be used here; otherwise,
	// it may read content beyond the request body into areas outside
	// the br buffer. This could affect the handling of the next request
	// in the br buffer, if there is one. The original two branches can
	// be handled with this single branch. by the way,
	// fix issue: https://github.com/valyala/fasthttp/issues/1816
	b, err = appendBodyFixedSize(r, dst, int(readN))
	if err != nil {
		return
	}
	err = ErrBodyTooLarge
	return
}

func readBodyIdentity(r *bufio.Reader, maxBodySize int64, dst []byte) (re []byte, err error) {
	dst = dst[:cap(dst)]
	if len(dst) == 0 {
		dst = make([]byte, 1024)
	}
	offset := 0
	nn := 0
	for {
		nn, err = r.Read(dst[offset:])
		if nn <= 0 {
			re = dst[:offset]
			if err == io.EOF {
				err = nil
				return
			}
			if err != nil {
				return
			}
			err = fmt.Errorf("bufio.Read() returned (%d, nil)", nn)
			return
		}
		offset += nn
		if maxBodySize > 0 && int64(offset) > maxBodySize {
			return dst[:offset], ErrBodyTooLarge
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			re = dst[:offset]
			return
		}
		if len(dst) == offset {
			n := roundUpForSliceCap(2 * offset)
			if maxBodySize > 0 && int64(n) > maxBodySize {
				n = int(maxBodySize) + 1
			}
			b := make([]byte, n)
			copy(b, dst)
			dst = b
		}
	}
}

type ReadBodyErr struct {
	s string
}

func (e *ReadBodyErr) Error() string {
	return e.s
}
func newReadBodyErr(what, val string) error {
	return &ReadBodyErr{what + ": " + val}
}

func appendBodyFixedSize(r *bufio.Reader, dst []byte, n int) ([]byte, error) {
	if n == 0 {
		return dst, nil
	}

	offset := len(dst)
	dstLen := offset + n
	if cap(dst) < dstLen {
		b := make([]byte, roundUpForSliceCap(dstLen))
		copy(b, dst)
		dst = b
	}
	dst = dst[:dstLen]

	for {
		nn, err := r.Read(dst[offset:])
		if nn <= 0 {
			if err == nil {
				return dst[:offset], errors.New("bufio.Read() returned (" + strconv.Itoa(nn) + ", nil)")
			}
			return dst[:offset], err
		}
		offset += nn
		if offset == dstLen {
			return dst, nil
		}
	}
}

// ErrBrokenChunk is returned when server receives a broken chunked body (Transfer-Encoding: chunked).
type ErrBrokenChunk struct {
	error
}

func readBodyChunked(r *bufio.Reader, maxBodySize int64, dst []byte) ([]byte, error) {
	if len(dst) > 0 {
		// data integrity might be in danger. No idea what we received,
		// but nothing we should write to.
		panic("BUG: expected zero-length buffer")
	}

	strCRLFLen := len(strCRLF)
	for {
		chunkSize, err := parseChunkSize(r)
		if err != nil {
			return dst, err
		}
		if chunkSize == 0 {
			return dst, err
		}
		if maxBodySize > 0 && int64(len(dst)+chunkSize) > maxBodySize {
			return dst, ErrBodyTooLarge
		}
		dst, err = appendBodyFixedSize(r, dst, chunkSize+strCRLFLen)
		if err != nil {
			return dst, err
		}
		if !bytes.Equal(dst[len(dst)-strCRLFLen:], strCRLF) {
			return dst, ErrBrokenChunk{
				error: errors.New("cannot find crlf at the end of chunk"),
			}
		}
		dst = dst[:len(dst)-strCRLFLen]
	}
}

func parseChunkSize(r *bufio.Reader) (n int, err error) {
	n, err = readHexInt(r)
	if err != nil {
		n = -1
		return
	}
	var c byte
	for {
		c, err = r.ReadByte()
		if err != nil {
			n = -1
			return
		}
		// Skip chunk extension after chunk size.
		// Add support later if anyone needs it.
		if c != '\r' {
			continue
		}
		if err = r.UnreadByte(); err != nil {
			n = -1
			return
		}
		break
	}
	err = readCrLf(r)
	if err != nil {
		n = -1
		return
	}
	return
}

func readCrLf(r *bufio.Reader) error {
	for _, exp := range strCRLF {
		c, err := r.ReadByte()
		if err != nil {
			return err
		}
		if c != exp {
			return ErrBrokenChunk{
				error: errors.New(`unexpected char "` + string(c) + `" at the end of chunk size. Expected "` + string(exp) + `"`),
			}
		}
	}
	return nil
}

// SetTimeout sets timeout for the request.
//
// The following code:
//
//	req.SetTimeout(t)
//	c.Do(&req, &resp)
//
// is equivalent to
//
//	c.DoTimeout(&req, &resp, t)
func (req *Request) SetTimeout(t time.Duration) {
	req.timeout = t
}
