package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/newacorn/goutils/compress"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rChar = byte('\r')
	nChar = byte('\n')
)

// ResponseHeader represents HTTP response header.
//
// It is forbidden copying ResponseHeader instances.
// Create new instances instead and use CopyTo.
//
// ResponseHeader instance MUST NOT be used from concurrently running
// goroutines.
type ResponseHeader struct {
	noCopy noCopy

	statusMessage      []byte
	protocol           []byte
	contentLengthBytes []byte

	contentType     []byte
	contentEncoding []byte
	server          []byte
	mulHeader       [][]byte

	h       []argsKV
	trailer []argsKV

	cookies []argsKV
	//bufKV   argsKV
	bufK []byte
	bufV []byte

	statusCode    int
	contentLength int64

	//
	noHTTP11        bool
	connectionClose bool
	//
	disableNormalizing    bool
	noDefaultContentType  bool
	secureErrorLogMessage bool
	headMethod            bool
	noDefaultDate         bool
}

// RequestHeader represents HTTP request header.
//
// It is forbidden copying RequestHeader instances.
// Create new instances instead and use CopyTo.
//
// RequestHeader instance MUST NOT be used from concurrently running
// goroutines.
type RequestHeader struct {
	noCopy             noCopy
	contentLengthBytes []byte
	method             []byte
	requestURI         []byte
	proto              []byte
	host               []byte
	contentType        []byte
	userAgent          []byte
	mulHeader          [][]byte
	h                  []argsKV
	trailer            []argsKV
	cookies            []argsKV
	// stores an immutable copy of headers as they were received from the
	// wire.
	rawHeaders    []byte
	contentLength int64
	//bufKV         argsKV
	bufK []byte
	bufV []byte
	//
	noHTTP11        bool
	connectionClose bool
	// These two fields have been moved close to other bool fields
	// for reducing RequestHeader object size.
	cookiesCollected bool
	// Does the request header contain the `Content-Length` header?
	contentLengthSeen bool
	//
	disableSpecialHeader  bool
	disableNormalizing    bool
	noDefaultContentType  bool
	secureErrorLogMessage bool
	//
	// Avoid parsing the first line of the request header repeatedly.
	firstLineLen int32
}

// SetContentRange sets 'Content-Range: bytes startPos-endPos/contentLength'
// header.
func (h *ResponseHeader) SetContentRange(startPos, endPos, contentLength int64) {
	b := h.bufV[:0]
	b = append(b, strBytes...)
	b = append(b, ' ')
	b = AppendUint(b, startPos)
	b = append(b, '-')
	b = AppendUint(b, endPos)
	b = append(b, '/')
	b = AppendUint(b, contentLength)
	h.bufV = b

	h.setNonSpecial(strContentRange, h.bufV)
}

// SetByteRange sets 'Range: bytes=startPos-endPos' header.
//
//   - If startPos is negative, then 'bytes=-startPos' value is set.
//   - If endPos is negative, then 'bytes=startPos-' value is set.
func (h *RequestHeader) SetByteRange(startPos, endPos int64) {
	b := h.bufV[:0]
	b = append(b, strBytes...)
	b = append(b, '=')
	if startPos >= 0 {
		b = AppendUint(b, startPos)
	} else {
		endPos = -startPos
	}
	b = append(b, '-')
	if endPos >= 0 {
		b = AppendUint(b, endPos)
	}
	h.bufV = b

	h.setNonSpecial(strRange, h.bufV)
}

// StatusCode returns response status code.
func (h *ResponseHeader) StatusCode() int {
	//if h.statusCode == 0 {
	//	return StatusOK
	//}
	return h.statusCode
}

// SetStatusCode sets response status code.
func (h *ResponseHeader) SetStatusCode(statusCode int) {
	h.statusCode = statusCode
}

// StatusMessage returns response status message.
func (h *ResponseHeader) StatusMessage() []byte {
	return h.statusMessage
}

// SetStatusMessage sets response status message bytes.
func (h *ResponseHeader) SetStatusMessage(statusMessage []byte) {
	h.statusMessage = append(h.statusMessage[:0], statusMessage...)
}

// Protocol returns response protocol bytes.
func (h *ResponseHeader) Protocol() []byte {
	if len(h.protocol) > 0 {
		return h.protocol
	}
	return strHTTP11
}

// SetProtocol sets response protocol bytes.
func (h *ResponseHeader) SetProtocol(protocol []byte) {
	h.protocol = append(h.protocol[:0], protocol...)
}

// SetLastModified sets 'Last-Modified' header to the given value.
func (h *ResponseHeader) SetLastModified(t time.Time) {
	h.bufV = AppendHTTPDate(h.bufV[:0], t)
	h.setNonSpecial(strLastModified, h.bufV)
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (h *ResponseHeader) ConnectionClose() bool {
	return h.connectionClose
}

// SetConnectionClose sets 'Connection: close' header.
func (h *ResponseHeader) SetConnectionClose() {
	h.connectionClose = true
}

// ResetConnectionClose clears 'Connection: close' header if it exists.
func (h *ResponseHeader) ResetConnectionClose() {
	if h.connectionClose {
		h.connectionClose = false
		h.h = delAllArgsBytes(h.h, strConnection)
	}
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (h *RequestHeader) ConnectionClose() bool {
	return h.connectionClose
}

// SetConnectionClose sets 'Connection: close' header.
func (h *RequestHeader) SetConnectionClose() {
	h.connectionClose = true
}

// ResetConnectionClose clears 'Connection: close' header if it exists.
func (h *RequestHeader) ResetConnectionClose() {
	if h.connectionClose {
		h.connectionClose = false
		h.h = delAllArgsBytes(h.h, strConnection)
	}
}

// ConnectionUpgrade returns true if 'Connection: Upgrade' header is set.
func (h *ResponseHeader) ConnectionUpgrade() bool {
	return hasHeaderValue(h.Peek(HeaderConnection), strUpgrade)
}

// ConnectionUpgrade returns true if 'Connection: Upgrade' header is set.
func (h *RequestHeader) ConnectionUpgrade() bool {
	return hasHeaderValue(h.Peek(HeaderConnection), strUpgrade)
}

// PeekCookie is able to returns cookie by a given key from response.
func (h *ResponseHeader) PeekCookie(key string) []byte {
	return peekArgStr(h.cookies, key)
}

// ContentLength returns Content-Length header value.
//
// It may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *ResponseHeader) ContentLength() int64 {
	return h.contentLength
}

// SetContentLength sets Content-Length header value.
//
// Content-Length may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *ResponseHeader) SetContentLength(contentLength int64) {
	h.contentLength = contentLength
	// TODO dont need handle below logic, we can handle in header write.
}
func bodyNotAllowedForStatus(statusCode int) bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	// Fast path.
	if statusCode < 100 || statusCode == StatusOK {
		return false
	}
	// Slow path.
	return statusCode == StatusNotModified || statusCode == StatusNoContent || statusCode < 200
}

func (h *ResponseHeader) mustSkipContentLength() bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	statusCode := h.StatusCode()

	// Fast path.
	if statusCode < 100 || statusCode == StatusOK {
		return false
	}

	// Slow path.
	return statusCode == StatusNotModified || statusCode == StatusNoContent || statusCode < 200
}

// ContentLength returns Content-Length header value.
//
// It may be negative:
// -1 means Transfer-Encoding: chunked.
func (h *RequestHeader) ContentLength() int64 {
	return h.realContentLength()
}

// realContentLength returns the actual Content-Length set in the request,
// including positive lengths for GET/HEAD requests.
func (h *RequestHeader) realContentLength() int64 {
	return h.contentLength
}

// SetContentLength sets Content-Length header value.
//
// Negative content-length sets 'Transfer-Encoding: chunked' header.
func (h *RequestHeader) SetContentLength(contentLength int64) {
	h.contentLength = contentLength
	if contentLength >= 0 {
		h.contentLengthBytes = AppendUint(h.contentLengthBytes[:0], contentLength)
		h.h = delAllArgsBytes(h.h, strTransferEncoding)
	} else {
		h.contentLengthBytes = h.contentLengthBytes[:0]
		h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
	}
}

func (h *ResponseHeader) isCompressibleContentType() bool {
	return compress.CheckMimeOk(h.ContentType())
}

// ContentType returns Content-Type header value.
func (h *ResponseHeader) ContentType() []byte {
	ct := h.contentType
	if !h.noDefaultContentType && len(h.contentType) == 0 {
		ct = defaultContentType
	}
	return ct
}

// SetContentType sets Content-Type header value.
func (h *ResponseHeader) SetContentType(contentType string) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// SetContentTypeBytes sets Content-Type header value.
func (h *ResponseHeader) SetContentTypeBytes(contentType []byte) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// ContentEncoding returns Content-Encoding header value.
func (h *ResponseHeader) ContentEncoding() []byte {
	return h.contentEncoding
}

// SetContentEncoding sets Content-Encoding header value.
func (h *ResponseHeader) SetContentEncoding(contentEncoding string) {
	h.contentEncoding = append(h.contentEncoding[:0], contentEncoding...)
}

// SetContentEncodingBytes sets Content-Encoding header value.
func (h *ResponseHeader) SetContentEncodingBytes(contentEncoding []byte) {
	h.contentEncoding = append(h.contentEncoding[:0], contentEncoding...)
}

// addVaryBytes add value to the 'Vary' header if it's not included.
func (h *ResponseHeader) addVaryBytes(value []byte) {
	v := h.peek(strVary)
	if len(v) == 0 {
		// 'Vary' is not set
		h.SetBytesV(HeaderVary, value)
	} else if !bytes.Contains(v, value) {
		// 'Vary' is set and not contains target value
		h.SetBytesV(HeaderVary, append(append(v, ','), value...))
	} // else: 'Vary' is set and contains target value
}

// Server returns Server header value.
func (h *ResponseHeader) Server() []byte {
	return h.server
}

// SetServer sets Server header value.
func (h *ResponseHeader) SetServer(server string) {
	h.server = append(h.server[:0], server...)
}

// SetServerBytes sets Server header value.
func (h *ResponseHeader) SetServerBytes(server []byte) {
	h.server = append(h.server[:0], server...)
}

// ContentType returns Content-Type header value.
func (h *RequestHeader) ContentType() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderContentType))
	}
	return h.contentType
}

// SetContentType sets Content-Type header value.
func (h *RequestHeader) SetContentType(contentType string) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// SetContentTypeBytes sets Content-Type header value.
func (h *RequestHeader) SetContentTypeBytes(contentType []byte) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// ContentEncoding returns Content-Encoding header value.
func (h *RequestHeader) ContentEncoding() []byte {
	return peekArgBytes(h.h, strContentEncoding)
}

// SetContentEncoding sets Content-Encoding header value.
func (h *RequestHeader) SetContentEncoding(contentEncoding string) {
	h.SetBytesK(strContentEncoding, contentEncoding)
}

// SetContentEncodingBytes sets Content-Encoding header value.
func (h *RequestHeader) SetContentEncodingBytes(contentEncoding []byte) {
	h.setNonSpecial(strContentEncoding, contentEncoding)
}

// SetMultipartFormBoundary sets the following Content-Type:
// 'multipart/form-data; boundary=...'
// where ... is substituted by the given boundary.
func (h *RequestHeader) SetMultipartFormBoundary(boundary string) {
	b := h.bufV[:0]
	b = append(b, strMultipartFormData...)
	b = append(b, ';', ' ')
	b = append(b, strBoundary...)
	b = append(b, '=')
	b = append(b, boundary...)
	h.bufV = b

	h.SetContentTypeBytes(h.bufV)
}

// SetMultipartFormBoundaryBytes sets the following Content-Type:
// 'multipart/form-data; boundary=...'
// where ... is substituted by the given boundary.
func (h *RequestHeader) SetMultipartFormBoundaryBytes(boundary []byte) {
	b := h.bufV[:0]
	b = append(b, strMultipartFormData...)
	b = append(b, ';', ' ')
	b = append(b, strBoundary...)
	b = append(b, '=')
	b = append(b, boundary...)
	h.bufV = b

	h.SetContentTypeBytes(h.bufV)
}

// SetTrailer sets header Trailer value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *ResponseHeader) SetTrailer(trailer string) error {
	return h.SetTrailerBytes(s2b(trailer))
}

// SetTrailerBytes sets Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *ResponseHeader) SetTrailerBytes(trailer []byte) error {
	h.trailer = h.trailer[:0]
	return h.AddTrailerBytes(trailer)
}

// AddTrailer add Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *ResponseHeader) AddTrailer(trailer string) error {
	return h.AddTrailerBytes(s2b(trailer))
}

var ErrBadTrailer = errors.New("contain forbidden trailer")

// AddTrailerBytes add Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *ResponseHeader) AddTrailerBytes(trailer []byte) error {
	var err error
	for i := -1; i+1 < len(trailer); {
		trailer = trailer[i+1:]
		i = bytes.IndexByte(trailer, ',')
		if i < 0 {
			i = len(trailer)
		}
		key := trailer[:i]
		for len(key) > 0 && key[0] == ' ' {
			key = key[1:]
		}
		for len(key) > 0 && key[len(key)-1] == ' ' {
			key = key[:len(key)-1]
		}
		// Forbidden by RFC 7230, section 4.1.2
		if isBadTrailer(key, h.disableNormalizing) {
			err = ErrBadTrailer
			continue
		}
		key = initHeaderK(&h.bufK, key, h.disableNormalizing)
		h.trailer = appendArgBytes(h.trailer, key, nil, argsNoValue)
	}

	return err
}

