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
	hasTrailingSlash := len(path) > 0 && path[len(path)-1] == '/'
	path = stripTrailingSlashes(path)

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

	mustCompress := false
	fileCacheKind := defaultCacheKind
	fileEncoding := ""
	byteRange := ctx.Request.Header.peek(strRange)
	tryCompress := false
	if h.needCompressFunc != nil {
		tryCompress = h.needCompressFunc(path)
	} else {
		tryCompress = h.compress
	}
	if len(byteRange) == 0 && tryCompress {
		switch {
		case h.compressBrotli && ctx.Request.Header.HasAcceptEncodingBytes(strBr):
			mustCompress = true
			fileCacheKind = brotliCacheKind
			fileEncoding = "br"
		case ctx.Request.Header.HasAcceptEncodingBytes(strGzip):
			mustCompress = true
			fileCacheKind = gzipCacheKind
			fileEncoding = "gzip"
		case ctx.Request.Header.HasAcceptEncodingBytes(strZstd):
			mustCompress = true
			fileCacheKind = zstdCacheKind
			fileEncoding = "zstd"
		}
	}

	pathStr := string(path)

	ff, ok := h.cacheManager.GetFileFromCache(fileCacheKind, pathStr)
	if !ok {
		filePath := h.pathToFilePath(pathStr)

		ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		}

		if errors.Is(err, errDirIndexRequired) {
			if !hasTrailingSlash {
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				err = nil
				return
			}
			ff, err = h.openIndexFile(ctx, filePath, mustCompress, fileEncoding)
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				err = FsDirIndexFileCantOpenError
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			err = FsFileNotFoundOrCantOpenError
			return
		}

		ff = h.cacheManager.SetFileToCache(fileCacheKind, pathStr, ff)
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.decReadersCount()
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
		switch fileEncoding {
		case "br":
			hdr.SetContentEncodingBytes(strBr)
		case "gzip":
			hdr.SetContentEncodingBytes(strGzip)
		case "zstd":
			hdr.SetContentEncodingBytes(strZstd)
		}
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
