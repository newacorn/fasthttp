package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"sync"
	ucompress "utils/compress"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/gzip"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp/stackless"
)

// Supported compression levels.
const (
	CompressNoCompression      = flate.NoCompression
	CompressBestSpeed          = flate.BestSpeed
	CompressBestCompression    = flate.BestCompression
	CompressDefaultCompression = -1 // flate.DefaultCompression
	CompressHuffmanOnly        = -2 // flate.HuffmanOnly
)

func acquireGzipReader(r io.Reader) (zr *gzip.Reader, err error) {
	zr = ucompress.DefaultGzipReaderPool.Get()
	err = zr.Reset(r)
	return
}

func releaseGzipReader(zr *gzip.Reader) {
	_ = zr.Close()
	ucompress.DefaultGzipReaderPool.Put(zr)
}

func acquireFlateReader(r io.Reader) (dr ucompress.DeflateReader, err error) {
	dr = ucompress.DefaultDeflateReaderPool.Get()
	err = dr.Reset(r)
	return
}

func releaseFlateReader(dr ucompress.DeflateReader) {
	_ = dr.Close()
	ucompress.DefaultDeflateReaderPool.Put(dr)
}

func acquireStacklessGzipWriter(w io.Writer, level int) stackless.Writer {
	p := stacklessGzipWriterPoolMap[normalizeCompressLevel(level)]
	v := p.Get()
	if v == nil {
		return stackless.NewWriter(w, func(w io.Writer) stackless.Writer {
			return acquireRealGzipWriter(w, level)
		})
	}
	sw := v.(stackless.Writer)
	sw.Reset(w)
	return sw
}

func releaseStacklessGzipWriter(sw stackless.Writer, level int) {
	_ = sw.Close()
	stacklessGzipWriterPoolMap[normalizeCompressLevel(level)].Put(sw)
}

func acquireRealGzipWriter(w io.Writer, level int) (gw ucompress.Writer) {
	gw = ucompress.DefaultGzipCompressPools.Pool(level).Get()
	gw.Reset(w)
	return
}

func releaseRealGzipWriter(zw ucompress.Writer, level int) {
	_ = zw.Close()
	ucompress.DefaultGzipCompressPools.Pool(level).Put(zw)
	return
}

var (
	stacklessGzipWriterPoolMap = newCompressWriterPoolMap()
)

// AppendGzipBytesLevel appends gzipped src to dst using the given
// compression level and returns the resulting dst.
//
// Supported compression levels are:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
func AppendGzipBytesLevel(dst, src []byte, level int) []byte {
	w := &byteSliceWriter{dst}
	_, _ = WriteGzipLevel(w, src, level) //nolint:errcheck
	return w.b
}

// WriteGzipLevel writes gzipped p to w using the given compression level
// and returns the number of compressed bytes written to w.
//
// Supported compression levels are:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
func WriteGzipLevel(w io.Writer, p []byte, level int) (int, error) {
	switch w.(type) {
	case *byteSliceWriter,
		*bytes.Buffer,
		*bytebufferpool.ByteBuffer:
		// These writers don't block, so we can just use stacklessWriteGzip
		ctx := &compressCtx{
			w:     w,
			p:     p,
			level: level,
		}
		stacklessWriteGzip(ctx)
		return len(p), nil
	default:
		zw := acquireStacklessGzipWriter(w, level)
		n, err := zw.Write(p)
		releaseStacklessGzipWriter(zw, level)
		return n, err
	}
}

var (
	stacklessWriteGzipOnce sync.Once
	stacklessWriteGzipFunc func(ctx any) bool
)

func stacklessWriteGzip(ctx any) {
	stacklessWriteGzipOnce.Do(func() {
		stacklessWriteGzipFunc = stackless.NewFunc(nonblockingWriteGzip)
	})
	stacklessWriteGzipFunc(ctx)
}

