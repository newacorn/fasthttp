package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	ucompress "utils/compress"

	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp/stackless"
)

const (
	CompressZstdSpeedNotSet = iota
	CompressZstdBestSpeed
	CompressZstdDefault
	CompressZstdSpeedBetter
	CompressZstdBestCompression
)

var (
	stacklessZstdWriterPoolMap = newCompressWriterPoolMap()
)

func acquireZstdReader(r io.Reader) (zr ucompress.ZstdReader, err error) {
	zr = ucompress.DefaultZstdReaderPool.Get()
	err = zr.Reset(r)
	return
}

func releaseZstdReader(zr ucompress.ZstdReader) {
	ucompress.DefaultZstdReaderPool.Put(zr)
}

func acquireZstdWriter(w io.Writer, level int) (zw ucompress.Writer, err error) {
	zw = ucompress.DefaultZstdCompressPools.Pool(level).Get()
	zw.Reset(w)
	return
}

func releaseZstdWriter(zw *zstd.Encoder, level int) { //nolint:unused
	_ = zw.Close()
	ucompress.DefaultZstdCompressPools.Pool(level).Put(zw)
}

func acquireStacklessZstdWriter(w io.Writer, compressLevel int) stackless.Writer {
	nLevel := normalizeZstdCompressLevel(compressLevel)
	p := stacklessZstdWriterPoolMap[nLevel]
	v := p.Get()
	if v == nil {
		return stackless.NewWriter(w, func(w io.Writer) stackless.Writer {
			return acquireRealZstdWriter(w, compressLevel)
		})
	}
	sw := v.(stackless.Writer)
	sw.Reset(w)
	return sw
}

func releaseStacklessZstdWriter(zf stackless.Writer, zstdDefault int) {
	_ = zf.Close()
	nLevel := normalizeZstdCompressLevel(zstdDefault)
	p := stacklessZstdWriterPoolMap[nLevel]
	p.Put(zf)
}

func acquireRealZstdWriter(w io.Writer, level int) (zw ucompress.Writer) {
	zw = ucompress.DefaultZstdCompressPools.Pool(level).Get()
	zw.Reset(w)
	return
}

func releaseRealZstdWriter(zw ucompress.Writer, level int) {
	_ = zw.Flush()
	_ = zw.Close()
	ucompress.DefaultZstdCompressPools.Pool(level).Put(zw)
}

func AppendZstdBytesLevel(dst, src []byte, level int) []byte {
	w := &byteSliceWriter{dst}
	_, _ = WriteZstdLevel(w, src, level) //nolint:errcheck
	return w.b
}

func WriteZstdLevel(w io.Writer, p []byte, level int) (int, error) {
	level = normalizeZstdCompressLevel(level)
	switch w.(type) {
	case *byteSliceWriter,
		*bytes.Buffer,
		*bytebufferpool.ByteBuffer:
		ctx := &compressCtx{
			w:     w,
			p:     p,
			level: level,
		}
		stacklessWriteZstd(ctx)
		return len(p), nil
	default:
		zw := acquireStacklessZstdWriter(w, level)
		n, err := zw.Write(p)
		releaseStacklessZstdWriter(zw, level)
		return n, err
	}
}

var (
	stacklessWriteZstdOnce sync.Once
	stacklessWriteZstdFunc func(ctx any) bool
)

func stacklessWriteZstd(ctx any) {
	stacklessWriteZstdOnce.Do(func() {
		stacklessWriteZstdFunc = stackless.NewFunc(nonblockingWriteZstd)
	})
	stacklessWriteZstdFunc(ctx)
}

func nonblockingWriteZstd(ctxv any) {
	ctx := ctxv.(*compressCtx)
	zw := acquireRealZstdWriter(ctx.w, ctx.level)
	_, _ = zw.Write(ctx.p) //nolint:errcheck
	releaseRealZstdWriter(zw, ctx.level)
}

// AppendZstdBytes appends zstd src to dst and returns the resulting dst.
func AppendZstdBytes(dst, src []byte) []byte {
	return AppendZstdBytesLevel(dst, src, CompressZstdDefault)
}

// WriteUnzstd writes unzstd p to w and returns the number of uncompressed
// bytes written to w.
func WriteUnzstd(w io.Writer, p []byte) (int, error) {
	r := &byteSliceReader{p}
	zr, err := acquireZstdReader(r)
	if err != nil {
		return 0, err
	}
	n, err := copyZeroAlloc(w, zr)
	releaseZstdReader(zr)
	nn := int(n)
	if int64(nn) != n {
		return 0, fmt.Errorf("too much data unzstd: %d", n)
	}
	return nn, err
}

// AppendUnzstdBytes appends unzstd src to dst and returns the resulting dst.
func AppendUnzstdBytes(dst, src []byte) ([]byte, error) {
	w := &byteSliceWriter{dst}
	_, err := WriteUnzstd(w, src)
	return w.b, err
}

// normalizes compression level into [0..7], so it could be used as an index
// in *PoolMap.
func normalizeZstdCompressLevel(level int) int {
	if level >= 5 {
		level = int(zstd.SpeedBestCompression)
	}
	if level <= 0 {
		level = int(zstd.SpeedFastest)
	}
	return level
}
