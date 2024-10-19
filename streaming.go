package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

type headerInterface interface {
	ContentLength() int64
	ReadTrailer(r *bufio.Reader) error
}

type requestStream struct {
	header          headerInterface
	prefetchedBytes *bytes.Reader
	reader          *bufio.Reader
	totalBytesRead  int64
	chunkLeft       int
}

func (rs *requestStream) Read(p []byte) (n int, err error) {
	// here Content-Length must > 0 or -1 for chunked encoding.
	var (
		chunkSize int
	)
	if rs.header.ContentLength() == -1 {
		if rs.chunkLeft == 0 {
			// read a chunk size and consume size + CRLF
			chunkSize, err = parseChunkSize(rs.reader)
			if err != nil {
				return
			}
			if chunkSize == 0 {
				err = rs.header.ReadTrailer(rs.reader)
				if err != nil {
					return
				}
				err = io.EOF
				return
			}
			rs.chunkLeft = chunkSize
		}
		bytesToRead := len(p)
		if rs.chunkLeft < len(p) {
			bytesToRead = rs.chunkLeft
		}
		n, err = rs.reader.Read(p[:bytesToRead])
		rs.totalBytesRead += int64(n)
		rs.chunkLeft -= n
		if err == io.EOF {
			err = ErrUnexpectedReqBodyEOF
			return
		}
		if err == nil && rs.chunkLeft == 0 {
			// consume every chunked message trailing CRLF
			err = readCrLf(rs.reader)
		}
		return
	}
	if rs.totalBytesRead == rs.header.ContentLength() {
		err = io.EOF
		return
	}
	prefetchedSize := int(rs.prefetchedBytes.Size())
	if int64(prefetchedSize) > rs.totalBytesRead {
		left := int64(prefetchedSize) - rs.totalBytesRead
		if int64(len(p)) > left {
			p = p[:left]
		}
		n, err = rs.prefetchedBytes.Read(p)
		rs.totalBytesRead += int64(n)
		return
	}
	left := rs.header.ContentLength() - rs.totalBytesRead
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err = rs.reader.Read(p)
	rs.totalBytesRead += int64(n)
	return
}

func acquireRequestStream(b *bytebufferpool.ByteBuffer, r *bufio.Reader, h headerInterface) *requestStream {
	rs := requestStreamPool.Get().(*requestStream)
	rs.prefetchedBytes = bytes.NewReader(b.B)
	rs.reader = r
	rs.header = h
	return rs
}

func releaseRequestStream(rs *requestStream) {
	rs.prefetchedBytes = nil
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.reader = nil
	rs.header = nil
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() any {
		return &requestStream{}
	},
}