// validHeaderFieldByte returns true if c valid header field byte
// as defined by RFC 7230.
func validHeaderFieldByte(c byte) bool {
	return c < 128 && validHeaderFieldByteTable[c] == 1
}

// validHeaderValueByte returns true if c valid header value byte
// as defined by RFC 7230.
func validHeaderValueByte(c byte) bool {
	return validHeaderValueByteTable[c] == 1
}

// VisitHeaderParams calls f for each parameter in the given header bytes.
// It stops processing when f returns false or an invalid parameter is found.
// Parameter values may be quoted, in which case \ is treated as an escape
// character, and the value is unquoted before being passed to value.
// See: https://www.rfc-editor.org/rfc/rfc9110#section-5.6.6
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
func VisitHeaderParams(b []byte, f func(key, value []byte) bool) {
	for len(b) > 0 {
		idxSemi := 0
		for idxSemi < len(b) && b[idxSemi] != ';' {
			idxSemi++
		}
		if idxSemi >= len(b) {
			return
		}
		b = b[idxSemi+1:]
		for len(b) > 0 && b[0] == ' ' {
			b = b[1:]
		}

		n := 0
		if len(b) == 0 || !validHeaderFieldByte(b[n]) {
			return
		}
		n++
		for n < len(b) && validHeaderFieldByte(b[n]) {
			n++
		}

		if n >= len(b)-1 || b[n] != '=' {
			return
		}
		param := b[:n]
		n++

		switch {
		case validHeaderFieldByte(b[n]):
			m := n
			n++
			for n < len(b) && validHeaderFieldByte(b[n]) {
				n++
			}
			if !f(param, b[m:n]) {
				return
			}
		case b[n] == '"':
			foundEndQuote := false
			escaping := false
			n++
			m := n
			for ; n < len(b); n++ {
				if b[n] == '"' && !escaping {
					foundEndQuote = true
					break
				}
				escaping = (b[n] == '\\' && !escaping)
			}
			if !foundEndQuote {
				return
			}
			if !f(param, b[m:n]) {
				return
			}
			n++
		default:
			return
		}
		b = b[n:]
	}
}

// MultipartFormBoundary returns boundary part
// from 'multipart/form-data; boundary=...' Content-Type.
func (h *RequestHeader) MultipartFormBoundary() []byte {
	b := h.ContentType()
	if !bytes.HasPrefix(b, strMultipartFormData) {
		return nil
	}
	b = b[len(strMultipartFormData):]
	if len(b) == 0 || b[0] != ';' {
		return nil
	}

	var n int
	for len(b) > 0 {
		n++
		for len(b) > n && b[n] == ' ' {
			n++
		}
		b = b[n:]
		if !bytes.HasPrefix(b, strBoundary) {
			if n = bytes.IndexByte(b, ';'); n < 0 {
				return nil
			}
			continue
		}

		b = b[len(strBoundary):]
		if len(b) == 0 || b[0] != '=' {
			return nil
		}
		b = b[1:]
		if n = bytes.IndexByte(b, ';'); n >= 0 {
			b = b[:n]
		}
		if len(b) > 1 && b[0] == '"' && b[len(b)-1] == '"' {
			b = b[1 : len(b)-1]
		}
		return b
	}
	return nil
}

// Host returns Host header value.
func (h *RequestHeader) Host() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderHost))
	}
	return h.host
}

// SetHost sets Host header value.
func (h *RequestHeader) SetHost(host string) {
	h.host = append(h.host[:0], host...)
}

// SetHostBytes sets Host header value.
func (h *RequestHeader) SetHostBytes(host []byte) {
	h.host = append(h.host[:0], host...)
}

func (h *RequestHeader) setHost(host []byte) {
	h.host = append(h.host[:0], host...)
}

// UserAgent returns User-Agent header value.
func (h *RequestHeader) UserAgent() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderUserAgent))
	}
	return h.userAgent
}

// SetUserAgent sets User-Agent header value.
func (h *RequestHeader) SetUserAgent(userAgent string) {
	h.userAgent = append(h.userAgent[:0], userAgent...)
}

// SetUserAgentBytes sets User-Agent header value.
func (h *RequestHeader) SetUserAgentBytes(userAgent []byte) {
	h.userAgent = append(h.userAgent[:0], userAgent...)
}

// Referer returns Referer header value.
func (h *RequestHeader) Referer() []byte {
	return peekArgBytes(h.h, strReferer)
}

// SetReferer sets Referer header value.
func (h *RequestHeader) SetReferer(referer string) {
	h.SetBytesK(strReferer, referer)
}

// SetRefererBytes sets Referer header value.
func (h *RequestHeader) SetRefererBytes(referer []byte) {
	h.setNonSpecial(strReferer, referer)
}

// Method returns HTTP request method.
func (h *RequestHeader) Method() []byte {
	if len(h.method) == 0 {
		return []byte(MethodGet)
	}
	return h.method
}

// SetMethod sets HTTP request method.
func (h *RequestHeader) SetMethod(method string) {
	h.method = append(h.method[:0], method...)
}

// SetMethodBytes sets HTTP request method.
func (h *RequestHeader) SetMethodBytes(method []byte) {
	h.method = append(h.method[:0], method...)
}

// Protocol returns HTTP protocol.
func (h *RequestHeader) Protocol() []byte {
	if len(h.proto) == 0 {
		return strHTTP11
	}
	return h.proto
}

// SetProtocol sets HTTP request protocol.
func (h *RequestHeader) SetProtocol(method string) {
	h.proto = append(h.proto[:0], method...)
	h.noHTTP11 = !bytes.Equal(h.proto, strHTTP11)
}

// SetProtocolBytes sets HTTP request protocol.
func (h *RequestHeader) SetProtocolBytes(method []byte) {
	h.proto = append(h.proto[:0], method...)
	h.noHTTP11 = !bytes.Equal(h.proto, strHTTP11)
}

// RequestURI returns RequestURI from the first HTTP request line.
func (h *RequestHeader) RequestURI() []byte {
	requestURI := h.requestURI
	if len(requestURI) == 0 {
		requestURI = strSlash
	}
	return requestURI
}

// SetRequestURI sets RequestURI for the first HTTP request line.
// RequestURI must be properly encoded.
// Use URI.RequestURI for constructing proper RequestURI if unsure.
func (h *RequestHeader) SetRequestURI(requestURI string) {
	h.requestURI = append(h.requestURI[:0], requestURI...)
}

const (
	HTTPSchemePrefix  = "http://"
	HTTPSSchemePrefix = "https://"
)

func (h *RequestHeader) setRequestURI(requestURI []byte) {
	h.requestURI = append(h.requestURI[:0], requestURI...)
}

// SetRequestURIBytes sets RequestURI for the first HTTP request line.
// RequestURI must be properly encoded.
// Use URI.RequestURI for constructing proper RequestURI if unsure.
func (h *RequestHeader) SetRequestURIBytes(requestURI []byte) {
	h.requestURI = append(h.requestURI[:0], requestURI...)
}

// SetTrailer sets Trailer header value for chunked request
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *RequestHeader) SetTrailer(trailer string) error {
	return h.SetTrailerBytes(s2b(trailer))
}

// SetTrailerBytes sets Trailer header value for chunked request
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *RequestHeader) SetTrailerBytes(trailer []byte) error {
	h.trailer = h.trailer[:0]
	return h.AddTrailerBytes(trailer)
}

// AddTrailer add Trailer header value for chunked request
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *RequestHeader) AddTrailer(trailer string) error {
	return h.AddTrailerBytes(s2b(trailer))
}

// AddTrailerBytes add Trailer header value for chunked request
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *RequestHeader) AddTrailerBytes(trailer []byte) error {
	var err error
	for i := -1; i+1 < len(trailer); {
		trailer = trailer[i+1:]
		i = bytes.IndexByte(trailer, ',')
		if i < 0 {
			i = len(trailer)
		}
		key := trailer[:i]
		for len(key) > 0 && key[0] == ' ' {
			key = key[1:]
		}
		for len(key) > 0 && key[len(key)-1] == ' ' {
			key = key[:len(key)-1]
		}
		// Forbidden by RFC 7230, section 4.1.2
		key = initHeaderK(&h.bufK, key, h.disableNormalizing)
		if isBadTrailer(key, h.disableNormalizing) {
			err = newHeaderParseErr("bad trailer key", b2s(key))
			continue
		}
		h.trailer = appendArgBytes(h.trailer, key, nil, argsNoValue)
	}

	return err
}

// IsGet returns true if request method is GET.
func (h *RequestHeader) IsGet() bool {
	return string(h.Method()) == MethodGet
}

// IsPost returns true if request method is POST.
func (h *RequestHeader) IsPost() bool {
	return string(h.Method()) == MethodPost
}

// IsPut returns true if request method is PUT.
func (h *RequestHeader) IsPut() bool {
	return string(h.Method()) == MethodPut
}

// IsHead returns true if request method is HEAD.
func (h *RequestHeader) IsHead() bool {
	return string(h.Method()) == MethodHead
}

// IsDelete returns true if request method is DELETE.
func (h *RequestHeader) IsDelete() bool {
	return string(h.Method()) == MethodDelete
}

// IsConnect returns true if request method is CONNECT.
func (h *RequestHeader) IsConnect() bool {
	return string(h.Method()) == MethodConnect
}

// IsOptions returns true if request method is OPTIONS.
func (h *RequestHeader) IsOptions() bool {
	return string(h.Method()) == MethodOptions
}

// IsTrace returns true if request method is TRACE.
func (h *RequestHeader) IsTrace() bool {
	return string(h.Method()) == MethodTrace
}

// IsPatch returns true if request method is PATCH.
func (h *RequestHeader) IsPatch() bool {
	return string(h.Method()) == MethodPatch
}

// IsHTTP11 returns true if the request is HTTP/1.1.
func (h *RequestHeader) IsHTTP11() bool {
	return !h.noHTTP11
}

// IsHTTP11 returns true if the response is HTTP/1.1.
func (h *ResponseHeader) IsHTTP11() bool {
	return !h.noHTTP11
}

// HasAcceptEncoding returns true if the header contains
// the given Accept-Encoding value.
func (h *RequestHeader) HasAcceptEncoding(acceptEncoding string) bool {
	h.bufV = append(h.bufV[:0], acceptEncoding...)
	return h.HasAcceptEncodingBytes(h.bufV)
}

// HasAcceptEncodingBytes returns true if the header contains
// the given Accept-Encoding value.
func (h *RequestHeader) HasAcceptEncodingBytes(acceptEncoding []byte) bool {
	ae := h.peek(strAcceptEncoding)
	n := bytes.Index(ae, acceptEncoding)
	if n < 0 {
		return false
	}
	b := ae[n+len(acceptEncoding):]
	if len(b) > 0 && b[0] != ',' {
		return false
	}
	if n == 0 {
		return true
	}
	return ae[n-1] == ' '
}

func HasAcceptEncodingBytes(ae []byte, acceptEncoding []byte) bool {
	n := bytes.Index(ae, acceptEncoding)
	if n < 0 {
		return false
	}
	b := ae[n+len(acceptEncoding):]
	if len(b) > 0 && b[0] != ',' {
		return false
	}
	if n == 0 {
		return true
	}
	return ae[n-1] == ' '
}

func (h *ResponseHeader) Charset() []byte {
	ct := h.Peek(HeaderContentType)
	if len(ct) == 0 {
		return s2b(CharsetUTF8)
	}
	i := bytes.LastIndexByte(ct, '=')
	if i == -1 {
		return s2b(CharsetUTF8)
	}
	return ct[i+1:]
}

// Len returns the number of headers set,
// i.e. the number of times f is called in VisitAll.
func (h *ResponseHeader) Len() int {
	n := 0
	h.VisitAll(func(_, _ []byte) { n++ })
	return n
}

// Len returns the number of headers set,
// i.e. the number of times f is called in VisitAll.
func (h *RequestHeader) Len() int {
	n := 0
	h.VisitAll(func(_, _ []byte) { n++ })
	return n
}

// DisableSpecialHeader disables special header processing.
// fasthttp will not set any special headers for you, such as Host, Content-Type, User-Agent, etc.
// You must set everything yourself.
// If RequestHeader.Read() is called, special headers will be ignored.
// This can be used to control case and order of special headers.
// This is generally not recommended.
func (h *RequestHeader) DisableSpecialHeader() {
	h.disableSpecialHeader = true
}

// EnableSpecialHeader enables special header processing.
// fasthttp will send Host, Content-Type, User-Agent, etc headers for you.
// This is suggested and enabled by default.
func (h *RequestHeader) EnableSpecialHeader() {
	h.disableSpecialHeader = false
}

