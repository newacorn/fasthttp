//go:build other12

package fasthttp

import (
	"bytes"
	"github.com/pkg/errors"
	"io"
)

var FsNilByteInPathError = errors.New("fasthttp fs serve: nil byte in path")
var FsDotDotSlashInPathError = errors.New("fasthttp fs serve: '/../' in path")
var FsDirIndexFileCantOpenError = errors.New("fasthttp fs serve: can't open directory index file")
var FsFileNotFoundOrCantOpenError = errors.New("fasthttp fs serve: file not found or can't open")
var FsCantGetFileReader = errors.New("fasthttp fs serve: can't get file reader")
var FsRangeNotSatisfiableError = errors.New("fasthttp fs serve: range not satisfiable")
var FsCantSeekByRangeError = errors.New("fasthttp fs serve: can seek by range")
var FsCantCloseFileReader = errors.New("fasthttp fs serve: cannot close file reader")

type RequestHandlerWithError func(ctx *RequestCtx) (err error)

func (h *fsHandler) handleRequestWithError(ctx *RequestCtx) (err error) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	var hasTrailingSlash bool
	if len(path) > 0 && path[len(path)-1] == '/' {
		hasTrailingSlash = true
		path = path[:len(path)-1]
	}

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		err = FsNilByteInPathError
		return
	}
	if h.pathRewrite != nil {
		// There is no need to check for '/../' if path = ctx.Path(),
		// since ctx.Path must normalize and sanitize the path.
		if n := bytes.Index(path, strSlashDotDotSlash); n >= 0 {
			ctx.Logger().Printf("cannot serve path with '/../' at position %d due to security reasons: %q", n, path)
			err = FsDotDotSlashInPathError
			return
		}
	}

	var (
		mustCompress bool
		tryCompress  bool
		fileEncoding string
	)
	fileCacheKind := defaultCacheKind
	byteRange := ctx.Request.Header.peek(strRange)
	//
	if h.needCompressFunc != nil {
		tryCompress = h.needCompressFunc(path)
	} else {
		tryCompress = h.compress
	}
	var compressorIndex int
	if len(byteRange) == 0 && tryCompress {
		for i, en := range h.Orders {
			if en == "" {
				continue
			}
			if ctx.Request.Header.HasAcceptEncodingBytes(s2b(string(en))) {
				compressorIndex = i
				//compressor = h.order2pool[i]
				fileEncoding = string(en)
				mustCompress = true
				fileCacheKind = h.order2kind[i]
				break
			}
		}
	}

	pathStr := string(path)

	ff, ok := h.cacheManager.GetFileFromCache(fileCacheKind, pathStr)
	if !ok {
		filePath := h.pathToFilePath(pathStr)

		ff, err = h.openFSFile(filePath, pathStr, mustCompress, compressorIndex)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, pathStr, false, compressorIndex)
		}

		if err != nil {
			if errors.Is(err, errDirIndexRequired) {
				if !hasTrailingSlash {
					ctx.RedirectBytes(append(path, '/'), StatusFound)
					err = nil
					return
				}
				ff, err = h.openIndexFile(ctx, filePath, pathStr, mustCompress, compressorIndex)

				if err != nil {
					ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
					err = FsDirIndexFileCantOpenError
					return
				}
			}
			if ff == nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				//err = FsDirIndexFileCantOpenErr
				err = FsFileNotFoundOrCantOpenError
				return
			}
		}
		//ff = h.cacheManager.SetFileToCache(fileCacheKind, pathStr, ff)
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		//ff.decReadersCount()
		ff.readersCount.Add(-1)
		ctx.NotModified()
		return
	}

	r, err := ff.NewReader()
	if err != nil {
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %v", path, err)
		err = FsCantGetFileReader
		return
	}

	hdr := &ctx.Response.Header
	if ff.compressed {
		hdr.SetContentEncoding(fileEncoding)
	}

	statusCode := StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.setNonSpecial(strAcceptRanges, strBytes)
		if len(byteRange) > 0 {
			var startPos, endPos int
			startPos, endPos, err = ParseByteRange(byteRange, contentLength)
			if err != nil {
				_ = r.(io.Closer).Close()
				ctx.Logger().Printf("cannot parse byte range %q for path=%q: %v", byteRange, path, err)
				err = FsRangeNotSatisfiableError
				return
			}

			if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil {
				_ = r.(io.Closer).Close()
				ctx.Logger().Printf("cannot seek byte range %q for path=%q: %v", byteRange, path, err)
				err = FsCantSeekByRangeError
				return
			}

			hdr.SetContentRange(startPos, endPos, contentLength)
			contentLength = endPos - startPos + 1
			statusCode = StatusPartialContent
		}
	}

	hdr.setNonSpecial(strLastModified, ff.lastModifiedStr)
	if !ctx.IsHead() {
		ctx.SetBodyStream(r, contentLength)
	} else {
		ctx.Response.ResetBody()
		ctx.Response.SkipBody = true
		ctx.Response.Header.SetContentLength(contentLength)
		if rc, ok := r.(io.Closer); ok {
			if err = rc.Close(); err != nil {
				ctx.Logger().Printf("cannot close file reader: %v", err)
				err = FsCantCloseFileReader
				return
			}
		}
	}
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
	return
}
