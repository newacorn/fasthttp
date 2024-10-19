package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	pbufio "github.com/newacorn/goutils/bufio"
	pbytes "github.com/newacorn/goutils/bytes"
	"io"
	"utils/compress"
)

type WriterFlusherCloser interface {
	Flush() error
	io.Writer
}
type WriterFlusher interface {
	Flush() error
	io.Writer
}

type BufferedChunkedWriter struct {
	*pbufio.Writer
}

func (bw BufferedChunkedWriter) Write(b []byte) (n int, err error) {
	return bw.Writer.Write(b)
}
func (bw BufferedChunkedWriter) Flush() (err error) {
	err = bw.Writer.Flush()
	bw.Writer.RecycleItems()
	return
}

// const c = len(HeaderContentLength)
const k = HeaderContentLength + ": "

type ChunkedWriter struct {
	*bufio.Writer
}

func (w ChunkedWriter) Write(b []byte) (n int, err error) {
	n = len(b)
	if n == 0 {
		return
	}
	err = writeChunkWithoutFlush(w.Writer, b)
	return
}

func (w ChunkedWriter) Flush() error {
	return nil
}

func (w ChunkedWriter) SubFlush() error {
	return w.Writer.Flush()
}

func (resp *Response) WriteCompress(w *bufio.Writer) (err error) {
	sendBody := !resp.mustSkipBody()

	if resp.bodyStream != nil {
		return resp.writeCompressBodyStream(w, sendBody)
	}

	body := resp.bodyBytes()
	bodyLen := len(body)
	//In HTTP/1.0 - server response without content-length is when the stream closes
	//In HTTP/1.1 - server response without content-length is when the response is chunked encoded
	if !sendBody {
		if bodyLen > 0 {
			resp.Header.SetContentLength(int64(bodyLen))
		}
		// No longer perform compression, so the `Content-Encoding` header should be removed.
		resp.Header.DelCanonical(strContentEncoding)
		err = resp.WriteHeader(w)
		return
	}
	//
	err = resp.Header.WriteCompress(w)
	if err == nil {
		if resp.ImmediateHeaderFlush {
			err = w.Flush()
		}
	}
	if err != nil {
		return
	}
	compressPool := resp.compressPool
	compressW := compressPool.Get()
	// We cannot directly write the compressed data to the buffer on top of `net.Conn`
	// because we wouldn't know the size of the response body.
	buf := pbytes.NewBufferWithSize(bodyLen)
	compressW.Reset(buf)
	_, err = compressW.Write(body)
	err2 := compressW.Close()
	compressPool.Put(compressW)
	if err == nil {
		err = err2
	}
	if err != nil {
		buf.RecycleItems()
		return
	}
	// write content-length
	tmp := make([]byte, 32)
	n1 := copy(tmp, k)
	n2 := copy(tmp[n1:], AppendUintInto(tmp[n1:], buf.Len()))
	n3 := copy(tmp[n1+n2:], "\r\n\r\n")
	_, err = w.Write(tmp[:n1+n2+n3])
	// write compressed body
	_, err = w.Write(buf.Bytes())
	buf.RecycleItems()
	return
}

var chunkedEnd = []byte("0\r\n")

// filter sendBody=false
func (resp *Response) writeCompressBodyStream(w *bufio.Writer, sendBody bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &WriteBodyStreamPanic{
				error: fmt.Errorf("panic while writing body stream: %+v", r),
			}
		}
	}()
	//
	contentLength := resp.Header.ContentLength()
	if contentLength < 0 {
		if contentLength == -2 {
			// Set `Connection: Close`, and then `Content-Length` will be rewritten.
			resp.Header.SetConnectionClose()
		}
		lrSize := limitedReaderSize(resp.bodyStream)
		if lrSize >= 0 {
			contentLength = lrSize
		}
	}
	// Always use Chunked encoding regardless.
	resp.Header.SetContentLength(-1)
	err = resp.WriteHeader(w)
	if err == nil && sendBody {
		//
		var wrapperWriter WriterFlusher
		compressPool := resp.compressPool
		if compressPool.NeedBuffer() {
			wrapperWriter = BufferedChunkedWriter{pbufio.NewWriter(ChunkedWriter{Writer: w})}
		} else {
			wrapperWriter = ChunkedWriter{Writer: w}
		}
		compressW := compressPool.Get()
		compressW.Reset(wrapperWriter)
		var n int64
		n, err = copyZeroAlloc(compressW, resp.bodyStream)
		if err == nil {
			// Size validation.
			if contentLength >= 0 && contentLength != n {
				err = fmt.Errorf("copied %d bytes from body stream instead of %d bytes", n, contentLength)
			}
		}
		// Release compressed resources.
		err2 := compressW.Close()
		compressPool.Put(compressW)
		if err == nil {
			err = err2
		}
		if err == nil {
			err = wrapperWriter.Flush()
			if err == nil {
				_, err = w.Write(chunkedEnd)
				if err == nil {
					err = resp.Header.writeTrailer(w)
				}
			}
		}
	}
	// whatever should call closeBodyStream method.
	err2 := resp.closeBodyStream(err)
	if err == nil {
		err = err2
	}
	return
}

func (resp *Response) needCompress() bool {
	return resp.compressPool != nil
}

func (resp *Response) CompressPool() compress.Pooler {
	return resp.compressPool
}

func (resp *Response) SetCompressPool(compressPool compress.Pooler) {
	resp.compressPool = compressPool
}

func (h *ResponseHeader) AppendBytesNoCRLFEncodingLength(dst []byte) []byte {
	//defer func() { h.RecycleItems() }()
	dst = h.appendStatusLine(dst[:0])

	server := h.Server()
	if len(server) != 0 {
		dst = appendHeaderLine(dst, strServer, server)
	}

	if !h.noDefaultDate {
		serverDateOnce.Do(updateServerDate)
		dst = appendHeaderLine(dst, strDate, *serverDate.Load())
	}

	// Append Content-Type only for non-zero responses
	// or if it is explicitly set.
	// See https://github.com/valyala/fasthttp/issues/28 .
	if h.ContentLength() != 0 || len(h.contentType) > 0 {
		contentType := h.ContentType()
		if len(contentType) > 0 {
			dst = appendHeaderLine(dst, strContentType, contentType)
		}
	}
	/*
		contentEncoding := h.ContentEncoding()
		if len(contentEncoding) > 0 {
			dst = appendHeaderLine(dst, strContentEncoding, contentEncoding)
		}
	*/

	/*
		if len(h.contentLengthBytes) > 0 {
			dst = appendHeaderLine(dst, strContentLength, h.contentLengthBytes)
		}
	*/

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
		if !exclude && (h.noDefaultDate || !bytes.Equal(kv.key, strDate)) {
			dst = appendHeaderLine(dst, kv.key, kv.value)
		}
	}

	if len(h.trailer) > 0 {
		dst = appendHeaderLine(dst, strTrailer, appendArgsKeyBytes(nil, h.trailer, strCommaSpace))
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

	return dst
}
func (h *ResponseHeader) HeaderNoCRLFEncodingLength() []byte {
	h.bufV = h.AppendBytesNoCRLFEncodingLength(h.bufV[:0])
	return h.bufV
}

func (h *ResponseHeader) WriteNoCRLFEncodingLength(w *bufio.Writer) error {
	_, err := w.Write(h.HeaderNoCRLFEncodingLength())
	return err
}

func writeChunkWithoutFlush(w *bufio.Writer, b []byte) error {
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
	if n > 0 {
		if _, err := w.Write(strCRLF); err != nil {
			return err
		}
	}
	//return w.Flush()
	return nil
}