// DisableNormalizing disables header names' normalization.
//
// By default all the header names are normalized by uppercasing
// the first letter and all the first letters following dashes,
// while lowercasing all the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// Disable header names' normalization only if know what are you doing.
func (h *RequestHeader) DisableNormalizing() {
	h.disableNormalizing = true
}

// EnableNormalizing enables header names' normalization.
//
// Header names are normalized by uppercasing the first letter and
// all the first letters following dashes, while lowercasing all
// the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// This is enabled by default unless disabled using DisableNormalizing().
func (h *RequestHeader) EnableNormalizing() {
	h.disableNormalizing = false
}

// DisableNormalizing disables header names' normalization.
//
// By default all the header names are normalized by uppercasing
// the first letter and all the first letters following dashes,
// while lowercasing all the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// Disable header names' normalization only if know what are you doing.
func (h *ResponseHeader) DisableNormalizing() {
	h.disableNormalizing = true
}

// EnableNormalizing enables header names' normalization.
//
// Header names are normalized by uppercasing the first letter and
// all the first letters following dashes, while lowercasing all
// the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// This is enabled by default unless disabled using DisableNormalizing().
func (h *ResponseHeader) EnableNormalizing() {
	h.disableNormalizing = false
}

// SetNoDefaultContentType allows you to control if a default Content-Type header will be set (false) or not (true).
func (h *ResponseHeader) SetNoDefaultContentType(noDefaultContentType bool) {
	h.noDefaultContentType = noDefaultContentType
}

// Reset clears response header.
func (h *ResponseHeader) Reset() {
	h.disableNormalizing = false
	h.noDefaultContentType = false
	h.secureErrorLogMessage = false
	h.headMethod = false
	h.noDefaultDate = false
	h.resetSkipNormalize()
}

func (h *ResponseHeader) resetSkipNormalize() {

	h.statusMessage = h.statusMessage[:0]
	h.protocol = h.protocol[:0]
	h.contentLengthBytes = h.contentLengthBytes[:0]
	h.contentType = h.contentType[:0]
	h.contentEncoding = h.contentEncoding[:0]
	h.server = h.server[:0]
	h.mulHeader = h.mulHeader[:0]
	h.h = h.h[:0]
	h.trailer = h.trailer[:0]
	h.cookies = h.cookies[:0]
	h.statusCode = 0
	h.contentLength = 0
	//
	h.noHTTP11 = false
	h.connectionClose = false
}

// SetNoDefaultContentType allows you to control if a default Content-Type header will be set (false) or not (true).
func (h *RequestHeader) SetNoDefaultContentType(noDefaultContentType bool) {
	h.noDefaultContentType = noDefaultContentType
}

// Reset clears request header.
func (h *RequestHeader) Reset() {
	h.disableSpecialHeader = false
	h.disableNormalizing = false
	h.noDefaultContentType = false
	h.secureErrorLogMessage = false
	h.resetSkipNormalize()
}

func (h *RequestHeader) resetSkipNormalize() {
	h.contentLengthBytes = h.contentLengthBytes[:0]
	h.method = h.method[:0]
	h.requestURI = h.requestURI[:0]
	h.proto = h.proto[:0]
	h.host = h.host[:0]
	h.contentType = h.contentType[:0]
	h.userAgent = h.userAgent[:0]
	h.mulHeader = h.mulHeader[:0]
	h.h = h.h[:0]
	h.trailer = h.trailer[:0]
	h.cookies = h.cookies[:0]
	h.rawHeaders = h.rawHeaders[:0]
	h.contentLength = 0
	h.firstLineLen = 0
	//
	h.noHTTP11 = false
	h.connectionClose = false
	h.cookiesCollected = false
	h.contentLengthSeen = false
}

// CopyTo copies all the headers to dst.
func (h *ResponseHeader) CopyTo(dst *ResponseHeader) {
	dst.Reset()

	dst.disableNormalizing = h.disableNormalizing
	dst.noHTTP11 = h.noHTTP11
	dst.connectionClose = h.connectionClose
	dst.noDefaultContentType = h.noDefaultContentType
	dst.noDefaultDate = h.noDefaultDate

	dst.statusCode = h.statusCode
	dst.statusMessage = append(dst.statusMessage, h.statusMessage...)
	dst.protocol = append(dst.protocol, h.protocol...)
	dst.contentLength = h.contentLength
	dst.contentLengthBytes = append(dst.contentLengthBytes, h.contentLengthBytes...)
	dst.contentType = append(dst.contentType, h.contentType...)
	dst.contentEncoding = append(dst.contentEncoding, h.contentEncoding...)
	dst.server = append(dst.server, h.server...)
	dst.h = copyArgs(dst.h, h.h)
	dst.cookies = copyArgs(dst.cookies, h.cookies)
	dst.trailer = copyArgs(dst.trailer, h.trailer)
}

// CopyTo copies all the headers to dst.
func (h *RequestHeader) CopyTo(dst *RequestHeader) {
	dst.Reset()

	dst.disableNormalizing = h.disableNormalizing
	dst.noHTTP11 = h.noHTTP11
	dst.connectionClose = h.connectionClose
	dst.noDefaultContentType = h.noDefaultContentType

	dst.contentLength = h.contentLength
	dst.contentLengthBytes = append(dst.contentLengthBytes, h.contentLengthBytes...)
	dst.method = append(dst.method, h.method...)
	dst.proto = append(dst.proto, h.proto...)
	dst.requestURI = append(dst.requestURI, h.requestURI...)
	dst.host = append(dst.host, h.host...)
	dst.contentType = append(dst.contentType, h.contentType...)
	dst.userAgent = append(dst.userAgent, h.userAgent...)
	dst.trailer = append(dst.trailer, h.trailer...)
	dst.h = copyArgs(dst.h, h.h)
	dst.cookies = copyArgs(dst.cookies, h.cookies)
	dst.cookiesCollected = h.cookiesCollected
	dst.rawHeaders = append(dst.rawHeaders, h.rawHeaders...)
	dst.contentLengthSeen = h.contentLengthSeen
	dst.firstLineLen = h.firstLineLen
}

// VisitAll calls f for each header.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
func (h *ResponseHeader) VisitAll(f func(key, value []byte)) {
	if len(h.contentLengthBytes) > 0 {
		f(strContentLength, h.contentLengthBytes)
	}
	contentType := h.ContentType()
	if len(contentType) > 0 {
		f(strContentType, contentType)
	}
	contentEncoding := h.ContentEncoding()
	if len(contentEncoding) > 0 {
		f(strContentEncoding, contentEncoding)
	}
	server := h.Server()
	if len(server) > 0 {
		f(strServer, server)
	}
	if len(h.cookies) > 0 {
		visitArgs(h.cookies, func(_, v []byte) {
			f(strSetCookie, v)
		})
	}
	if len(h.trailer) > 0 {
		f(strTrailer, appendArgsKeyBytes(nil, h.trailer, strCommaSpace))
	}
	visitArgs(h.h, f)
	if h.ConnectionClose() {
		f(strConnection, strClose)
	}
}

// VisitAllTrailer calls f for each response Trailer.
//
// f must not retain references to value after returning.
func (h *ResponseHeader) VisitAllTrailer(f func(value []byte)) {
	visitArgsKey(h.trailer, f)
}

// VisitAllTrailer calls f for each request Trailer.
//
// f must not retain references to value after returning.
func (h *RequestHeader) VisitAllTrailer(f func(value []byte)) {
	visitArgsKey(h.trailer, f)
}

// VisitAllCookie calls f for each response cookie.
//
// Cookie name is passed in key and the whole Set-Cookie header value
// is passed in value on each f invocation. Value may be parsed
// with Cookie.ParseBytes().
//
// f must not retain references to key and/or value after returning.
func (h *ResponseHeader) VisitAllCookie(f func(key, value []byte)) {
	visitArgs(h.cookies, f)
}

// VisitAllCookie calls f for each request cookie.
//
// f must not retain references to key and/or value after returning.
func (h *RequestHeader) VisitAllCookie(f func(key, value []byte)) {
	h.collectCookies()
	visitArgs(h.cookies, f)
}

// VisitAll calls f for each header.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
//
// To get the headers in order they were received use VisitAllInOrder.
func (h *RequestHeader) VisitAll(f func(key, value []byte)) {
	host := h.Host()
	if len(host) > 0 {
		f(strHost, host)
	}
	if len(h.contentLengthBytes) > 0 {
		f(strContentLength, h.contentLengthBytes)
	}
	ct := h.ContentType()
	if len(ct) > 0 {
		f(strContentType, ct)
	}
	userAgent := h.UserAgent()
	if len(userAgent) > 0 {
		f(strUserAgent, userAgent)
	}
	if len(h.trailer) > 0 {
		f(strTrailer, appendArgsKeyBytes(nil, h.trailer, strCommaSpace))
	}

	h.collectCookies()
	if len(h.cookies) > 0 {
		h.bufV = appendRequestCookieBytes(h.bufV[:0], h.cookies)
		f(strCookie, h.bufV)
	}
	visitArgs(h.h, f)
	if h.ConnectionClose() {
		f(strConnection, strClose)
	}
}

// VisitAllInOrder calls f for each header in the order they were received.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
//
// This function is slightly slower than VisitAll because it has to reparse the
// raw headers to get the order.
func (h *RequestHeader) VisitAllInOrder(f func(key, value []byte)) {
	var s headerScanner
	s.b = h.rawHeaders
	s.disableNormalizing = h.disableNormalizing
	for s.next() {
		if len(s.key) > 0 {
			f(s.key, s.value)
		}
	}
}

// Del deletes header with the given key.
func (h *ResponseHeader) Del(key string) {
	h.DelBytes(s2b(key))
}

// DelCanonical deletes header with the given key assuming key
// is canonical form.
func (h *ResponseHeader) DelCanonical(key []byte) {
	h.del(key)
}

// DelCanonical deletes header with the given key assuming key
// is canonical form.
func (h *RequestHeader) DelCanonical(key []byte) {
	h.del(key)
}

func (h *ResponseHeader) DelNoSpecialCanonical(key []byte) {
	h.h = delAllArgsBytes(h.h, key)
}

func (h *RequestHeader) DelNoSpecialCanonical(key []byte) {
	h.h = delAllArgsBytes(h.h, key)
}

// DelBytes deletes header with the given key.
func (h *ResponseHeader) DelBytes(key []byte) {
	h.del(initHeaderK(&h.bufV, key, h.disableNormalizing))
}

func (h *ResponseHeader) del(key []byte) {
	switch string(key) {
	case HeaderContentType:
		h.contentType = h.contentType[:0]
	case HeaderContentEncoding:
		h.contentEncoding = h.contentEncoding[:0]
	case HeaderServer:
		h.server = h.server[:0]
	case HeaderSetCookie:
		h.cookies = h.cookies[:0]
	case HeaderContentLength:
		h.contentLength = -2
		h.contentLengthBytes = h.contentLengthBytes[:0]
	case HeaderConnection:
		h.connectionClose = false
	case HeaderTrailer:
		h.trailer = h.trailer[:0]
	}
	h.h = delAllArgsBytes(h.h, key)
}

// Del deletes header with the given key.
func (h *RequestHeader) Del(key string) {
	h.DelBytes(s2b(key))
}

// DelBytes deletes header with the given key.
func (h *RequestHeader) DelBytes(key []byte) {
	h.del(initHeaderK(&h.bufK, key, h.disableNormalizing))
}

func (h *RequestHeader) del(key []byte) {
	switch string(key) {
	case HeaderHost:
		h.host = h.host[:0]
	case HeaderContentType:
		h.contentType = h.contentType[:0]
	case HeaderUserAgent:
		h.userAgent = h.userAgent[:0]
	case HeaderCookie:
		h.cookies = h.cookies[:0]
	case HeaderContentLength:
		h.contentLength = 0
		h.contentLengthBytes = h.contentLengthBytes[:0]
	case HeaderConnection:
		h.connectionClose = false
	case HeaderTrailer:
		h.trailer = h.trailer[:0]
	}
	h.h = delAllArgsBytes(h.h, key)
}

// setSpecialHeader handles special headers and return true when a header is processed.
// key is formated according h.disableNormalizing setting.
// value have formated without \r and \n.
func (h *ResponseHeader) setSpecialHeader(key, value []byte, set bool) bool {
	if len(key) == 0 {
		return false
	}

	switch key[0] | 0x20 {
	case 'c':
		switch {
		case bytes.Equal(strContentType, key):
			h.SetContentTypeBytes(value)
			return true
		case bytes.Equal(strContentLength, key):
			if contentLength, err := parseContentLength(value); err == nil {
				h.contentLength = contentLength
				h.contentLengthBytes = append(h.contentLengthBytes[:0], value...)
			}
			return true
		case bytes.Equal(strContentEncoding, key):
			h.SetContentEncodingBytes(value)
			return true
		case bytes.Equal(strConnection, key):
			if bytes.Equal(strClose, value) {
				h.SetConnectionClose()
			} else {
				h.ResetConnectionClose()
				h.setNonSpecial(key, value)
			}
			return true
		}
	case 's':
		if bytes.Equal(strServer, key) {
			h.SetServerBytes(value)
			return true
		} else if bytes.Equal(strSetCookie, key) {
			if set {
				h.DelAllCookies()
			}
			var kv *argsKV
			h.cookies, kv = allocArg(h.cookies)
			kv.key = getCookieKey(kv.key, value)
			kv.value = append(kv.value[:0], value...)
			return true
		}
	case 't':
		if bytes.Equal(strTransferEncoding, key) {
			// Transfer-Encoding is managed automatically.
			return true
		} else if bytes.Equal(strTrailer, key) {
			_ = h.SetTrailerBytes(value)
			return true
		}
	case 'd':
		// TODO why we do this here?
		if bytes.Equal(strDate, key) {
			// Date is managed automatically.
			return true
		}
	}

	return false
}

