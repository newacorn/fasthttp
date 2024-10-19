package fasthttp

import (
	pio "github.com/newacorn/goutils/io"
	"io"
	"utils/compress"
)

//

//goland:noinspection GoUnhandledErrorResult
func BrotliReader(r io.Reader, level int) (newR io.ReadCloser) {
	pr, pw := pio.Pipe()
	newR = pr
	go func() {
		p := compress.DefaultBrotliCompressPools.Pool(level)
		//w := compress.DefaultBrotliCompressPools.Pool(level).Get()
		w := p.Get()
		w.Reset(pw)
		_, wErr := copyZeroAlloc(w, r)
		switch v := r.(type) {
		case io.Closer:
			v.Close()
		case ReadCloserWithError:
			v.CloseWithError(wErr) //nolint:errcheck
		}
		w.Close()
		pw.Close()
		p.Put(w)
	}()

	return
}

//goland:noinspection GoUnhandledErrorResult
func BrotliReaderPool(r io.Reader, level int) (newR io.ReadCloser) {
	pr, pw := pio.Pipe()
	newR = pr
	brotliCompressChan <- &CompressInfo{
		level: level,
		pw:    pw,
		src:   r,
	}
	return
}

const compressGoCount = 4096

type CompressInfo struct {
	level int
	pw    io.WriteCloser
	src   io.Reader
}
type CompressInfoChan chan *CompressInfo

var zstdCompressChan = make(CompressInfoChan, compressGoCount)
var brotliCompressChan = make(CompressInfoChan, compressGoCount)
var gzipCompressChan = make(CompressInfoChan, compressGoCount)
var deflateCompressChan = make(CompressInfoChan, compressGoCount)

//goland:noinspection GoUnhandledErrorResult
func ZstdReader(r io.Reader, level int) (newR io.ReadCloser) {
	pr, pw := pio.Pipe()
	newR = pr
	go func() {
		p := compress.DefaultZstdCompressPools.Pool(level)
		w := p.Get()
		w.Reset(pw)
		_, wErr := copyZeroAlloc(w, r)

		switch v := r.(type) {
		case io.Closer:
			v.Close()
		case ReadCloserWithError:
			v.CloseWithError(wErr) //nolint:errcheck
		}
		w.Flush()
		w.Close()
		pw.Close()
		p.Put(w)
	}()
	return
}

//goland:noinspection GoUnhandledErrorResult
func ZstdReaderPool(r io.Reader, level int) (newR io.ReadCloser) {
	pr, pw := pio.Pipe()
	//ps := fasthttputil.NewPipeConns()
	//pr, pw := ps.Conn1(), ps.Conn2()
	zstdCompressChan <- &CompressInfo{
		level: level,
		pw:    pw,
		src:   r,
	}
	newR = pr
	return
}

//goland:noinspection GoUnhandledErrorResult
func GzipReaderPool(r io.Reader, level int) (newR io.ReadCloser) {
	level = level + 2
	//ps := fasthttputil.NewPipeConns()
	//pr, pw := ps.Conn1(), ps.Conn2()
	pr, pw := pio.Pipe()
	//pr, pw := io.Pipe()
	newR = pr
	gzipCompressChan <- &CompressInfo{
		level: level,
		src:   r,
		pw:    pw,
	}
	return
}

//goland:noinspection GoUnhandledErrorResult
func GzipReader(r io.Reader, level int) (newR io.Reader) {
	level = level + 2
	pr, pw := pio.Pipe()
	newR = pr
	go func() {
		p := compress.DefaultGzipCompressPools.Pool(level)
		w := p.Get()
		w.Reset(pw)
		_, wErr := copyZeroAlloc(w, r)

		switch v := r.(type) {
		case io.Closer:
			v.Close()
		case ReadCloserWithError:
			v.CloseWithError(wErr) //nolint:errcheck
		}
		w.Flush()
		w.Close()
		pw.Close()
		p.Put(w)
	}()

	return
}