func nonblockingWriteGzip(ctxv any) {
	ctx := ctxv.(*compressCtx)
	zw := acquireRealGzipWriter(ctx.w, ctx.level)
	_, _ = zw.Write(ctx.p) //nolint:errcheck // no way to handle this error anyway
	releaseRealGzipWriter(zw, ctx.level)
}

// WriteGzip writes gzipped p to w and returns the number of compressed
// bytes written to w.
func WriteGzip(w io.Writer, p []byte) (int, error) {
	return WriteGzipLevel(w, p, CompressDefaultCompression)
}

// AppendGzipBytes appends gzipped src to dst and returns the resulting dst.
func AppendGzipBytes(dst, src []byte) []byte {
	return AppendGzipBytesLevel(dst, src, CompressDefaultCompression)
}

// WriteGunzip writes ungzipped p to w and returns the number of uncompressed
// bytes written to w.
func WriteGunzip(w io.Writer, p []byte) (n int, err error) {
	r := &byteSliceReader{p}
	zr := ucompress.DefaultGzipReaderPool.Get()
	err = zr.Reset(r)
	if err != nil {
		return 0, err
	}
	n1, err := copyZeroAlloc(w, zr)
	err = zr.Close()
	if err != nil {
		return
	}
	ucompress.DefaultGzipReaderPool.Put(zr)
	n = int(n1)
	return
}

// AppendGunzipBytes appends gunzipped src to dst and returns the resulting dst.
func AppendGunzipBytes(dst, src []byte) ([]byte, error) {
	w := &byteSliceWriter{dst}
	_, err := WriteGunzip(w, src)
	return w.b, err
}

// AppendDeflateBytesLevel appends deflated src to dst using the given
// compression level and returns the resulting dst.
//
// Supported compression levels are:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
func AppendDeflateBytesLevel(dst, src []byte, level int) []byte {
	w := &byteSliceWriter{dst}
	_, _ = WriteDeflateLevel(w, src, level) //nolint:errcheck
	return w.b
}

// WriteDeflateLevel writes deflated p to w using the given compression level
// and returns the number of compressed bytes written to w.
//
// Supported compression levels are:
//
//   - CompressNoCompression
//   - CompressBestSpeed
//   - CompressBestCompression
//   - CompressDefaultCompression
//   - CompressHuffmanOnly
func WriteDeflateLevel(w io.Writer, p []byte, level int) (int, error) {
	switch w.(type) {
	case *byteSliceWriter,
		*bytes.Buffer,
		*bytebufferpool.ByteBuffer:
		// These writers don't block, so we can just use stacklessWriteDeflate
		ctx := &compressCtx{
			w:     w,
			p:     p,
			level: level,
		}
		stacklessWriteDeflate(ctx)
		return len(p), nil
	default:
		zw := acquireStacklessDeflateWriter(w, level)
		n, err := zw.Write(p)
		releaseStacklessDeflateWriter(zw, level)
		return n, err
	}
}

var (
	stacklessWriteDeflateOnce sync.Once
	stacklessWriteDeflateFunc func(ctx any) bool
)

func stacklessWriteDeflate(ctx any) {
	stacklessWriteDeflateOnce.Do(func() {
		stacklessWriteDeflateFunc = stackless.NewFunc(nonblockingWriteDeflate)
	})
	stacklessWriteDeflateFunc(ctx)
}

func nonblockingWriteDeflate(ctxv any) {
	ctx := ctxv.(*compressCtx)
	zw := acquireRealDeflateWriter(ctx.w, ctx.level)

	_, _ = zw.Write(ctx.p) //nolint:errcheck // no way to handle this error anyway

	releaseRealDeflateWriter(zw, ctx.level)
}

type compressCtx struct {
	w     io.Writer
	p     []byte
	level int
}

// WriteDeflate writes deflated p to w and returns the number of compressed
// bytes written to w.
func WriteDeflate(w io.Writer, p []byte) (int, error) {
	return WriteDeflateLevel(w, p, CompressDefaultCompression)
}