func (h *ResponseHeader) SetCookieBytes(value []byte) {
	var kv *argsKV
	h.cookies, kv = allocArg(h.cookies)
	kv.key = getCookieKey(kv.key, value)
	kv.value = append(kv.value[:0], value...)
}

// SetTransferEncoding is no op
// Transfer-Encoding is managed automatically.
// This method is provided because we can't
// always remember which headers are handled automatically.
func (h *ResponseHeader) SetTransferEncoding(value []byte) {
}

// SetDate no op method. Date head is managed automatically.
// This method is provided because we can't always
// remember which headers are handled automatically.
func (h *ResponseHeader) SetDate(value []byte) {
}

// setNonSpecial directly put into map i.e. not a basic header.
func (h *ResponseHeader) setNonSpecial(key, value []byte) {
	h.h = setArgBytes(h.h, key, value, argsHasValue)
}

// setSpecialHeader handles special headers and return true when a header is processed.
// key is formated according h.disableNormalizing setting.
// value have formated without \r and \n.
func (h *RequestHeader) setSpecialHeader(key, value []byte, set bool) bool {
	if len(key) == 0 || h.disableSpecialHeader {
		return false
	}

	switch key[0] | 0x20 {
	case 'c':
		switch {
		case bytes.Equal(strContentType, key):
			h.SetContentTypeBytes(value)
			return true
		case bytes.Equal(strContentLength, key):
			if contentLength, err := parseContentLength(value); err == nil {
				h.contentLength = contentLength
				h.contentLengthBytes = append(h.contentLengthBytes[:0], value...)
			}
			return true
		case bytes.Equal(strConnection, key):
			if bytes.Equal(strClose, value) {
				h.SetConnectionClose()
			} else {
				h.ResetConnectionClose()
				h.setNonSpecial(key, value)
			}
			return true
		case bytes.Equal(strCookie, key):
			h.collectCookies()
			if set {
				h.cookies = h.cookies[:0]
			}
			h.cookies = parseRequestCookies(h.cookies, value)
			return true
		}
	case 't':
		if bytes.Equal(strTransferEncoding, key) {
			// Transfer-Encoding is managed automatically.
			return true
		} else if bytes.Equal(strTrailer, key) {
			_ = h.SetTrailerBytes(value)
			return true
		}
	case 'h':
		if bytes.Equal(strHost, key) {
			h.setHost(value)
			return true
		}
	case 'u':
		if bytes.Equal(strUserAgent, key) {
			h.SetUserAgentBytes(value)
			return true
		}
	}

	return false
}

// SetCookieBytes Directly adding a new cookie through the
// cookie string will not overwrite the already added cookies.
func (h *RequestHeader) SetCookieBytes(value []byte) {
	var kv *argsKV
	h.cookies, kv = allocArg(h.cookies)
	kv.key = getCookieKey(kv.key, value)
	kv.value = append(kv.value[:0], value...)
}

// SetTransferEncoding is no op
// Transfer-Encoding is managed automatically.
// This method is provided because we can't
// always remember which headers are handled automatically.
func (h *RequestHeader) SetTransferEncoding(value []byte) {
	return
}

func (h *RequestHeader) TransferEncoding() []byte {
	t := h.peek(strTransferEncoding)
	if len(t) == 0 {
		if h.ContentLength() == -1 {
			return strChunked
		}
	} else {
		return t
	}
	return nil
}

// SetDate no op method. Date head is managed automatically.
// This method is provided because we can't always
// remember which headers are handled automatically.
func (h *RequestHeader) SetDate(value []byte) {
	// Date is managed automatically.
}

// setNonSpecial directly put into map i.e. not a basic header.
func (h *RequestHeader) setNonSpecial(key, value []byte) {
	h.h = setArgBytes(h.h, key, value, argsHasValue)
}

// Add adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use Set for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Set-Cookie,
// Transfer-Encoding and Date headers can only be set once and will
// overwrite the previous value.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) Add(key, value string) {
	h.AddBytesKV(s2b(key), s2b(value))
}

// AddCanonical Same as Add assuming key is canonical form.
func (h *ResponseHeader) AddCanonical(key, value []byte) {
	h.addCanonical(key, initHeaderV(&h.bufV, value))
}

// AddBytesK adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesK for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Set-Cookie,
// Transfer-Encoding and Date headers can only be set once and will
// overwrite the previous value.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesK(key []byte, value string) {
	h.AddBytesKV(key, s2b(value))
}

// AddBytesV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Set-Cookie,
// Transfer-Encoding and Date headers can only be set once and will
// overwrite the previous value.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesV(key string, value []byte) {
	h.AddBytesKV(s2b(key), value)
}

// AddBytesKV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesKV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Set-Cookie,
// Transfer-Encoding and Date headers can only be set once and will
// overwrite the previous value.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesKV(key, value []byte) {
	h.addCanonical(initHeaderK(&h.bufK, key, h.disableNormalizing), initHeaderV(&h.bufV, value))
}

// addCanonical adds the given 'key: value' header assuming key is canonical form and
// value without \r \n.
func (h *ResponseHeader) addCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value, false) {
		return
	}
	h.h = appendArgBytes(h.h, key, value, argsHasValue)
}

// Set sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use Add for setting multiple header values under the same key.
func (h *ResponseHeader) Set(key, value string) {
	h.SetBytesKV(s2b(key), s2b(value))
}

// SetBytesK sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesK for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesK(key []byte, value string) {
	h.SetBytesKV(key, s2b(value))
}

// SetBytesV sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesV for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesV(key string, value []byte) {
	h.SetBytesKV(s2b(key), value)
}

// SetBytesKV sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesKV for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesKV(key, value []byte) {
	h.setCanonical(initHeaderK(&h.bufK, key, h.disableNormalizing), initHeaderV(&h.bufV, value))
}

// setCanonical sets the given 'key: value' header assuming that
// key is in canonical form and value without  `\r` and `\n`.
func (h *ResponseHeader) setCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value, true) {
		return
	}
	h.setNonSpecial(key, value)
}

// SetCanonical sets the given 'key: value' header assuming that
// key is in canonical form.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) SetCanonical(key, value []byte) {

	h.setCanonical(key, initHeaderV(&h.bufV, value))
}

// SetCookie sets the given response cookie.
//
// It is safe re-using the cookie after the function returns.
func (h *ResponseHeader) SetCookie(cookie *Cookie) {
	h.cookies = setArgBytes(h.cookies, cookie.Key(), cookie.Cookie(), argsHasValue)
}

// SetCookie sets 'key: value' cookies.
func (h *RequestHeader) SetCookie(key, value string) {
	h.collectCookies()
	h.cookies = setArg(h.cookies, key, value, argsHasValue)
}

// SetCookieBytesK sets 'key: value' cookies.
func (h *RequestHeader) SetCookieBytesK(key []byte, value string) {
	h.SetCookie(b2s(key), value)
}

// SetCookieBytesKV sets 'key: value' cookies.
func (h *RequestHeader) SetCookieBytesKV(key, value []byte) {
	h.SetCookie(b2s(key), b2s(value))
}

// DelClientCookie instructs the client to remove the given cookie.
// This doesn't work for a cookie with specific domain or path,
// you should delete it manually like:
//
//	c := AcquireCookie()
//	c.SetKey(key)
//	c.SetDomain("example.com")
//	c.SetPath("/path")
//	c.SetExpire(CookieExpireDelete)
//	h.SetCookie(c)
//	ReleaseCookie(c)
//
// Use DelCookie if you want just removing the cookie from response header.
func (h *ResponseHeader) DelClientCookie(key string) {
	h.DelCookie(key)

	c := AcquireCookie()
	c.SetKey(key)
	c.SetExpire(CookieExpireDelete)
	h.SetCookie(c)
	ReleaseCookie(c)
}

// DelClientCookieBytes instructs the client to remove the given cookie.
// This doesn't work for a cookie with specific domain or path,
// you should delete it manually like:
//
//	c := AcquireCookie()
//	c.SetKey(key)
//	c.SetDomain("example.com")
//	c.SetPath("/path")
//	c.SetExpire(CookieExpireDelete)
//	h.SetCookie(c)
//	ReleaseCookie(c)
//
// Use DelCookieBytes if you want just removing the cookie from response header.
func (h *ResponseHeader) DelClientCookieBytes(key []byte) {
	h.DelClientCookie(b2s(key))
}

// DelCookie removes cookie under the given key from response header.
//
// Note that DelCookie doesn't remove the cookie from the client.
// Use DelClientCookie instead.
func (h *ResponseHeader) DelCookie(key string) {
	h.cookies = delAllArgs(h.cookies, key)
}

// DelCookieBytes removes cookie under the given key from response header.
//
// Note that DelCookieBytes doesn't remove the cookie from the client.
// Use DelClientCookieBytes instead.
func (h *ResponseHeader) DelCookieBytes(key []byte) {
	h.DelCookie(b2s(key))
}

// DelCookie removes cookie under the given key.
func (h *RequestHeader) DelCookie(key string) {
	h.collectCookies()
	h.cookies = delAllArgs(h.cookies, key)
}

// DelCookieBytes removes cookie under the given key.
func (h *RequestHeader) DelCookieBytes(key []byte) {
	h.DelCookie(b2s(key))
}

// DelAllCookies removes all the cookies from response headers.
func (h *ResponseHeader) DelAllCookies() {
	h.cookies = h.cookies[:0]
}

// DelAllCookies removes all the cookies from request headers.
func (h *RequestHeader) DelAllCookies() {
	h.collectCookies()
	h.cookies = h.cookies[:0]
}

// Add adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use Set for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) Add(key, value string) {
	h.AddBytesKV(s2b(key), s2b(value))
}

func (h *RequestHeader) AddCanonical(key, value []byte) {
	h.AddBytesKV(key, initHeaderV(&h.bufV, value))
}

// AddBytesK adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesK for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesK(key []byte, value string) {
	h.AddBytesKV(key, s2b(value))
}

// AddBytesV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesV for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesV(key string, value []byte) {
	h.AddBytesKV(s2b(key), value)
}

// AddBytesKV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesKV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Cookie,
// Transfer-Encoding, Host and User-Agent headers can only be set once
// and will overwrite the previous value.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesKV(key, value []byte) {
	h.addCanonical(initHeaderK(&h.bufK, key, h.disableNormalizing), initHeaderV(&h.bufV, value))
}

// addCanonical adds the given 'key: value' header assuming key is canonical form and
// value without \r \n.
func (h *RequestHeader) addCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value, false) {
		return
	}
	h.h = appendArgBytes(h.h, key, value, argsHasValue)
}

// Set sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use Add for setting multiple header values under the same key.
func (h *RequestHeader) Set(key, value string) {
	h.SetBytesKV(s2b(key), s2b(value))
}

// SetBytesK sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesK for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesK(key []byte, value string) {
	h.SetBytesKV(key, s2b(value))
}

// SetBytesV sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesV for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesV(key string, value []byte) {
	h.SetBytesKV(s2b(key), value)
}

// SetBytesKV sets the given 'key: value' header.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesKV for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesKV(key, value []byte) {
	h.setCanonical(initHeaderK(&h.bufK, key, h.disableNormalizing), initHeaderV(&h.bufV, value))
}

// SetCanonical sets the given 'key: value' header assuming that
// key is in canonical form.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) SetCanonical(key, value []byte) {
	h.setCanonical(key, initHeaderV(&h.bufV, value))
}

func (h *RequestHeader) SetCanonicalKV(key, value string) {
	h.setCanonical(s2b(key), initHeaderV(&h.bufV, s2b(value)))
}

func (h *RequestHeader) SetCanonicalNoSpecial(key, value []byte) {
	h.setNonSpecial(key, value)
}
func (h *RequestHeader) SetCanonicalNoSpecialKV(key, value string) {
	h.setNonSpecial(s2b(key), s2b(value))
}
func (h *RequestHeader) SetCanonicalNoSpecialK(key string, value []byte) {
	h.setNonSpecial(s2b(key), value)
}

// SetCanonical sets the given 'key: value' header assuming that
// key is in canonical form and value without \r \n.
func (h *RequestHeader) setCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value, true) {
		return
	}
	h.setNonSpecial(key, value)
}

// Peek returns header value for the given key assuming that
// key is in canonical form.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to the returned value. Make copies instead.
func (h *ResponseHeader) Peek(key string) []byte {
	return h.PeekBytes(s2b(key))
}