//goland:noinspection GoUnhandledErrorResult
func DeflateReader(r io.Reader, level int) (newR io.ReadCloser) {
	level = level + 2
	pr, pw := pio.Pipe()
	newR = pr
	go func() {
		p := compress.DefaultDeflateCompressPools.Pool(level)
		//w := DeflateWriterPools[level].Get()
		w := p.Get()
		w.Reset(pw)
		_, wErr := copyZeroAlloc(w, r)

		switch v := r.(type) {
		case io.Closer:
			v.Close()
		case ReadCloserWithError:
			v.CloseWithError(wErr) //nolint:errcheck
		}
		w.Close()
		pw.Close()
		//DeflateWriterPools[level].Put(w)
		p.Put(w)
	}()

	return
}

//goland:noinspection GoUnhandledErrorResult
func DeflateReaderPool(r io.Reader, level int) (newR io.ReadCloser) {
	level = level + 2
	//ps := fasthttputil.NewPipeConns()
	//pr, pw := ps.Conn1(), ps.Conn2()
	pr, pw := pio.Pipe()
	//pr, pw := io.Pipe()
	newR = pr
	deflateCompressChan <- &CompressInfo{
		level: level,
		src:   r,
		pw:    pw,
	}
	return
}

func init() {
	for i := 0; i < 32; i++ {
		go func() {
			//w.Write([]byte)
			for c := range zstdCompressChan {
				p := compress.DefaultZstdCompressPools.Pool(c.level)
				w := p.Get()
				w.Reset(c.pw)
				_, wErr := copyZeroAlloc(w, c.src)
				switch v := c.src.(type) {
				case io.Closer:
					_ = v.Close()
				case ReadCloserWithError:
					_ = v.CloseWithError(wErr) //nolint:errcheck
				}
				_ = w.Flush()
				_ = w.Close()
				_ = c.pw.Close()
				p.Put(w)
			}
		}()
	}
	for i := 0; i < 32; i++ {
		go func() {
			//w.Write([]byte)
			//w, _ := gzip.NewWriterLevel(nil, -1)
			for c := range gzipCompressChan {
				p := compress.DefaultGzipCompressPools.Pool(c.level)
				w := p.Get()
				w.Reset(c.pw)
				_, wErr := copyZeroAlloc(w, c.src)
				switch v := c.src.(type) {
				case io.Closer:
					_ = v.Close()
				case ReadCloserWithError:
					_ = v.CloseWithError(wErr) //nolint:errcheck
				}
				//_ = w.Flush()
				_ = w.Close()
				_ = c.pw.Close()
				//GzipWriterPools[gzip.DefaultCompression+2].Put(w)
				p.Put(w)
			}
		}()
	}

	for i := 0; i < 32; i++ {
		go func() {
			//w.Write([]byte)
			//w, _ := gzip.NewWriterLevel(nil, -1)
			for c := range deflateCompressChan {
				p := compress.DefaultDeflateCompressPools.Pool(c.level)
				w := p.Get()
				w.Reset(c.pw)
				_, wErr := copyZeroAlloc(w, c.src)
				switch v := c.src.(type) {
				case io.Closer:
					_ = v.Close()
				case ReadCloserWithError:
					_ = v.CloseWithError(wErr) //nolint:errcheck
				}
				//_ = w.Flush()
				_ = w.Close()
				_ = c.pw.Close()
				//DeflateWriterPools[c.level].Put(w)
				p.Put(w)
			}
		}()
	}

	for i := 0; i < 32; i++ {
		go func() {
			//w.Write([]byte)
			//w, _ := gzip.NewWriterLevel(nil, -1)
			for c := range brotliCompressChan {
				p := compress.DefaultBrotliCompressPools.Pool(c.level)
				w := p.Get()
				w.Reset(c.pw)
				_, wErr := copyZeroAlloc(w, c.src)
				switch v := c.src.(type) {
				case io.Closer:
					_ = v.Close()
				case ReadCloserWithError:
					_ = v.CloseWithError(wErr) //nolint:errcheck
				}
				//_ = w.Flush()
				_ = w.Close()
				_ = c.pw.Close()
				//BrotliWriterPools[4].Put(w)
				p.Put(w)
			}
		}()
	}
}