// AppendDeflateBytes appends deflated src to dst and returns the resulting dst.
func AppendDeflateBytes(dst, src []byte) []byte {
	return AppendDeflateBytesLevel(dst, src, CompressDefaultCompression)
}

// WriteInflate writes inflated p to w and returns the number of uncompressed
// bytes written to w.
func WriteInflate(w io.Writer, p []byte) (int, error) {
	r := &byteSliceReader{p}
	zr, err := acquireFlateReader(r)
	if err != nil {
		return 0, err
	}
	n, err := copyZeroAlloc(w, zr)
	releaseFlateReader(zr)
	nn := int(n)
	if int64(nn) != n {
		return 0, fmt.Errorf("too much data inflated: %d", n)
	}
	return nn, err
}

// AppendInflateBytes appends inflated src to dst and returns the resulting dst.
func AppendInflateBytes(dst, src []byte) ([]byte, error) {
	w := &byteSliceWriter{dst}
	_, err := WriteInflate(w, src)
	return w.b, err
}

type byteSliceWriter struct {
	b []byte
}

func (w *byteSliceWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

type byteSliceReader struct {
	b []byte
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func (r *byteSliceReader) ReadByte() (byte, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := r.b[0]
	r.b = r.b[1:]
	return n, nil
}

func acquireStacklessDeflateWriter(w io.Writer, level int) stackless.Writer {
	nLevel := normalizeCompressLevel(level)
	p := stacklessDeflateWriterPoolMap[nLevel]
	v := p.Get()
	if v == nil {
		return stackless.NewWriter(w, func(w io.Writer) stackless.Writer {
			return acquireRealDeflateWriter(w, level)
		})
	}
	sw := v.(stackless.Writer)
	sw.Reset(w)
	return sw
}

func releaseStacklessDeflateWriter(sw stackless.Writer, level int) {
	_ = sw.Close()
	nLevel := normalizeCompressLevel(level)
	p := stacklessDeflateWriterPoolMap[nLevel]
	p.Put(sw)
}

func acquireRealDeflateWriter(w io.Writer, level int) (dw ucompress.Writer) {
	dw = ucompress.DefaultDeflateCompressPools.Pool(level).Get()
	dw.Reset(w)
	return
}

func releaseRealDeflateWriter(dw ucompress.Writer, level int) {
	_ = dw.Close()
	ucompress.DefaultDeflateCompressPools.Pool(level).Put(dw)
}

var (
	stacklessDeflateWriterPoolMap = newCompressWriterPoolMap()
)

func newCompressWriterPoolMap() []*sync.Pool {
	var m = make([]*sync.Pool, 12)
	for i := 0; i < 12; i++ {
		m[i] = &sync.Pool{}
	}
	return m
}

func isFileCompressible(f fs.File, minCompressRatio float64) bool {
	// Try compressing the first 4kb of the file
	// and see if it can be compressed by more than
	// the given minCompressRatio.
	b := bytebufferpool.Get()
	zw := acquireStacklessGzipWriter(b, CompressDefaultCompression)
	lr := &io.LimitedReader{
		R: f,
		N: 4096,
	}
	_, err := copyZeroAlloc(zw, lr)
	if err != nil {
		return false
	}
	releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	seeker, ok := f.(io.Seeker)
	if !ok {
		return false
	}
	_, err = seeker.Seek(0, io.SeekStart) //nolint:errcheck
	if err != nil {
		return false
	}
	n := 4096 - lr.N
	zn := len(b.B)
	bytebufferpool.Put(b)
	return float64(zn) < float64(n)*minCompressRatio
}

// normalizes compression level into [0..11], so it could be used as an index
// in *PoolMap.
func normalizeCompressLevel(level int) int {
	// -2 is the lowest compression level - CompressHuffmanOnly
	// 9 is the highest compression level - CompressBestCompression
	if level < -2 || level > 9 {
		level = CompressDefaultCompression
	}
	return level + 2
}