// PeeKCanonical returns header value for the given key assuming that
// key is in canonical form.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeeKCanonical(key []byte) []byte {
	return h.peek(key)
}

func (h *ResponseHeader) PeekCanonicalNoSpecial(key []byte) []byte {
	return peekArgBytes(h.h, key)
}
func (h *ResponseHeader) PeekCanonicalNoSpecialK(key string) []byte {
	return peekArgBytes(h.h, s2b(key))
}
func (h *ResponseHeader) PeeKCanonicalK(key string) []byte {
	return h.peek(s2b(key))
}

// PeekBytes returns header value for the given key.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekBytes(key []byte) []byte {
	return h.peek(initHeaderK(&h.bufK, key, h.disableNormalizing))
}

// Peek returns header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) Peek(key string) []byte {
	return h.PeekBytes(s2b(key))
}

// PeekCanonical returns header value for the given key assuming that
// key is in canonical form.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekCanonical(key []byte) []byte {
	return h.peek(key)
}

func (h *RequestHeader) PeekCanonicalNoSpecial(key []byte) []byte {
	return peekArgBytes(h.h, key)
}

func (h *RequestHeader) PeekCanonicalNoSpecialStr(key string) []byte {
	return peekArgBytes(h.h, s2b(key))
}

// PeekBytes returns header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekBytes(key []byte) []byte {
	return h.peek(initHeaderK(&h.bufK, key, h.disableNormalizing))
}
func (h *ResponseHeader) peekSpecial(key []byte) (r []byte, find bool) {
	if len(key) == 0 {
		return
	}
	switch key[0] {
	case 'C':
		switch {
		case string(key) == HeaderContentType:
			r = h.ContentType()
		case string(key) == HeaderContentEncoding:
			r = h.ContentEncoding()
		case string(key) == HeaderConnection:
			if h.ConnectionClose() {
				r = strClose
			} else {
				return
			}
		case string(key) == HeaderContentLength:
			if h.contentLength > 0 {
				h.contentLengthBytes = AppendUint(h.contentLengthBytes[:0], h.contentLength)
				r = h.contentLengthBytes
			}
		default:
			return
		}
	case 'S':
		if string(key) == HeaderServer {
			r = h.Server()
		} else if string(key) == HeaderSetCookie {
			r = appendResponseCookieBytes(nil, h.cookies)
		} else {
			return
		}
	case 'T':
		if string(key) == HeaderTrailer {
			r = appendArgsKeyBytes(nil, h.trailer, strCommaSpace)
		} else if string(key) == HeaderTransferEncoding {
			if h.contentLength == -1 {
				r = strChunked
			}
		} else {
			return
		}
	default:
		return
	}
	find = true
	return
}

func (h *ResponseHeader) peek(key []byte) []byte {
	r, ok := h.peekSpecial(key)
	if ok {
		return r
	}
	return peekArgBytes(h.h, key)
}

func (h *RequestHeader) peekSpecial(key []byte) (r []byte, find bool) {
	if len(key) == 0 {
		return
	}
	switch key[0] {
	case 'H':
		if string(key) == HeaderHost {
			return h.Host(), true
		}
	case 'U':
		if string(key) == HeaderUserAgent {
			return h.UserAgent(), true
		}
	case 'C':
		switch {
		case string(key) == HeaderContentType:
			return h.ContentType(), true
		case string(key) == HeaderCookie:
			if h.cookiesCollected {
				return appendRequestCookieBytes(nil, h.cookies), true
			}
		case string(key) == HeaderConnection:
			if h.ConnectionClose() {
				return strClose, true
			}
		case string(key) == HeaderContentLength:
			return h.contentLengthBytes, true
		}
	case 'T':
		if string(key) == HeaderTrailer {
			return appendArgsKeyBytes(nil, h.trailer, strCommaSpace), true
		}
	}
	return
}

func (h *RequestHeader) peek(key []byte) []byte {
	r, ok := h.peekSpecial(key)
	if ok {
		return r
	}
	return peekArgBytes(h.h, key)
}

// PeekAll returns all header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekAll(key string) [][]byte {
	return h.PeekAllBytes(s2b(key))
}

func (h *RequestHeader) PeekAllBytes(key []byte) [][]byte {
	return h.peekAll(initHeaderK(&h.bufK, key, h.disableNormalizing))
}

// PeekAllCanonical Same as PeekAll assuming that key is in canonical form.
func (h *RequestHeader) PeekAllCanonical(key []byte) [][]byte {
	return h.peekAll(key)
}

func (h *RequestHeader) peekAll(key []byte) [][]byte {
	h.mulHeader = h.mulHeader[:0]
	r, ok := h.peekSpecial(key)
	if ok && len(r) > 0 {
		h.mulHeader = append(h.mulHeader, r)
	} else {
		h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
	}
	return h.mulHeader
}

// PeekAll returns all header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseResponse or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekAll(key string) [][]byte {
	return h.PeekAllBytes(s2b(key))
}

func (h *ResponseHeader) PeekAllBytes(key []byte) [][]byte {
	return h.peekAll(initHeaderK(&h.bufK, key, h.disableNormalizing))
}

// PeekAllCanonical Same as PeekAll assuming that key is in canonical form.
func (h *ResponseHeader) PeekAllCanonical(key []byte) [][]byte {
	return h.peekAll(key)
}

func (h *ResponseHeader) peekAll(key []byte) [][]byte {
	h.mulHeader = h.mulHeader[:0]
	r, ok := h.peekSpecial(key)
	if ok && len(r) > 0 {
		h.mulHeader = append(h.mulHeader, r)
	} else {
		h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
	}
	return h.mulHeader
}

// PeekKeys return all header keys.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	h.mulHeader = peekArgsKeys(h.mulHeader, h.h)
	return h.mulHeader
}

// PeekTrailerKeys return all trailer keys.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekTrailerKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	h.mulHeader = peekArgsKeys(h.mulHeader, h.trailer)
	return h.mulHeader
}

// PeekKeys return all header keys.
//
// The returned value is valid until the request is released,
// either though ReleaseResponse or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	h.mulHeader = peekArgsKeys(h.mulHeader, h.h)
	return h.mulHeader
}

// PeekTrailerKeys return all trailer keys.
//
// The returned value is valid until the request is released,
// either though ReleaseResponse or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekTrailerKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	h.mulHeader = peekArgsKeys(h.mulHeader, h.trailer)
	return h.mulHeader
}

// Cookie returns cookie for the given key.
func (h *RequestHeader) Cookie(key string) []byte {
	h.collectCookies()
	return peekArgStr(h.cookies, key)
}

// CookieBytes returns cookie for the given key.
func (h *RequestHeader) CookieBytes(key []byte) []byte {
	h.collectCookies()
	return peekArgBytes(h.cookies, key)
}

// Cookie fills cookie for the given cookie.Key.
//
// Returns false if cookie with the given cookie.Key is missing.
func (h *ResponseHeader) Cookie(cookie *Cookie) bool {
	v := peekArgBytes(h.cookies, cookie.Key())
	if v == nil {
		return false
	}
	//goland:noinspection GoUnhandledErrorResult
	cookie.ParseBytes(v) //nolint:errcheck
	return true
}

// Read reads response header from r.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *ResponseHeader) Read(r *bufio.Reader) (err error) {
	h.resetSkipNormalize()
	n := 1
	for {
		err = h.tryRead(r, n)
		if err == nil {
			return
		}
		if err != errNeedMore {
			return
		}
		n = r.Buffered() + 1
	}
}

// error is io.EOF only when n is 1, and nothing read, convert any error
// to io.EOF. or not when nothing read and s is io.EOF n is what not required.
func (h *ResponseHeader) tryRead(r *bufio.Reader, n int) (err error) {
	b, err := r.Peek(n)
	if n != len(b) {
		if err == io.EOF {
			err = ErrUnexpectedHeaderEOF
		}
		return
	}
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parse(b)
	if errParse != nil {
		err = errParse
		return
	}
	mustDiscard(r, headersLen)
	return
}

var ErrUnexpectedTrailerEOF = errors.New("http: unexpected EOF reading trailer")
var ErrUnexpectedHeaderEOF = errors.New("http: unexpected EOF reading header")
var ErrUnexpectedReqBodyEOF = errors.New("http: unexpected EOF reading request body")
var ErrUnexpectedRespBodyEOF = errors.New("http: unexpected EOF reading response body")

// ReadTrailer reads response trailer header from r.
//
// io.EOF is returned if r is closed before reading the first byte.
func (h *ResponseHeader) ReadTrailer(r *bufio.Reader) (err error) {
	// The common case, since nobody uses trailers.
	buf, err := r.Peek(2)
	if bytes.Equal(buf, strCRLF) {
		//goland:noinspection GoUnhandledErrorResult
		r.Discard(2)
		return
	}
	if len(buf) < 2 {
		if err == io.EOF {
			err = ErrUnexpectedTrailerEOF
		}
		return
	}
	// here err must nil.
	n := 1
	for {
		err = h.tryReadTrailer(r, n)
		if err == nil {
			return
		}
		if err != errNeedMore {
			return
		}
		n = r.Buffered() + 1
	}
}

func (h *ResponseHeader) tryReadTrailer(r *bufio.Reader, n int) (err error) {
	b, err := r.Peek(n)

	if len(b) != n {
		if err == io.EOF {
			err = ErrUnexpectedHeaderEOF
		} else if err == bufio.ErrBufferFull {
			err = HeaderBufferSmallErr
		}
		return err
	}
	//
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parseTrailer(b)
	err = errParse
	if err != nil {
		return
		/*
			if err == io.EOF {
				return err
			}
			return headerError("response", err, errParse, b, h.secureErrorLogMessage)
		*/
	}
	mustDiscard(r, headersLen)
	return nil
}

func headerError(typ string, err, errParse error, b []byte, secureErrorLogMessage bool) error {
	if errParse != errNeedMore {
		return headerErrorMsg(typ, errParse, b, secureErrorLogMessage)
	}
	if err == nil {
		return errNeedMore
	}

	// Buggy servers may leave trailing CRLFs after http body.
	// Treat this case as EOF.
	if isOnlyCRLF(b) {
		return io.EOF
	}

	if err != bufio.ErrBufferFull {
		return headerErrorMsg(typ, err, b, secureErrorLogMessage)
	}
	// errParse is  errNeedMore, s is not nil and bufio.ErrBufferFull
	return &ErrSmallBuffer{
		error: headerErrorMsg(typ, errSmallBuffer, b, secureErrorLogMessage),
	}
}

func headerErrorMsg(typ string, err error, b []byte, secureErrorLogMessage bool) error {
	if secureErrorLogMessage {
		return fmt.Errorf("error when reading %s headers: %w. Buffer size=%d", typ, err, len(b))
	}
	return fmt.Errorf("error when reading %s headers: %w. Buffer size=%d, contents: %s", typ, err, len(b), bufferSnippet(b))
}

// Read reads request header from r.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *RequestHeader) Read(r *bufio.Reader) error {
	return h.readLoop(r, true, false, false)
}

// readLoop reads request header from r optionally loops until it has enough data.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *RequestHeader) readLoop(r *bufio.Reader, waitForMore bool, prevMethodIsPost bool, dontReset bool) (err error) {
	if prevMethodIsPost {
		// RFC 7230 section 3 tolerance for old buggy clients.
		// copy net/http
		peek, _ := r.Peek(4) // tryRead will get err below
		//goland:noinspection GoUnhandledErrorResult
		r.Discard(numLeadingCRorLF(peek))
	}
	// reset RequestHeader before reading header
	if !dontReset {
		h.resetSkipNormalize()
	}
	n := 1
	for {
		err = h.tryRead(r, n)
		if err == nil {
			return
		}
		if !waitForMore || err != errNeedMore {
			// if s is bufio.ErrBufferFull, parse again is useless.
			if err == bufio.ErrBufferFull {
				err = HeaderBufferSmallErr
			}
			// why reset here?
			//h.resetSkipNormalize()
			return
		}
		n = r.Buffered() + 1
	}
}

// ReadTrailer reads request trailer header from r.
//
// io.EOF is returned if r is closed before reading the first byte.
func (h *RequestHeader) ReadTrailer(r *bufio.Reader) (err error) {
	// The common case, since nobody uses trailers.
	buf, err := r.Peek(2)
	if bytes.Equal(buf, strCRLF) {
		//goland:noinspection GoUnhandledErrorResult
		r.Discard(2)
		return
	}
	if len(buf) < 2 {
		if err == io.EOF {
			err = ErrUnexpectedTrailerEOF
		}
		return
	}
	n := 1
	for {
		err = h.tryReadTrailer(r, n)
		if err == nil {
			return
		}
		if err != errNeedMore {
			return
		}
		n = r.Buffered() + 1
	}
}

func (h *RequestHeader) tryReadTrailer(r *bufio.Reader, n int) (err error) {
	b, err := r.Peek(n)
	if len(b) != n {
		if err == io.EOF {
			err = ErrUnexpectedTrailerEOF
		} else if err == bufio.ErrBufferFull {
			err = TrailerHeaderBufferSmallErr
		}
		return
	}
	// err here must nil.
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parseTrailer(b)
	err = errParse
	if err != nil {
		return
	}
	mustDiscard(r, headersLen)
	return
}

