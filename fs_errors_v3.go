package fasthttp

import (
	"bytes"
	"github.com/pkg/errors"
	"io"
)

var FsNilByteInPathErr = errors.New("fasthttp Fs serve: nil byte in path")
var FsDotDotSlashInPathErr = errors.New("fasthttp Fs serve: '/../' in path")
var FsDirIndexFileCantOpenErr = errors.New("fasthttp Fs serve: can't open directory index file")
var FsFileNotFoundOrCantOpenErr = errors.New("fasthttp Fs serve: file not found or can't open")
var FsNotAllowedIndexDirErr = errors.New("fasthttp Fs serve: want visit dir")
var FsFileNotFoundErr = errors.New("fasthttp Fs serve: opened file not found")
var FsCantHandleBigFileErr = errors.New("fasthttp Fs serve: cant handle this size file on this platform")
var FsOpenedFileNotSeekableErr = errors.New("fasthttp Fs serve: opened file not seekable")
var FsSeekStartNotZeroErr = errors.New("bug: File.Seek(0, io.SeekStart) returned (0, nil)")
var FsPermissionDeniedErr = errors.New("fasthttp Fs serve: permission denied")
var FsCantGetFileReader = errors.New("fasthttp Fs serve: can't get file reader")
var FsRangeNotSatisfiableError = errors.New("fasthttp Fs serve: range not satisfiable")
var FsCantSeekByRangeError = errors.New("fasthttp Fs serve: can seek by range")
var FsCantCloseFileReader = errors.New("Fasthttp Fs serve: cannot close file reader")

type RequestHandlerWithError func(ctx *RequestCtx) (err error)

func (h *fsHandler) handleRequestWithError(ctx *RequestCtx) (err error) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	//
	if n := bytes.IndexByte(path, 0); n >= 0 {
		err = FsNilByteInPathErr
		return
	}
	if h.pathRewrite != nil {
		// There is no need to check for '/../' if path = ctx.Path(),
		// since ctx.Path must normalize and sanitize the path.
		if n := bytes.Index(path, strSlashDotDotSlash); n >= 0 {
			err = FsDotDotSlashInPathErr
			return
		}
	}

	hasTrailingSlash := len(path) > 0 && path[len(path)-1] == '/'
	var (
		fileEncoding  string
		fileCacheKind CacheKind
		byteRange     = ctx.Request.Header.PeekCanonical(strRange)
		mustCompress  = h.compress && len(byteRange) == 0
	)
	originalPathStr := string(path)
	pathStr := originalPathStr
	if hasTrailingSlash {
		pathStr = pathStr[:len(pathStr)-1]
	}
	if mustCompress {
		if h.needCompressFunc != nil {
			mustCompress = h.needCompressFunc(path)
		}
	}
	if mustCompress {
		for i, en := range h.Orders {
			if en == "" {
				continue
			}
			if ctx.Request.Header.HasAcceptEncodingBytes(s2b(string(en))) {
				fileEncoding = string(en)
				fileCacheKind = h.order2kind[i]
				break
			}
		}
		mustCompress = fileEncoding != ""
	}
	var (
		ff *fsFile
		ok bool
	)

	if h.cacheManager != nil {
		ff, ok = h.cacheManager.GetFileFromCache(fileCacheKind, originalPathStr)
	}
	//
	if !ok {
		filePath := h.pathToFilePath(pathStr)

		ff, err = h.openFSFile(filePath, originalPathStr, hasTrailingSlash, mustCompress, fileCacheKind)
		if err != nil {
			if err == errDirIndexRequired {
				err = nil
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				return
			}
			return
		}
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.readersCount.Add(-1)
		ctx.NotModified()
		return
	}
	//
	r, err := ff.NewReader()
	if err != nil {
		return
	}
	//
	hdr := &ctx.Response.Header
	if ff.compressed {
		hdr.SetContentEncoding(fileEncoding)
	}

	statusCode := StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.setNonSpecial(strAcceptRanges, strBytes)
		if len(byteRange) > 0 {
			var startPos, endPos int64
			startPos, endPos, err = ParseByteRange(byteRange, contentLength)
			if err != nil {
				_ = r.(io.Closer).Close()
				return
			}

			if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil {
				_ = r.(io.Closer).Close()
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
		if rc, ok1 := r.(io.Closer); ok1 {
			if err = rc.Close(); err != nil {
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