func (h *RequestHeader) tryRead(r *bufio.Reader, n int) (err error) {
	b, err := r.Peek(n)
	if n != len(b) {
		if err == io.EOF {
			err = ErrUnexpectedHeaderEOF
		}
		return
	}
	// here n must equal len(b), so err must nil.
	// top had filter n==0, here b's length at least 1.
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parse(b)
	if errParse != nil {
		err = errParse
		return
	}
	// discard had read part. not necessary full b.
	mustDiscard(r, headersLen)
	return
}

func bufferSnippet(b []byte) string {
	n := len(b)
	start := 200
	end := n - start
	if start >= end {
		start = n
		end = n
	}
	bStart, bEnd := b[:start], b[end:]
	if len(bEnd) == 0 {
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("%q...%q", bStart, bEnd)
}

func isOnlyCRLF(b []byte) bool {
	for _, ch := range b {
		if ch != rChar && ch != nChar {
			return false
		}
	}
	return true
}

func updateServerDate() {
	refreshServerDate()
	go func() {
		for {
			time.Sleep(time.Second)
			refreshServerDate()
		}
	}()
}

var (
	serverDate     atomic.Pointer[[]byte]
	serverDateOnce sync.Once // serverDateOnce.Do(updateServerDate)
)

func refreshServerDate() {
	b := AppendHTTPDate(nil, time.Now())
	serverDate.Store(&b)
}

// Write writes response header to w.
func (h *ResponseHeader) Write(w *bufio.Writer) error {
	_, err := w.Write(h.Header())
	return err
}

// WriteCompress writes response header to w.
// Neither the Content-Length header nor the delimiter
// between the headers and the response body will be written.
func (h *ResponseHeader) WriteCompress(w *bufio.Writer) error {
	_, err := w.Write(h.Header(true))
	return err
}

// WriteTo writes response header to w.
//
// WriteTo implements io.WriterTo interface.
func (h *ResponseHeader) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(h.Header())
	return int64(n), err
}

// Header returns response header representation.
//
// Headers that set as Trailer will not represent. Use TrailerHeader for trailers.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
//
// When the `compress` parameter is passed, neither the `Content-Length` header
// nor the delimiter between the headers and the response body will be written.
func (h *ResponseHeader) Header(compressed ...bool) []byte {
	h.bufV = h.AppendBytes(h.bufV[:0], compressed...)
	return h.bufV
}

// writeTrailer writes response trailer to w.
func (h *ResponseHeader) writeTrailer(w *bufio.Writer) error {
	_, err := w.Write(h.TrailerHeader())
	return err
}

// TrailerHeader returns response trailer header representation.
//
// Trailers will only be received with chunked transfer.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) TrailerHeader() []byte {
	h.bufV = h.bufV[:0]
	for _, t := range h.trailer {
		value := h.peek(t.key)
		h.bufV = appendHeaderLine(h.bufV, t.key, value)
	}
	h.bufV = append(h.bufV, strCRLF...)
	return h.bufV
}

// String returns response header representation.
func (h *ResponseHeader) String() string {
	return string(h.Header())
}

// appendStatusLine appends the response status line to dst and returns
// the extended dst.
func (h *ResponseHeader) appendStatusLine(dst []byte) []byte {
	statusCode := h.StatusCode()
	if statusCode <= 0 {
		statusCode = StatusOK
	}
	return formatStatusLine(dst, h.Protocol(), statusCode, h.StatusMessage())
}

var (
	suppressedHeaders304    = []string{"Content-Type", "Content-Length", "Transfer-Encoding"}
	suppressedHeadersNoBody = []string{"Content-Length", "Transfer-Encoding"}
	excludedHeadersNoBody   = map[string]bool{"Content-Length": true, "Transfer-Encoding": true}
)

func suppressedHeaders(status int) []string {
	switch {
	case status == 304:
		// RFC 7232 section 4.1
		return suppressedHeaders304
	case bodyNotAllowedForStatus(status):
		return suppressedHeadersNoBody
	}
	return nil
}

// AppendBytes appends response header representation to dst and returns
// the extended dst.
func (h *ResponseHeader) AppendBytes(dst []byte, compressed ...bool) []byte {
	statusCode := h.StatusCode()
	cL := h.ContentLength()
	trailer := h.trailer[:0]
	dst = h.appendStatusLine(dst[:0])
	h.DelNoSpecialCanonical(strTransferEncoding)
	// TODO check Server head first.
	server := h.Server()
	if len(server) != 0 {
		dst = appendHeaderLine(dst, strServer, server)
	}

	// TODO must set by server???
	if !h.noDefaultDate {
		// TODO move to server start.
		serverDateOnce.Do(updateServerDate)
		dst = appendHeaderLine(dst, strDate, *serverDate.Load())
	}

	if !bodyNotAllowedForStatus(statusCode) {
		ct := h.ContentType()
		if len(ct) > 0 {
			if len(h.contentType) > 0 || cL != 0 {
				dst = appendHeaderLine(dst, strContentType, ct)
			}
		}
		contentEncoding := h.ContentEncoding()
		if len(contentEncoding) > 0 {
			dst = appendHeaderLine(dst, strContentEncoding, contentEncoding)
		}
		// Head method only send content-length when length>0,
		// same as net/http.
		if cL > 0 {
			if len(compressed) == 0 {
				h.contentLengthBytes = AppendUint(h.contentLengthBytes[:0], cL)
				dst = appendHeaderLine(dst, strContentLength, h.contentLengthBytes)
			}
		} else {
			if !h.headMethod {
				if cL == 0 {
					dst = appendHeaderLine(dst, strContentLength, []byte{'0'})
				} else if cL == -1 {
					dst = appendHeaderLine(dst, strTransferEncoding, strChunked)
					trailer = h.trailer
				} else {
					h.SetConnectionClose()
				}
			}
		}
	}
	if len(trailer) == 0 {
		for i, n := 0, len(h.h); i < n; i++ {
			dst = appendHeaderLine(dst, h.h[i].key, h.h[i].value)
		}
	} else {
		for i, n := 0, len(h.h); i < n; i++ {
			kv := &h.h[i]
			// Exclude trailer from header
			exclude := false
			for _, t := range trailer {
				if bytes.Equal(kv.key, t.key) {
					exclude = true
					break
				}
			}
			// TODO kv.key is not perhaps strDate Because it is a special key.
			if !exclude {
				dst = appendHeaderLine(dst, kv.key, kv.value)
			}
		}
		dst = appendHeaderLine(dst, strTrailer, appendArgsKeyBytes(nil, trailer, strCommaSpace))
	}

	n := len(h.cookies)
	if n > 0 {
		for i := 0; i < n; i++ {
			kv := &h.cookies[i]
			dst = appendHeaderLine(dst, strSetCookie, kv.value)
		}
	}

	if h.ConnectionClose() {
		dst = appendHeaderLine(dst, strConnection, strClose)
	}
	if len(compressed) == 0 {
		return append(dst, strCRLF...)
	}
	return dst

}

// Write writes request header to w.
func (h *RequestHeader) Write(w *bufio.Writer) error {
	_, err := w.Write(h.Header())
	return err
}

// WriteTo writes request header to w.
//
// WriteTo implements io.WriterTo interface.
func (h *RequestHeader) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(h.Header())
	return int64(n), err
}

// Header returns request header representation.
//
// Headers that set as Trailer will not represent. Use TrailerHeader for trailers.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) Header() []byte {
	h.bufV = h.AppendBytes(h.bufV[:0])
	return h.bufV
}

// writeTrailer writes request trailer to w.
func (h *RequestHeader) writeTrailer(w *bufio.Writer) error {
	_, err := w.Write(h.TrailerHeader())
	return err
}

// TrailerHeader returns request trailer header representation.
//
// Trailers will only be received with chunked transfer.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) TrailerHeader() []byte {
	h.bufV = h.bufV[:0]
	for _, t := range h.trailer {
		value := h.peek(t.key)
		h.bufV = appendHeaderLine(h.bufV, t.key, value)
	}
	h.bufV = append(h.bufV, strCRLF...)
	return h.bufV
}

// RawHeaders returns raw header key/value bytes.
//
// Depending on server configuration, header keys may be normalized to
// capital-case in place.
//
// This copy is set aside during parsing, so empty slice is returned for all
// cases where parsing did not happen. Similarly, request line is not stored
// during parsing and can not be returned.
//
// The slice is not safe to use after the handler returns.
func (h *RequestHeader) RawHeaders() []byte {
	return h.rawHeaders
}

// String returns request header representation.
func (h *RequestHeader) String() string {
	return string(h.Header())
}

// AppendBytes appends request header representation to dst and returns
// the extended dst.
func (h *RequestHeader) AppendBytes(dst []byte) []byte {
	dst = append(dst, h.Method()...)
	dst = append(dst, ' ')
	dst = append(dst, h.RequestURI()...)
	dst = append(dst, ' ')
	dst = append(dst, h.Protocol()...)
	dst = append(dst, strCRLF...)

	userAgent := h.UserAgent()
	if len(userAgent) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strUserAgent, userAgent)
	}

	host := h.Host()
	if len(host) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strHost, host)
	}

	ct := h.ContentType()
	if !h.noDefaultContentType && len(ct) == 0 && !h.ignoreBody() {
		ct = strDefaultContentType
	}
	if len(ct) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strContentType, ct)
	}
	if len(h.contentLengthBytes) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strContentLength, h.contentLengthBytes)
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		// Exclude trailer from header
		exclude := false
		for _, t := range h.trailer {
			if bytes.Equal(kv.key, t.key) {
				exclude = true
				break
			}
		}
		if !exclude {
			dst = appendHeaderLine(dst, kv.key, kv.value)
		}
	}

	if len(h.trailer) > 0 {
		dst = appendHeaderLine(dst, strTrailer, appendArgsKeyBytes(nil, h.trailer, strCommaSpace))
	}

	// there is no need in h.collectCookies() here, since if cookies aren't collected yet,
	// they all are located in h.h.
	n := len(h.cookies)
	if n > 0 && !h.disableSpecialHeader {
		dst = append(dst, strCookie...)
		dst = append(dst, strColonSpace...)
		dst = appendRequestCookieBytes(dst, h.cookies)
		dst = append(dst, strCRLF...)
	}

	if h.ConnectionClose() && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strConnection, strClose)
	}

	return append(dst, strCRLF...)
}

func appendHeaderLine(dst, key, value []byte) []byte {
	dst = append(dst, key...)
	dst = append(dst, strColonSpace...)
	dst = append(dst, value...)
	return append(dst, strCRLF...)
}

func (h *ResponseHeader) parse(buf []byte) (n int, err error) {
	m, err := h.parseFirstLine(buf)
	if err != nil {
		return
	}
	n1, err := h.parseHeaders(buf[m:])
	if err != nil {
		return
	}
	n = m + n1
	return
}

func (h *ResponseHeader) parseTrailer(buf []byte) (int, error) {
	// Skip any 0 length chunk.
	if buf[0] == '0' {
		skip := len(strCRLF) + 1
		if len(buf) < skip {
			return 0, io.EOF
		}
		buf = buf[skip:]
	}

	var s headerScanner
	s.b = buf
	s.disableNormalizing = h.disableNormalizing
	var err error
	for s.next() {
		if len(s.key) > 0 {
			if bytes.IndexByte(s.key, ' ') != -1 || bytes.IndexByte(s.key, '\t') != -1 {
				err = fmt.Errorf("invalid trailer key %q", s.key)
				continue
			}
			// Forbidden by RFC 7230, section 4.1.2
			if isBadTrailer(s.key, false) {
				err = fmt.Errorf("forbidden trailer key %q", s.key)
				continue
			}
			h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
		}
	}
	if s.err != nil {
		return 0, s.err
	}
	if err != nil {
		return 0, err
	}
	return s.hLen, nil
}

func (h *RequestHeader) ignoreBody() bool {
	return h.IsGet() || h.IsHead()
}

func (h *RequestHeader) parse(buf []byte) (n int, err error) {
	m := int(h.firstLineLen)
	if m == 0 {
		// Avoid parsing the first line of the request header repeatedly.
		m, err = h.parseFirstLine(buf)
		if err != nil {
			return
		}
	}

	// here h has protocol method requestUri
	h.rawHeaders, _, err = readRawHeaders(h.rawHeaders[:0], buf[m:])
	if err != nil {
		return
	}
	n1, err := h.parseHeaders(buf[m:])
	if err != nil {
		return
	}
	n = m + n1
	return
}

func (h *RequestHeader) parseTrailer(buf []byte) (n int, err error) {
	var s headerScanner
	s.b = buf
	s.disableNormalizing = h.disableNormalizing
	for s.next() {
		if len(s.key) > 0 {
			if bytes.IndexByte(s.key, ' ') != -1 || bytes.IndexByte(s.key, '\t') != -1 {
				err = errors.New(`invalid trailer key "` + b2s(s.key) + `"`)
				// obey net/http.
				return
			}
			// Forbidden by RFC 7230, section 4.1.2
			// obey net/http only check in Trailer head.
			h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
		}
	}
	err = s.err
	if err != nil {
		return
	}
	n = s.hLen
	return
}

func isBadTrailer(key []byte, disableNormalize bool) bool {
	if len(key) == 0 {
		return true
	}

	switch key[0] | 0x20 {
	case 'a':
		return headNameCompare(key, strAuthorization, disableNormalize)
	case 'c':
		if len(key) > len(HeaderContentType) && headNameCompare(key[:8], strContentType[:8], disableNormalize) {
			// skip compare prefix 'Content-'
			return headNameCompare(key[8:], strContentEncoding[8:], disableNormalize) ||
				headNameCompare(key[8:], strContentLength[8:], disableNormalize) ||
				headNameCompare(key[8:], strContentType[8:], disableNormalize) ||
				headNameCompare(key[8:], strContentRange[8:], disableNormalize)
		}
		return headNameCompare(key, strConnection, disableNormalize)
	case 'e':
		return headNameCompare(key, strExpect, disableNormalize)
	case 'h':
		return headNameCompare(key, strHost, disableNormalize)
	case 'k':
		return headNameCompare(key, strKeepAlive, disableNormalize)
	case 'm':
		return headNameCompare(key, strMaxForwards, disableNormalize)
	case 'p':
		if len(key) > len(HeaderProxyConnection) && headNameCompare(key[:6], strProxyConnection[:6], disableNormalize) {
			// skip compare prefix 'Proxy-'
			return headNameCompare(key[6:], strProxyConnection[6:], disableNormalize) ||
				headNameCompare(key[6:], strProxyAuthenticate[6:], disableNormalize) ||
				headNameCompare(key[6:], strProxyAuthorization[6:], disableNormalize)
		}
	case 'r':
		return headNameCompare(key, strRange, disableNormalize)
	case 't':
		return headNameCompare(key, strTE, disableNormalize) ||
			headNameCompare(key, strTrailer, disableNormalize) ||
			headNameCompare(key, strTransferEncoding, disableNormalize)
	case 'w':
		return headNameCompare(key, strWWWAuthenticate, disableNormalize)
	}
	return false
}

// ErrMalformedResponse is returned by Dialer to indicate that server response
// can not be parsed.
var ErrMalformedResponse = fmt.Errorf("malformed HTTP response")

func (h *ResponseHeader) parseFirstLine(buf []byte) (n int, err error) {
	bNext := buf
	var b []byte
	b, bNext, err = nextLine(bNext)
	if err != nil {
		return
	}
	// parse protocol
	n1 := bytes.IndexByte(b, ' ')
	if n1 <= 0 {
		err = ErrMalformedResponse
		// cannot find whitespace in the first line of response
		return
	}
	h.noHTTP11 = !bytes.Equal(b[:n1], strHTTP11)
	b = b[n1+1:]

	// parse status code
	statusCode, n1, err := parseUintBuf(b)
	h.statusCode = int(statusCode)
	if err != nil {
		// cannot parse response status code
		err = ErrMalformedResponse
		return
	}
	if len(b) > n1 && b[n1] != ' ' {
		// unexpected char at the end of status code
		err = ErrMalformedResponse
		return
	}
	if len(b) > n1+1 {
		h.SetStatusMessage(b[n1+1:])
	}
	n = len(buf) - len(bNext)
	return
}

func isValidMethod(method []byte) bool {
	for _, ch := range method {
		if validMethodValueByteTable[ch] == 0 {
			return false
		}
	}
	return true
}
func (h *RequestHeader) resetFirstLine() {
	h.method = h.method[:0]
	h.requestURI = h.requestURI[:0]
	h.proto = h.proto[:0]
	h.noHTTP11 = false
}

func numLeadingCRorLF(v []byte) (n int) {
	for _, b := range v {
		if b == '\r' || b == '\n' {
			n++
			continue
		}
		break
	}
	return
}

func (h *RequestHeader) parseFirstLine(buf []byte) (n int, err error) {
	bNext := buf
	var (
		b         []byte
		firstLine string
	)
	// obey net/http
	if b, bNext, err = nextLine(bNext); err != nil {
		return
	}
	firstLine = b2s(b)

	// parse method
	n1 := bytes.IndexByte(b, ' ')
	if n1 <= 0 {
		err = newHeaderParseErr("request method not found", firstLine)
		return
	}
	method := append(h.method[:0], b[:n1]...)

	if !isValidMethod(h.method) {
		err = newHeaderParseErr("invalid method", b2s(method))
		return
	}

	b = b[n1+1:]

	// parse requestURI
	n1 = bytes.LastIndexByte(b, ' ')
	if n1 <= 0 {
		err = newHeaderParseErr("request uri not found", firstLine)
		return
	}

	protoStr := b[n1+1:]

	// Follow RFCs 7230 and 9112 and require that HTTP versions match the following pattern: HTTP/[0-9]\.[0-9]
	if len(protoStr) != len(strHTTP11) {
		err = newHeaderParseErr("unsupported HTTP version", b2s(protoStr))
		return
	}
	// only support http/1.0 http/1.1
	if !bytes.HasPrefix(protoStr, strHTTP11[:5]) {
		err = newHeaderParseErr("unsupported HTTP version", b2s(protoStr))
		return
	}
	if protoStr[5] < '0' || protoStr[5] > '9' || protoStr[7] < '0' || protoStr[7] > '9' {
		err = newHeaderParseErr("unsupported HTTP version", b2s(protoStr))
		return
	}

	h.method = method
	h.noHTTP11 = !bytes.Equal(protoStr, strHTTP11)
	h.proto = append(h.proto[:0], protoStr...)
	h.requestURI = append(h.requestURI[:0], b[:n1]...)
	h.firstLineLen = int32(len(buf) - len(bNext))
	n = int(h.firstLineLen)
	return
}

// Can recognize \n or \r\n split head lines.
// Return []byte include head and body split \r\n or \n.
func readRawHeaders(dst, buf []byte) ([]byte, int, error) {
	n := bytes.IndexByte(buf, nChar)
	if n < 0 {
		return dst[:0], 0, errNeedMore
	}
	if (n == 1 && buf[0] == rChar) || n == 0 {
		// empty headers
		return dst, n + 1, nil
	}

	n++
	b := buf
	m := n
	for {
		b = b[m:]
		m = bytes.IndexByte(b, nChar)
		if m < 0 {
			return dst, 0, errNeedMore
		}
		m++
		n += m
		if (m == 2 && b[0] == rChar) || m == 1 {
			dst = append(dst, buf[:n]...)
			return dst, n, nil
		}
	}
}

func isUnsupportedTEError(err error) bool {
	//goland:noinspection GoTypeAssertionOnErrors
	_, ok := err.(*unsupportedTEError)
	return ok
}

// unsupportedTEError reports unsupported transfer-encodings.
type unsupportedTEError struct {
	s string
}

func (u *unsupportedTEError) Error() string {
	return u.s
}

func (h *ResponseHeader) parseHeaders(buf []byte) (n int, err error) {
	h.contentLength = -2
	if h.headMethod {
		h.contentLength = 0
	}

	var (
		s  headerScanner
		kv *argsKV
	)
	s.b = buf
	s.disableNormalizing = h.disableNormalizing

	for s.next() {
		if len(s.key) == 0 {
			err = EmptyHeaderKeyErr
			return
		}
		for _, ch := range s.key {
			if !validHeaderFieldByte(ch) {
				err = newHeaderParseErr("invalid header key", b2s(s.key))
				return
			}
		}
		for _, ch := range s.value {
			if !validHeaderValueByte(ch) {
				err = newHeaderParseErr("invalid header value", b2s(s.value))
				return
			}
		}

		switch s.key[0] | 0x20 {
		case 'c':
			if headNameCompare(s.key, strContentType, h.disableNormalizing) {
				h.contentType = append(h.contentType[:0], s.value...)
				continue
			}
			if headNameCompare(s.key, strContentEncoding, h.disableNormalizing) {
				h.contentEncoding = append(h.contentEncoding[:0], s.value...)
				continue
			}
			if headNameCompare(s.key, strContentLength, h.disableNormalizing) {
				if h.contentLength != -1 {
					h.contentLength, err = parseContentLength(s.value)
					if err != nil {
						err = ContentLengthNotIntErr
						//err = newHeaderParseErr("parse Content-Length", err.Error())
						return
					}
					h.contentLengthBytes = append(h.contentLengthBytes[:0], s.value...)
				}
				continue
			}
			if headNameCompare(s.key, strConnection, h.disableNormalizing) {
				if bytes.Equal(s.value, strClose) {
					h.connectionClose = true
				} else {
					h.connectionClose = false
					h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
				}
				continue
			}
		case 's':
			if headNameCompare(s.key, strServer, h.disableNormalizing) {
				h.server = append(h.server[:0], s.value...)
				continue
			}
			if headNameCompare(s.key, strSetCookie, h.disableNormalizing) {
				h.cookies, kv = allocArg(h.cookies)
				kv.key = getCookieKey(kv.key, s.value)
				kv.value = append(kv.value[:0], s.value...)
				continue
			}
		case 't':
			if headNameCompare(s.key, strTransferEncoding, h.disableNormalizing) {
				if len(s.value) > 0 && bytes.Equal(s.value, strChunked) {
					h.contentLength = -1
					continue
				}
				err = &unsupportedTEError{"unsupported transfer encoding: " + b2s(s.value)}
				return
			}
			if headNameCompare(s.key, strTrailer, h.disableNormalizing) {
				err = h.SetTrailerBytes(s.value)
				if err != nil {
					err = BadTrailerInHeader
					return
				}
				continue
			}
		}
		h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
	}

	if s.err != nil {
		err = s.err
		return
	}

	if h.contentLength < 0 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
	}
	if h.noHTTP11 && !h.connectionClose {
		// close connection for non-http/1.1 response unless 'Connection: keep-alive' is set.
		v := peekArgBytes(h.h, strConnection)
		h.connectionClose = !hasHeaderValue(v, strKeepAlive)
	}

	n = len(buf) - len(s.b)
	return
}

func (h *RequestHeader) parseHeaders(buf []byte) (n int, err error) {
	// RFC 7230 neither explicitly permits nor forbids an
	// entity-body on a GET request so we permit one if
	// declared, but we default to 0 here (not -1 below)
	// if there's no mention of a body.
	// Likewise, all other request methods are assumed to have
	// no body if neither Transfer-Encoding chunked nor a
	// Content-Length are set.
	// See https://github.com/golang/go/blob/9e8ea567c838574a0f14538c0bbbd83c3215aa55/src/net/http/transfer.go#L729
	//
	// If the request header does not contain `Content-Length` or `Transfer-Encoding`,
	// the default value is 0.
	h.contentLength = 0
	h.contentLengthSeen = false

	var s headerScanner
	s.b = buf
	s.disableNormalizing = h.disableNormalizing

	for s.next() {
		if len(s.key) == 0 {
			err = EmptyHeaderKeyErr
			return
		}

		for _, ch := range s.key {
			if !validHeaderFieldByte(ch) {
				err = newHeaderParseErr("invalid header key", b2s(s.key))
				return
			}
		}
		for _, ch := range s.value {
			if !validHeaderValueByte(ch) {
				err = newHeaderParseErr("invalid header value", b2s(s.value))
				return
			}
		}

		if h.disableSpecialHeader {
			h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
			continue
		}

		switch s.key[0] | 0x20 {
		case 'h':
			if headNameCompare(s.key, strHost, s.disableNormalizing) {
				h.host = append(h.host[:0], s.value...)
				continue
			}
		case 'u':
			if headNameCompare(s.key, strUserAgent, s.disableNormalizing) {
				h.userAgent = append(h.userAgent[:0], s.value...)
				continue
			}
		case 'c':
			if headNameCompare(s.key, strContentType, s.disableNormalizing) {
				h.contentType = append(h.contentType[:0], s.value...)
				continue
			}
			if headNameCompare(s.key, strContentLength, s.disableNormalizing) {
				if h.contentLengthSeen {
					err = MultipleContentLengthErr
					return
				}
				h.contentLengthSeen = true

				if h.contentLength != -1 {
					h.contentLength, err = parseContentLength(s.value)
					if err != nil {
						err = ContentLengthNotIntErr
						return
					}
					h.contentLengthBytes = append(h.contentLengthBytes[:0], s.value...)
				}
				continue
			}
			if headNameCompare(s.key, strConnection, s.disableNormalizing) {
				if bytes.Equal(s.value, strClose) {
					h.connectionClose = true
				} else {
					h.connectionClose = false
					h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
				}
				continue
			}
		case 't':
			if headNameCompare(s.key, strTransferEncoding, s.disableNormalizing) {
				//isIdentity := headNameCompare(s.value, strIdentity, h.disableSpecialHeader)
				isChunked := headNameCompare(s.value, strChunked, h.disableSpecialHeader)
				if !isChunked {
					err = &unsupportedTEError{s: "unsupported Transfer-Encoding: " + b2s(s.value)}
					return
				}
				h.contentLength = -1
				h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
				continue
			}
			if headNameCompare(s.key, strTrailer, s.disableNormalizing) {
				err = h.SetTrailerBytes(s.value)
				if err != nil {
					return
				}
				continue
			}
		}
		h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
	}

	if s.err != nil {
		err = s.err
		return
	}

	if h.contentLength < 0 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
	}
	if h.noHTTP11 && !h.connectionClose {
		// close connection for non-http/1.1 request unless 'Connection: keep-alive' is set.
		v := peekArgBytes(h.h, strConnection)
		h.connectionClose = !hasHeaderValue(v, strKeepAlive)
	}
	n = s.hLen
	return
}

func (h *RequestHeader) collectCookies() {
	if h.cookiesCollected {
		return
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		if caseInsensitiveCompare(kv.key, strCookie) {
			h.cookies = parseRequestCookies(h.cookies, kv.value)
			tmp := *kv
			copy(h.h[i:], h.h[i+1:])
			n--
			i--
			h.h[n] = tmp
			h.h = h.h[:n]
		}
	}
	h.cookiesCollected = true
}

var errNonNumericChars = errors.New("non-numeric chars found")

func parseContentLength(b []byte) (int64, error) {
	v, n, err := parseUintBuf(b)
	if err != nil {
		return -1, fmt.Errorf("cannot parse Content-Length: %w", err)
	}
	if n != len(b) {
		return -1, fmt.Errorf("cannot parse Content-Length: %w", errNonNumericChars)
	}
	return v, nil
}

type headerScanner struct {
	err error

	b     []byte
	key   []byte
	value []byte

	// hLen stores header subslice len
	hLen int

	// by checking whether the next line contains a colon or not to tell
	// it's a header entry or a multi line value of current header entry.
	// the side effect of this operation is that we know the index of the
	// next colon and new line, so this can be used during next iteration,
	// instead of find them again.
	nextColon   int
	nextNewLine int

	disableNormalizing bool
	initialized        bool
}

func (s *headerScanner) next() bool {
	if !s.initialized {
		s.nextColon = -1
		s.nextNewLine = -1
		s.initialized = true
	}
	bLen := len(s.b)
	if bLen >= 2 && s.b[0] == rChar && s.b[1] == nChar {
		s.b = s.b[2:]
		s.hLen += 2
		return false
	}
	if bLen >= 1 && s.b[0] == nChar {
		s.b = s.b[1:]
		s.hLen++
		return false
	}
	var n int
	if s.nextColon >= 0 {
		n = s.nextColon
		s.nextColon = -1
	} else {
		n = bytes.IndexByte(s.b, ':')

		// There can't be a \n inside the header name, check for this.
		x := bytes.IndexByte(s.b, nChar)
		if x < 0 {
			// A header name should always at some point be followed by a \n
			// even if it's the one that terminates the header block.
			s.err = errNeedMore
			return false
		}
		if x < n {
			// There was a \n before the :
			s.err = errInvalidName
			return false
		}
	}
	if n < 0 {
		s.err = errNeedMore
		return false
	}
	s.key = s.b[:n]
	if !s.disableNormalizing {
		normalizeHeaderKeyV2(s.key)
	}
	n++
	for len(s.b) > n && (s.b[n] == ' ' || s.b[n] == '\t') {
		n++
		// the newline index is a relative index, and lines below trimmed `s.b` by `n`,
		// so the relative newline index also shifted forward. it's safe to decrease
		// to a minus value, it means it's invalid, and will find the newline again.
		s.nextNewLine--
	}
	s.hLen += n
	s.b = s.b[n:]
	if s.nextNewLine >= 0 {
		n = s.nextNewLine
		s.nextNewLine = -1
	} else {
		n = bytes.IndexByte(s.b, nChar)
	}
	if n < 0 {
		s.err = errNeedMore
		return false
	}
	isMultiLineValue := false
	for {
		if n+1 >= len(s.b) {
			break
		}
		if s.b[n+1] != ' ' && s.b[n+1] != '\t' {
			break
		}
		d := bytes.IndexByte(s.b[n+1:], nChar)
		if d <= 0 {
			break
		} else if d == 1 && s.b[n+1] == rChar {
			break
		}
		e := n + d + 1
		if c := bytes.IndexByte(s.b[n+1:e], ':'); c >= 0 {
			s.nextColon = c
			s.nextNewLine = d - c - 1
			break
		}
		isMultiLineValue = true
		n = e
	}
	if n >= len(s.b) {
		s.err = errNeedMore
		return false
	}
	oldB := s.b
	s.value = s.b[:n]
	s.hLen += n + 1
	s.b = s.b[n+1:]

	if n > 0 && s.value[n-1] == rChar {
		n--
	}
	for n > 0 && (s.value[n-1] == ' ' || s.value[n-1] == '\t') {
		n--
	}
	s.value = s.value[:n]
	if isMultiLineValue {
		s.value, s.b, s.hLen = normalizeHeaderValue(s.value, oldB, s.hLen)
	}

	return true
}

type headerValueScanner struct {
	b     []byte
	value []byte
}

func (s *headerValueScanner) next() bool {
	b := s.b
	if len(b) == 0 {
		return false
	}
	n := bytes.IndexByte(b, ',')
	if n < 0 {
		s.value = stripSpace(b)
		s.b = b[len(b):]
		return true
	}
	s.value = stripSpace(b[:n])
	s.b = b[n+1:]
	return true
}

func stripSpace(b []byte) []byte {
	for len(b) > 0 && b[0] == ' ' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func hasHeaderValue(s, value []byte) bool {
	var vs headerValueScanner
	vs.b = s
	for vs.next() {
		if caseInsensitiveCompare(vs.value, value) {
			return true
		}
	}
	return false
}
func nextLine(b []byte) ([]byte, []byte, error) {
	nNext := bytes.IndexByte(b, nChar)
	if nNext < 0 {
		return nil, nil, errNeedMore
	}
	n := nNext
	if n > 0 && b[n-1] == rChar {
		n--
	}
	return b[:n], b[nNext+1:], nil
}

// https://tools.ietf.org/html/rfc7230#section-3.2.4
func initHeaderV(bufV *[]byte, raw []byte) []byte {
	// check if a `\r` is present and save the position.
	// if no `\r` is found, check if a `\n` is present.
	foundR := bytes.IndexByte(raw, rChar)
	foundN := bytes.IndexByte(raw, nChar)
	start := 0

	switch {
	case foundN != -1:
		if foundR > foundN {
			start = foundN
		} else if foundR != -1 {
			start = foundR
		}
	case foundR != -1:
		start = foundR
	default:
		return raw
	}
	*bufV = append((*bufV)[:0], raw...)
	raw = *bufV
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case rChar, nChar:
			raw[i] = ' '
		default:
			continue
		}
	}
	return raw
}
func initHeaderK(bufK *[]byte, key []byte, disableNormalizing bool) []byte {
	if disableNormalizing {
		return key
	}
	*bufK = append((*bufK)[:0], key...)
	// slow path, above can inline.
	return normalizeHeaderKeyV2(*bufK)
}
func normalizeHeaderKeyV2(b []byte) []byte {
	n := len(b)
	if n == 0 {
		return b
	}

	b[0] = toUpperTable[b[0]]
	for i := 1; i < n; i++ {
		p := &b[i]
		if *p == '-' {
			i++
			if i < n {
				b[i] = toUpperTable[b[i]]
			}
			continue
		}
		*p = toLowerTable[*p]
	}
	return b
}

func normalizeHeaderValue(ov, ob []byte, headerLength int) (nv, nb []byte, nhl int) {
	nv = ov
	length := len(ov)
	if length <= 0 {
		return
	}
	write := 0
	shrunk := 0
	once := false
	lineStart := false
	for read := 0; read < length; read++ {
		c := ov[read]
		switch {
		case c == rChar || c == nChar:
			shrunk++
			if c == nChar {
				lineStart = true
				once = false
			}
			continue
		case lineStart && (c == '\t' || c == ' '):
			if !once {
				c = ' '
				once = true
			} else {
				shrunk++
				continue
			}
		default:
			lineStart = false
		}
		nv[write] = c
		write++
	}

	nv = nv[:write]
	copy(ob[write:], ob[write+shrunk:])

	// Check if we need to skip \r\n or just \n
	skip := 0
	if ob[write] == rChar {
		if ob[write+1] == nChar {
			skip += 2
		} else {
			skip++
		}
	} else if ob[write] == nChar {
		skip++
	}

	nb = ob[write+skip : len(ob)-shrunk]
	nhl = headerLength - shrunk
	return
}

// removeNewLines will replace `\r` and `\n` with an empty space.
func removeNewLinesV2(kv *argsKV, raw []byte) []byte {
	// check if a `\r` is present and save the position.
	// if no `\r` is found, check if a `\n` is present.
	foundR := bytes.IndexByte(raw, rChar)
	foundN := bytes.IndexByte(raw, nChar)
	start := 0

	switch {
	case foundN != -1:
		if foundR > foundN {
			start = foundN
		} else if foundR != -1 {
			start = foundR
		}
	case foundR != -1:
		start = foundR
	default:
		return raw
	}
	kv.value = append(kv.value[:0], raw...)
	raw = kv.value
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case rChar, nChar:
			raw[i] = ' '
		default:
			continue
		}
	}
	return raw
}

// removeNewLines will replace `\r` and `\n` with an empty space.
func removeNewLines(raw []byte) []byte {
	// check if a `\r` is present and save the position.
	// if no `\r` is found, check if a `\n` is present.
	foundR := bytes.IndexByte(raw, rChar)
	foundN := bytes.IndexByte(raw, nChar)
	start := 0

	switch {
	case foundN != -1:
		if foundR > foundN {
			start = foundN
		} else if foundR != -1 {
			start = foundR
		}
	case foundR != -1:
		start = foundR
	default:
		return raw
	}

	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case rChar, nChar:
			raw[i] = ' '
		default:
			continue
		}
	}
	return raw
}

// AppendNormalizedHeaderKey appends normalized header key (name) to dst
// and returns the resulting dst.
//
// Normalized header key starts with uppercase letter. The first letters
// after dashes are also uppercased. All the other letters are lowercased.
// Examples:
//
//   - coNTENT-TYPe -> Content-Type
//   - HOST -> Host
//   - foo-bar-baz -> Foo-Bar-Baz
func AppendNormalizedHeaderKey(dst []byte, key string) []byte {
	dst = append(dst, key...)
	normalizeHeaderKeyV2(dst[len(dst)-len(key):])
	return dst
}

// AppendNormalizedHeaderKeyBytes appends normalized header key (name) to dst
// and returns the resulting dst.
//
// Normalized header key starts with uppercase letter. The first letters
// after dashes are also uppercased. All the other letters are lowercased.
// Examples:
//
//   - coNTENT-TYPe -> Content-Type
//   - HOST -> Host
//   - foo-bar-baz -> Foo-Bar-Baz
func AppendNormalizedHeaderKeyBytes(dst, key []byte) []byte {
	return AppendNormalizedHeaderKey(dst, b2s(key))
}

func appendArgsKeyBytes(dst []byte, args []argsKV, sep []byte) []byte {
	for i, n := 0, len(args); i < n; i++ {
		kv := &args[i]
		dst = append(dst, kv.key...)
		if i+1 < n {
			dst = append(dst, sep...)
		}
	}
	return dst
}

var (
	errNeedMore    = errors.New("need more data: cannot find trailing lf")
	errInvalidName = errors.New("invalid header name")
	errSmallBuffer = errors.New("small read buffer. Increase ReadBufferSize")
)

type PeekOneByteErr struct {
	error
}

// ErrSmallBuffer is returned when the provided buffer size is too small
// for reading request and/or response headers.
//
// ReadBufferSize value from Server or clients should reduce the number
// of such errors.
type ErrSmallBuffer struct {
	error
}

type HeaderParseErr struct {
	s string
}

func (h *HeaderParseErr) Error() string { return h.s }

func newHeaderParseErr(what, val string) *HeaderParseErr {
	return &HeaderParseErr{s: what + ":" + val}
}

var ContentLengthNotIntErr = newHeaderParseErr("Content-Length not int string", "")
var BadTrailerInHeader = newHeaderParseErr("header contain forbidden trailer", "")
var MultipleContentLengthErr = newHeaderParseErr("multiple Content-Length headers", "")
var EmptyHeaderKeyErr = newHeaderParseErr("empty header key", "")
var HeaderBufferSmallErr = errors.New("header buffer is too small")
var TrailerHeaderBufferSmallErr = errors.New("trailer header buffer is too small")

func mustPeekBuffered(r *bufio.Reader) []byte {
	buf, err := r.Peek(r.Buffered())
	if len(buf) == 0 || err != nil {
		panic(fmt.Sprintf("bufio.Reader.Peek() returned unexpected data (%q, %v)", buf, err))
	}
	return buf
}

func mustDiscard(r *bufio.Reader, n int) {
	if _, err := r.Discard(n); err != nil {
		panic(fmt.Sprintf("bufio.Reader.Discard(%d) failed: %v", n, err))
	}
}

func headNameCompare(a, b []byte, disableNormalize bool) bool {
	if !disableNormalize {
		return string(a) == string(b)
	}
	return caseInsensitiveCompare(a, b)
}
