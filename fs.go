//go:build other12

package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	pbytes "github.com/newacorn/goutils/bytes"
	pool "github.com/newacorn/simple-bytes-pool"
	"github.com/puzpuzpuz/xsync/v3"
	"html"
	"io"
	"io/fs"
	"mime"
	ghttp "net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"utils/compress"
	"utils/http"

	//"utils/http"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"
)

// ServeFileBytesUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFileBytes may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFileBytes.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytesUncompressed(ctx *RequestCtx, path []byte) {
	ServeFileUncompressed(ctx, b2s(path))
}

// ServeFileUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFile may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileUncompressed(ctx *RequestCtx, path string) {
	ctx.Request.Header.DelBytes(strAcceptEncoding)
	ServeFile(ctx, path)
}

// ServeFileBytes returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileBytesUncompressed is you don't need serving compressed
// file contents.
//
// See also RequestCtx.SendFileBytes.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytes(ctx *RequestCtx, path []byte) {
	ServeFile(ctx, b2s(path))
}

// ServeFile returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileUncompressed is you don't need serving compressed file contents.
//
// See also RequestCtx.SendFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFile(ctx *RequestCtx, path string) {
	rootFSOnce.Do(func() {
		rootFSHandler = rootFS.NewRequestHandler()
	})

	if path == "" || !filepath.IsAbs(path) {
		// extend relative path to absolute path
		hasTrailingSlash := path != "" && (path[len(path)-1] == '/' || path[len(path)-1] == '\\')

		var err error
		path = filepath.FromSlash(path)
		if path, err = filepath.Abs(path); err != nil {
			ctx.Logger().Printf("cannot resolve path %q to absolute file path: %v", path, err)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return
		}
		if hasTrailingSlash {
			path += "/"
		}
	}

	// convert the path to forward slashes regardless the OS in order to set the URI properly
	// the handler will convert back to OS path separator before opening the file
	path = filepath.ToSlash(path)

	ctx.Request.SetRequestURI(path)
	rootFSHandler(ctx)
}

var DefaultOrders = [3]compress.Order{compress.Br, compress.Gzip, compress.Zstd}
var DefaultLevels = [3]compress.Level{compress.BrotliDefaultLevel, compress.GzipDefaultLevel, compress.ZstdDefaultLevel}
var emptyOrders = [3]compress.Order{}
var (
	rootFSOnce sync.Once
	rootFS     = &FS{
		Root:               "",
		AllowEmptyRoot:     true,
		Orders:             [3]compress.Order{compress.Br, compress.Gzip, compress.Zstd},
		Levels:             [3]compress.Level{compress.BrotliDefaultLevel, compress.GzipDefaultLevel, compress.ZstdDefaultLevel},
		GenerateIndexPages: true,
		Compress:           true,
		AcceptByteRange:    true,
	}
	rootFSHandler RequestHandler
)

// ServeFS returns HTTP response containing compressed file contents from the given fs.FS's path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// See also ServeFile.
func ServeFS(ctx *RequestCtx, filesystem fs.FS, path string) {
	f := &FS{
		FS:                 filesystem,
		Root:               "",
		AllowEmptyRoot:     true,
		GenerateIndexPages: true,
		Compress:           true,
		AcceptByteRange:    true,
	}
	handler := f.NewRequestHandler()

	ctx.Request.SetRequestURI(path)
	handler(ctx)
}

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path must not contain '/../' substrings due to security reasons,
// since such paths may refer files outside FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestCtx) []byte

// NewVHostPathRewriter returns path rewriter, which strips slashesCount
// leading slashes from the path and prepends the path with request's host,
// thus simplifying virtual hosting for static files.
//
// Examples:
//
//   - host=foobar.com, slashesCount=0, original path="/foo/bar".
//     Resulting path: "/foobar.com/foo/bar"
//
//   - host=img.aaa.com, slashesCount=1, original path="/images/123/456.jpg"
//     Resulting path: "/img.aaa.com/123/456.jpg"
//
//   - host=img.aaa.com, slashesCount=1, original path="/456.jpg"
//     Resulting path: "/img.aaa.com"
//
//   - host=img.aaa.com/sub, slashesCount=1, original path="/456.jpg"
//     Resulting path: "/invalid-host"
func NewVHostPathRewriter(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := stripLeadingSlashes(ctx.Path(), slashesCount)
		host := ctx.Host()
		if n := bytes.IndexByte(host, '/'); n >= 0 {
			host = nil
		}
		if len(host) == 0 {
			host = strInvalidHost
		}
		b := bytebufferpool.Get()
		b.B = append(b.B, '/')
		b.B = append(b.B, host...)
		b.B = append(b.B, path...)
		ctx.URI().SetPathBytes(b.B)
		bytebufferpool.Put(b)

		return ctx.Path()
	}
}

var strInvalidHost = []byte("invalid-host")

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   - slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   - slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   - slashesCount = 2, original path: "/foo/bar", result: ""
//   - slashesCount = 3, original path: "/foo/bar", result: ""
//   - slashesCount = 1, original path: "/foo", result: ""
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathSlashesStripper(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		return stripLeadingSlashes(ctx.Path(), slashesCount)
	}
}

// NewPathPrefixStripper returns path rewriter, which removes prefixSize bytes
// from the path prefix.
//
// Examples:
//
//   - prefixSize = 0, original path: "/foo/bar", result: "/foo/bar"
//   - prefixSize = 3, original path: "/foo/bar", result: "o/bar"
//   - prefixSize = 7, original path: "/foo/bar", result: "r"
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathPrefixStripper(prefixSize int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := ctx.Path()
		if len(path) >= prefixSize {
			path = path[prefixSize:]
		}
		return path
	}
}

// FS represents settings for request handler serving static files
// from the local filesystem.
//
// It is prohibited copying FS values. Create new values instead.
type FS struct {
	noCopy noCopy

	// FS is filesystem to serve files from. eg: embed.FS os.DirFS
	FS fs.FS

	// Path to the root directory to serve files from.
	Root string

	// List of index file names to try opening during directory access.
	//
	// For example:
	//
	//     * index.html
	//     * index.htm
	//     * my-super-index.xml
	//
	// By default the list is empty.
	IndexNames []string

	// Path to the compressed root directory to serve files from. If this value
	// is empty, Root is used.
	CompressRoot string

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// PathNotFound fires when file is not found in filesystem
	// this functions tries to replace "Cannot open requested path"
	// server response giving to the programmer the control of server flow.
	//
	// By default PathNotFound returns
	// "Cannot open requested path"
	PathNotFound RequestHandler

	// AllowEmptyRoot controls what happens when Root is empty. When false (default) it will default to the
	// current working directory. An empty root is mostly useful when you want to use absolute paths
	// on windows that are on different filesystems. On linux setting your Root to "/" already allows you to use
	// absolute paths on any filesystem.
	AllowEmptyRoot bool

	// Uses brotli encoding and fallbacks to gzip in responses if set to true, uses gzip if set to false.
	//
	// This value has sense only if Compress is set.
	//
	// Brotli encoding is disabled by default.
	//CompressBrotli bool
	Orders [3]compress.Order
	Levels [3]compress.Level

	// Index pages for directories without files matching IndexNames
	// are automatically generated if set.
	//
	// Directory index generation may be quite slow for directories
	// with many files (more than 1K), so it is discouraged enabling
	// index pages' generation for such directories.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Transparently compresses responses if set to true.
	//
	// The server tries minimizing CPU usage by caching compressed files.
	// It adds CompressedFileSuffix suffix to the original file name and
	// tries saving the resulting compressed file under the new file name.
	// So it is advisable to give the server write access to Root
	// and to all inner folders in order to minimize CPU usage when serving
	// compressed responses.
	//
	// Transparent compression is disabled by default.
	Compress bool

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

	// SkipCache if true, will cache no file handler.
	//
	// By default is false.
	SkipCache bool

	// Expiration duration for inactive file handlers.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	// Suffix to add to the name of cached compressed file.
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffix is used by default.
	//CompressedFileSuffix string

	// Suffixes list to add to compressedFileSuffix depending on encoding
	//
	// This value has sense only if Compress is set.
	//
	// DefaultCompressedFileSuffixes is used by default.
	CompressedFileSuffixes [3]string

	// If CleanStop is set, the channel can be closed to stop the cleanup handlers
	// for the FS RequestHandlers created with NewRequestHandler.
	// NEVER close this channel while the handler is still being used!
	CleanStop chan struct{}

	once sync.Once
	h    RequestHandler
	//
	hWithError          RequestHandlerWithError
	NeedCompressFunc    func(path []byte) bool
	NeedCompressSize    int64
	bigFileReaderPool   *bigFileReaderPool
	smallFileReaderPool *smallFileReaderPool
	bigFsFilePool       *fsFilePool
	smallFsFilePool     *fsFilePool
}

// FSCompressedFileSuffix is the suffix FS adds to the original file names
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
//const FSCompressedFileSuffix = ".fasthttp.gz"

// DefaultCompressedFileSuffixes is the suffixes FS adds to the original file names depending on encoding
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
/*
var DefaultCompressedFileSuffixes = map[string]string{
	"gzip": ".fasthttp.gz",
	"br":   ".fasthttp.br",
	"zstd": ".fasthttp.zst",
}
*/
// DefaultCompressedFileSuffixes cache kind2suffix
var DefaultCompressedFileSuffixes = [3]string{
	".fasthttp.br", ".fasthttp.gz", ".fasthttp.zst",
}

// FSHandlerCacheDuration is the default expiration duration for inactive
// file handlers opened by FS.
const FSHandlerCacheDuration = 60 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   - stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   - stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   - stripSlashes = 2, original path: "/foo/bar", result: ""
//
// The returned request handler automatically generates index pages
// for directories without index.html.
//
// The returned handler caches requested file handles
// for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple request handler instances for the same
// (root, stripSlashes) arguments - just reuse a single instance.
// Otherwise, goroutine leak will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	fs_ := &FS{
		Root:               root,
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		AcceptByteRange:    true,
	}
	if stripSlashes > 0 {
		fs_.PathRewrite = NewPathSlashesStripper(stripSlashes)
	}
	return fs_.NewRequestHandler()
}

// NewRequestHandler returns new request handler with the given FS settings.
//
// The returned handler caches requested file handles
// for FS.CacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if FS.Root folder contains many files.
//
// Do not create multiple request handlers from a single FS instance -
// just reuse a single request handler.
func (fs *FS) NewRequestHandler() RequestHandler {
	fs.once.Do(fs.initRequestHandler)
	return fs.h
}

func (fs *FS) NewRequestHandlerWithError() RequestHandlerWithError {
	fs.once.Do(fs.initRequestHandler)
	return fs.hWithError
}

func (fs *FS) normalizeRoot(root string) string {
	// fs.FS uses relative paths, that paths are slash-separated on all systems, even Windows.
	if fs.FS == nil {
		// Serve files from the current working directory if Root is empty or if Root is a relative path.
		if (!fs.AllowEmptyRoot && root == "") || (root != "" && !filepath.IsAbs(root)) {
			path, err := os.Getwd()
			if err != nil {
				path = "."
			}
			root = path + "/" + root
		}

		// convert the root directory slashes to the native format
		root = filepath.FromSlash(root)
	}

	// strip trailing slashes from the root path
	for root != "" && root[len(root)-1] == os.PathSeparator {
		root = root[:len(root)-1]
	}
	return root
}

//var defaultOrders = []

func (fs *FS) initRequestHandler() {
	// 如果 fs.FS 为空:
	// 1. fs.Root是相对路径
	// 2.fs.Root为空且fs.AllowEmptyRoot为false,
	// fs.Root设置为当前目录+fs.Root。否则fs.Root不变。
	//
	// 但无论如何fs.normalizeRoot返回的root，其/都被替换为系统分隔符，并且末尾的分隔符都会被移除(如果有的话)。
	root := fs.normalizeRoot(fs.Root)

	compressRoot := fs.CompressRoot
	if compressRoot == "" {
		compressRoot = root
	} else {
		compressRoot = fs.normalizeRoot(compressRoot)
	}

	compressedFileSuffixes := fs.CompressedFileSuffixes

	h := &fsHandler{
		filesystem:         fs.FS,
		root:               root,
		indexNames:         fs.IndexNames,
		pathRewrite:        fs.PathRewrite,
		generateIndexPages: fs.GenerateIndexPages,
		compress:           fs.Compress,
		compressRoot:       compressRoot,
		pathNotFound:       fs.PathNotFound,
		acceptByteRange:    fs.AcceptByteRange,
		needCompressFunc:   fs.NeedCompressFunc,
		needCompressSize:   fs.NeedCompressSize,
		Orders:             fs.Orders,
		Levels:             fs.Levels,
		fileLock:           &FileLocks{*xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(400))},
	}
	//
	if fs.Orders == emptyOrders {
		h.Orders = DefaultOrders
		h.Levels = DefaultLevels
	}
	//
	if fs.smallFileReaderPool == nil {
		h.smallFileReaderPool = &defaultSmallFileReaderPool
	}
	if fs.smallFsFilePool == nil {
		h.smallFsFilePool = &defaultSmallFsFilePool
	}
	if fs.bigFileReaderPool == nil {
		h.bigFileReaderPool = &defaultBigFileReaderPool
	}
	if fs.bigFsFilePool == nil {
		h.bigFsFilePool = &defaultBigFsFilePool
	}
	//h.cacheManager = newCacheManager(fs)
	cm := newSyncMapCacheManager(fs)
	if cm != nil {
		h.cacheManager = cm
	}

	if h.filesystem == nil {
		h.filesystem = &osFS{} // It provides os.Open and os.Stat
	}
	for i, o := range h.Orders {
		if o == compress.Dump {
			continue
		}
		switch o {
		case compress.Br:
			h.order2kind[i] = brotliCacheKind
			if compressedFileSuffixes[brotliCacheKind-1] == "" {
				h.compressedFileSuffixes[brotliCacheKind] = DefaultCompressedFileSuffixes[brotliCacheKind-1]
			}
			//
			var level = h.Levels[i]
			if level == 0 {
				level = compress.BrotliDefaultLevel
			}
			h.order2pool[i] = compress.Info{
				Pool: compress.Pool(int(level), o),
			}
		case compress.Gzip:
			h.order2kind[i] = gzipCacheKind
			if compressedFileSuffixes[gzipCacheKind-1] == "" {
				h.compressedFileSuffixes[gzipCacheKind] = DefaultCompressedFileSuffixes[gzipCacheKind-1]
			}
			//
			var level = h.Levels[i]
			if level == 0 {
				level = compress.GzipDefaultLevel
			}
			h.order2pool[i] = compress.Info{
				Pool: compress.Pool(int(level), o),
			}
		case compress.Zstd:
			h.order2kind[i] = zstdCacheKind
			if compressedFileSuffixes[zstdCacheKind-1] == "" {
				h.compressedFileSuffixes[zstdCacheKind] = DefaultCompressedFileSuffixes[zstdCacheKind-1]
			}
			//
			//
			var level = h.Levels[i]
			if level == 0 {
				level = compress.ZstdDefaultLevel
			}
			h.order2pool[i] = compress.Info{
				Pool: compress.Pool(int(level), o),
			}
		}
	}
	fs.h = h.handleRequest
	fs.hWithError = h.handleRequestWithError
}

type fsHandler struct {
	// filesystem 字段总是被设置，如果用户没有设置，则会被设置为 &osFS{}
	filesystem fs.FS
	// 如果 filesystem 字段不是 &osFS ,则在服务文件时不会使用 root 字段的值。
	// 如果 root 不为空，则root末尾肯定不包括系统路径分隔符
	//
	// 当 filesystem 字段是 &osFS 时，root 字段不会被使用到
	// TODO fsHandler dont need root， only use filesystem ok. use root to create
	//  filesystem.
	// 服务文件： &osFS + (root + request.Path); filesystem + request.Path
	root                   string
	indexNames             []string
	pathRewrite            PathRewriteFunc
	pathNotFound           RequestHandler
	generateIndexPages     bool
	compress               bool
	acceptByteRange        bool
	compressRoot           string
	compressedFileSuffixes [4]string

	cacheManager cacheManager

	smallFileReaderPool *smallFileReaderPool
	bigFileReaderPool   *bigFileReaderPool
	bigFsFilePool       *fsFilePool
	smallFsFilePool     *fsFilePool
	needCompressFunc    func(path []byte) bool
	needCompressSize    int64
	Levels              [3]compress.Level
	Orders              [3]compress.Order
	order2pool          [3]compress.Info
	order2kind          [3]CacheKind
	fileLock            *FileLocks
}

var DefaultBigFileReaderCount = 400

//var DefaultSmallFileReaderCount = 50

var defaultBigFileReaderPool = bigFileReaderPool{
	sync.Pool{
		New: func() interface{} {
			return &bigFileReader{}
		},
	},
}
var defaultBigFsFilePool = fsFilePool{
	sync.Pool{
		New: func() interface{} {
			rs := make([]unsafe.Pointer, DefaultBigFileReaderCount)
			return &fsFile{
				bigfile:    true,
				normalFile: true,
				c:          uint32(DefaultBigFileReaderCount),
				fileReader: unsafe.Pointer(unsafe.SliceData(rs)),
			}
		},
	},
}
var defaultSmallFileReaderPool = smallFileReaderPool{
	sync.Pool{
		New: func() interface{} {
			return &fsSmallFileReader{}
		},
	},
}
var defaultSmallFsFilePool = fsFilePool{
	sync.Pool{
		New: func() interface{} {
			//rs := make([]unsafe.Pointer, DefaultBigFileReaderCount)
			return &fsFile{}
		},
	},
}

type fsFilePool struct {
	sync.Pool
}

func (fp *fsFilePool) Get() *fsFile {
	return fp.Pool.Get().(*fsFile)
}
func (fp *fsFilePool) Put(ff *fsFile) {
	fp.Pool.Put(ff)
}

type bigFileReaderPool struct {
	sync.Pool
}

func (big *bigFileReaderPool) Get() *bigFileReader {
	return big.Pool.Get().(*bigFileReader)
}
func (big *bigFileReaderPool) Put(r *bigFileReader) {
	r.f = nil
	r.ff = nil
	r.r = nil
	r.lr.R = nil
	big.Pool.Put(r)
}

type smallFileReaderPool struct {
	sync.Pool
}

func (s *smallFileReaderPool) Get() *fsSmallFileReader {
	return s.Pool.Get().(*fsSmallFileReader)
}
func (s *smallFileReaderPool) Put(r *fsSmallFileReader) {
	s.Pool.Put(r)
}

type FileSeeker interface {
	fs.File
	io.Seeker
}

// fsFile for cache
type fsFile struct {
	h             *fsHandler
	f             FileSeeker
	filename      []byte // fs.FileInfo.Name() return filename, isn't filepath.
	dirIndex      []byte
	contentType   string
	contentLength int // dont need reset

	lastModified    time.Time // dont need reset
	lastModifiedStr []byte    //   dont need reset

	t            time.Time    //  dont need reset
	readersCount atomic.Int32 // working bigFileReader / fsSamllFileReader count //need reset.
	c            uint32
	l            uint32 // need reset
	compressed   bool   // dong need reset
	bigfile      bool   // dont need reset
	dir          bool   // dont need reset
	normalFile   bool   // dont need reset
	//bigFiles     []*bigFileReader
	fileReader     unsafe.Pointer // need reset
	fileReaderLock sync.Mutex
	pb             [1]pool.RecycleItemser
	//bigFilesLock sync.Mutex
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.readersCount.Add(-1)
			//ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallFileReader()
}

func (ff *fsFile) smallFileReader() (io.Reader, error) {
	ff.readersCount.Add(1)
	r := ff.h.smallFileReaderPool.Get()
	r.ff = ff
	r.startPos = 0
	r.endPos = ff.contentLength
	return r, nil
}

// Files bigger than this size are sent with sendfile.
const maxSmallFileSize = 2 * 4096

// TODO add a bool in fsHandler, type assert is small expensive
// only for big file and not dir.
func (ff *fsFile) isBig() bool {
	/*
		if _, ok := ff.h.filesystem.(*osFS); !ok { // fs.FS only uses bigFileReader, memory cache uses fsSmallFileReader
			return ff.f != nil
		}
		return ff.contentLength > MaxSmallFileSize && len(ff.dirIndex) == 0
	*/
	return ff.bigfile
}

var OpenedFileNotSeekableErr = errors.New("opened file not seekable")

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	var r *bigFileReader

	ff.readersCount.Add(1)
	ff.fileReaderLock.Lock()
	if ff.l > 0 {
		ff.l--
		r = *(**bigFileReader)(unsafe.Add(ff.fileReader, 8*ff.l))
	}
	ff.fileReaderLock.Unlock()
	if r != nil {
		return r, nil
	}

	f, err := ff.h.filesystem.Open(b2s(ff.filename))
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %w", err)
	}
	f1, ok := f.(FileSeeker)
	if !ok {
		err = OpenedFileNotSeekableErr
	}
	//
	r = ff.h.bigFileReaderPool.Get()
	r.r = f
	r.f = f1
	r.ff = ff
	return r, nil
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		_ = ff.f.Close()
		ff.f = nil
	}
	//
	ff.readersCount.Store(0)
	//
	if len(ff.filename) > 0 {
		ff.filename = ff.filename[:0]
	}
	if len(ff.dirIndex) > 0 {
		ff.dirIndex = ff.dirIndex[:0]
	}
	if ff.pb[0] != nil {
		ff.pb[0].RecycleItems()
	}
	if len(ff.lastModifiedStr) > 0 {
		ff.lastModifiedStr = ff.lastModifiedStr[:0]
	}
	if ff.bigfile {
		ff.fileReaderLock.Lock()
		if ff.bigfile {
			for i := 0; i < int(ff.l); i++ {
				r := *(**bigFileReader)(unsafe.Add(ff.fileReader, i*8))
				if r.f != nil {
					_ = r.f.Close()
				}
				r.f = nil
			}
		}
		ff.l = 0
		ff.fileReaderLock.Unlock()
		ff.h.bigFsFilePool.Put(ff)
	} else {
		ff.h.smallFsFilePool.Put(ff)
	}
	ff.h = nil
}

func (ff *fsFile) decReadersCount() {
	ff.h.cacheManager.WithLock(func() {
		ff.readersCount.Add(-1)
	})
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  FileSeeker       // normally must is os.File
	ff *fsFile          // cache item.
	r  io.Reader        // read content always use this field.
	lr io.LimitedReader // for bytes range read.
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int) error {
	if r.f == nil {
		return errors.New("file must not be nil")
	}
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		return errors.New("must implement io.Seeker")
	}
	if _, err := seeker.Seek(int64(startPos), io.SeekStart); err != nil {
		return err
	}
	r.r = &r.lr
	r.lr.R = r.f
	r.lr.N = int64(endPos - startPos + 1)
	return nil
}
func (r *bigFileReader) FileOrLimitedReader() io.Reader {
	return r.r
}
func (r *bigFileReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *bigFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		// fast path. Send file must be triggered
		return rf.ReadFrom(r.r)
	}

	// slow path
	return copyZeroAlloc(w, r.r)
}

var SeekStartNotZeroErr = errors.New("bug: File.Seek(0, io.SeekStart) returned (0, nil)")

// Close named Release  more properly, if error occur close linked FileSeeker
// , otherwise back to fsFile's bigFileReader slice container.
func (r *bigFileReader) Close() (err error) {
	r.r = r.f
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		_ = r.f.Close()
		r.ff.h.bigFileReaderPool.Put(r)
		r.ff.readersCount.Add(-1)
		return errors.New("must implement io.Seeker")
	}
	//
	n, err := seeker.Seek(0, io.SeekStart)
	if err != nil || n != 0 {
		if n != 0 {
			err = SeekStartNotZeroErr
		}
		_ = r.f.Close()
		r.ff.h.bigFileReaderPool.Put(r)
		r.ff.readersCount.Add(-1)
		return
	}
	//
	ff := r.ff
	ff.fileReaderLock.Lock()
	if ff.l < ff.c {
		*(**bigFileReader)(unsafe.Add(ff.fileReader, ff.l*8)) = r
		ff.l++
	}
	ff.fileReaderLock.Unlock()
	r.ff.readersCount.Add(-1)
	return
}

type fsSmallFileReader struct {
	ff       *fsFile
	startPos int
	endPos   int
}

// Close not close linked FileSeeker.
func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	r.ff = nil
	r.startPos = 0
	r.endPos = 0
	ff.h.smallFileReaderPool.Put(r)
	//ff.decReadersCount()
	ff.readersCount.Add(-1)
	return nil
}

func (r *fsSmallFileReader) UpdateByteRange(startPos, endPos int) error {
	r.startPos = startPos
	r.endPos = endPos + 1
	return nil
}

func (r *fsSmallFileReader) Read(p []byte) (int, error) {
	tailLen := r.endPos - r.startPos
	if tailLen <= 0 {
		return 0, io.EOF
	}
	if len(p) > tailLen {
		p = p[:tailLen]
	}

	ff := r.ff
	if ff.f != nil {
		ra, ok := ff.f.(io.ReaderAt)
		if !ok {
			return 0, errors.New("must implement io.ReaderAt")
		}
		n, err := ra.ReadAt(p, int64(r.startPos))
		r.startPos += n
		return n, err
	}

	n := copy(p, ff.dirIndex[r.startPos:])
	r.startPos += n
	return n, nil
}

func (r *fsSmallFileReader) WriteTo(w io.Writer) (int64, error) {
	ff := r.ff

	var n int
	var err error
	if ff.f == nil {
		n, err = w.Write(ff.dirIndex[r.startPos:r.endPos])
		return int64(n), err
	}

	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	curPos := r.startPos
	py := pool.Get(8192)
	py.B = py.B[:cap(py.B)]
	//bufv := copyBufPool.Get()
	//buf := bufv.([]byte)
	buf := py.B
	for err == nil {
		tailLen := r.endPos - curPos
		if tailLen <= 0 {
			break
		}
		if len(buf) > tailLen {
			buf = buf[:tailLen]
		}
		ra, ok := ff.f.(io.ReaderAt)
		if !ok {
			return 0, errors.New("must implement io.ReaderAt")
		}
		n, err = ra.ReadAt(buf, int64(curPos))
		nw, errw := w.Write(buf[:n])
		curPos += nw
		if errw == nil && nw != n {
			errw = errors.New("bug: Write(p) returned (n, nil), where n != len(p)")
		}
		if err == nil {
			err = errw
		}
	}
	//copyBufPool.Put(bufv)
	py.RecycleToPool00()

	if err == io.EOF {
		err = nil
	}
	return int64(curPos - r.startPos), err
}

type cacheManager interface {
	WithLock(work func())
	// GetFileFromCache path is request path not filepath
	GetFileFromCache(cacheKind CacheKind, path string) (*fsFile, bool)
	SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile
}

func newSyncMapCacheManager(fs *FS) *syncMapCacheManager {
	if fs.SkipCache {
		return nil
	}
	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	return &syncMapCacheManager{
		cacheDuration: cacheDuration,
		caches: [4]*xsync.MapOf[string, *fsFile]{
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(400)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
		},
	}
}

type syncMapCacheManager struct {
	caches        [4]*xsync.MapOf[string, *fsFile]
	cacheDuration time.Duration
}

func (s *syncMapCacheManager) WithLock(work func()) {
	work()
}

func (s *syncMapCacheManager) GetFileFromCache(cacheKind CacheKind, path string) (ff *fsFile, ok bool) {
	ff, ok = s.caches[cacheKind].Load(path)
	if ok {
		ff.readersCount.Add(1)
	}
	return
}

func (s *syncMapCacheManager) SetFileToCache(cacheKind CacheKind, path string, fF *fsFile) (ff *fsFile) {
	ff, ok := s.caches[cacheKind].LoadOrStore(path, fF)
	if ok {
		fF.Release()
	}
	ff.readersCount.Add(1)
	return
}
func (s *syncMapCacheManager) cleanCache(pendingFiles []*fsFile) []*fsFile {
	now := time.Now()
	for _, cache := range s.caches {
		if cache.Size() < cacheCleanThreshold {
			continue
		}
		cache.Range(func(path string, ff *fsFile) bool {
			if now.Sub(ff.t) > s.cacheDuration && ff.readersCount.Load() < 2 {
				pendingFiles = append(pendingFiles, ff)
			}
			cache.Delete(path)
			return true
		})
	}
	return pendingFiles
}
func (s *syncMapCacheManager) handlePendingCache(pendingFiles []*fsFile) {
	for _, ff := range pendingFiles {
		if ff.readersCount.Load() <= 0 {
			ff.Release()
		}
	}
}

func (s *syncMapCacheManager) handleCleanCache(cleanStop chan struct{}) {
	var pendingFiles []*fsFile

	clean := func() {
		pendingFiles = s.cleanCache(pendingFiles)
	}

	if cleanStop != nil {
		t := time.NewTicker(s.cacheDuration / 2)
		for {
			select {
			case <-t.C:
				clean()
				s.handlePendingCache(pendingFiles)
			case _, stillOpen := <-cleanStop:
				// Ignore values send on the channel, only stop when it is closed.
				if !stillOpen {
					t.Stop()
					return
				}
			}
		}
	}
	for {
		time.Sleep(s.cacheDuration / 2)
		s.handlePendingCache(pendingFiles)
		clean()
	}
}

var (
	_ cacheManager = (*inMemoryCacheManager)(nil)
	_ cacheManager = (*noopCacheManager)(nil)
	_ cacheManager = (*syncMapCacheManager)(nil)
)

type CacheKind uint8

var cacheKindToStr = [4]compress.Order{
	compress.Dump,
	compress.Br,
	compress.Gzip,
	compress.Zstd,
}

const (
	defaultCacheKind CacheKind = iota
	brotliCacheKind
	gzipCacheKind
	zstdCacheKind
)

func newCacheManager(fs *FS) cacheManager {
	if fs.SkipCache {
		return &noopCacheManager{}
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	instance := &inMemoryCacheManager{
		cacheDuration: cacheDuration,
		cache:         make(map[string]*fsFile),
		cacheBrotli:   make(map[string]*fsFile),
		cacheGzip:     make(map[string]*fsFile),
	}

	go instance.handleCleanCache(fs.CleanStop)

	return instance
}

type noopCacheManager struct {
	cacheLock sync.Mutex
}

func (n *noopCacheManager) WithLock(work func()) {
	n.cacheLock.Lock()

	work()

	n.cacheLock.Unlock()
}

func (*noopCacheManager) GetFileFromCache(cacheKind CacheKind, path string) (*fsFile, bool) {
	return nil, false
}

func (*noopCacheManager) SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile {
	return ff
}

type inMemoryCacheManager struct {
	cacheDuration time.Duration
	cache         map[string]*fsFile
	cacheBrotli   map[string]*fsFile
	cacheGzip     map[string]*fsFile
	cacheLock     sync.Mutex
}

func (cm *inMemoryCacheManager) WithLock(work func()) {
	cm.cacheLock.Lock()

	work()

	cm.cacheLock.Unlock()
}

func (cm *inMemoryCacheManager) getFsCache(cacheKind CacheKind) (fileCache map[string]*fsFile) {
	switch cacheKind {
	case brotliCacheKind:
		fileCache = cm.cacheBrotli
	case gzipCacheKind:
		fileCache = cm.cacheGzip
	default:
		fileCache = cm.cache
	}

	return fileCache
}

func (cm *inMemoryCacheManager) GetFileFromCache(cacheKind CacheKind, path string) (*fsFile, bool) {
	fileCache := cm.getFsCache(cacheKind)

	cm.cacheLock.Lock()
	ff, ok := fileCache[path]
	if ok {
		ff.readersCount.Add(1)
	}
	cm.cacheLock.Unlock()

	return ff, ok
}

func (cm *inMemoryCacheManager) SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile {
	fileCache := cm.getFsCache(cacheKind)

	cm.cacheLock.Lock()
	ff1, ok := fileCache[path]
	if !ok {
		fileCache[path] = ff
		ff.readersCount.Add(1)
	} else {
		ff1.readersCount.Add(1)
	}
	cm.cacheLock.Unlock()

	if ok {
		// The file has been already opened by another
		// goroutine, so close the current file and use
		// the file opened by another goroutine instead.
		ff.Release()
		ff = ff1
	}

	return ff
}

func (cm *inMemoryCacheManager) handleCleanCache(cleanStop chan struct{}) {
	var pendingFiles []*fsFile

	clean := func() {
		pendingFiles = cm.cleanCache(pendingFiles)
	}

	if cleanStop != nil {
		t := time.NewTicker(cm.cacheDuration / 2)
		for {
			select {
			case <-t.C:
				clean()
			case _, stillOpen := <-cleanStop:
				// Ignore values send on the channel, only stop when it is closed.
				if !stillOpen {
					t.Stop()
					return
				}
			}
		}
	}
	for {
		time.Sleep(cm.cacheDuration / 2)
		clean()
	}
}

func (cm *inMemoryCacheManager) cleanCache(pendingFiles []*fsFile) []*fsFile {
	var filesToRelease []*fsFile

	cm.cacheLock.Lock()

	// Close files which couldn't be closed before due to non-zero
	// readers count on the previous run.
	var remainingFiles []*fsFile
	for _, ff := range pendingFiles {
		if ff.readersCount.Load() > 0 {
			remainingFiles = append(remainingFiles, ff)
		} else {
			filesToRelease = append(filesToRelease, ff)
		}
	}
	pendingFiles = remainingFiles

	pendingFiles, filesToRelease = cleanCacheNoLock(cm.cache, pendingFiles, filesToRelease, cm.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNoLock(cm.cacheBrotli, pendingFiles, filesToRelease, cm.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNoLock(cm.cacheGzip, pendingFiles, filesToRelease, cm.cacheDuration)

	cm.cacheLock.Unlock()

	for _, ff := range filesToRelease {
		ff.Release()
	}

	return pendingFiles
}

func cleanCacheNoLock(
	cache map[string]*fsFile, pendingFiles, filesToRelease []*fsFile, cacheDuration time.Duration,
) ([]*fsFile, []*fsFile) {
	t := time.Now()
	for k, ff := range cache {
		if t.Sub(ff.t) > cacheDuration {
			if ff.readersCount.Load() > 0 {
				// There are pending readers on stale file handle,
				// so we cannot close it. Put it into pendingFiles
				// so it will be closed later.
				pendingFiles = append(pendingFiles, ff)
			} else {
				filesToRelease = append(filesToRelease, ff)
			}
			delete(cache, k)
		}
	}
	return pendingFiles, filesToRelease
}

// for filesystem is not os.FS, strip first slash, become a relative path.
// for filesystem is os.FS, simple join path and root, but replace all forward
// slash to os system separator.
func (h *fsHandler) pathToFilePath(path string) string {
	if _, ok := h.filesystem.(*osFS); !ok {
		if len(path) < 1 {
			return path
		}
		return path[1:]
	}
	return filepath.FromSlash(h.root + path)
}

// filepath for osFS, is absolute path and must begin with h.root,
// handle by above method pathToFilePath.
//
// if fileSystem is not osFS, simple return original.
func (h *fsHandler) filePathToCompressed(filePath string) string {
	if h.root == h.compressRoot {
		return filePath
	}
	if !strings.HasPrefix(filePath, h.root) {
		return filePath
	}
	return filepath.FromSlash(h.compressRoot + filePath[len(h.root):])
}

//goland:noinspection GoDfaNilDereference
func (h *fsHandler) handleRequest(ctx *RequestCtx) {
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
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}
	if h.pathRewrite != nil {
		// There is no need to check for '/../' if path = ctx.Path(),
		// since ctx.Path must normalize and sanitize the path.
		if n := bytes.Index(path, strSlashDotDotSlash); n >= 0 {
			ctx.Logger().Printf("cannot serve path with '/../' at position %d due to security reasons: %q", n, path)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return
		}
	}

	mustCompress := false
	fileCacheKind := defaultCacheKind
	fileEncoding := ""
	byteRange := ctx.Request.Header.peek(strRange)
	tryCompress := false

	pathStr := string(path)

	var compressorIndex int
	if h.needCompressFunc != nil {
		tryCompress = h.needCompressFunc(path)
	} else {
		tryCompress = h.compress
	}
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
	var (
		ff *fsFile
		ok bool
	)
	if h.cacheManager != nil {
		ff, ok = h.cacheManager.GetFileFromCache(fileCacheKind, pathStr)
	}
	if !ok {
		filePath := h.pathToFilePath(pathStr)

		var err error
		ff, err = h.openFSFile(filePath, pathStr, mustCompress, compressorIndex)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, pathStr, mustCompress, compressorIndex)
		}

		if errors.Is(err, errDirIndexRequired) {
			if !hasTrailingSlash {
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				return
			}
			ff, err = h.openIndexFile(ctx, filePath, pathStr, mustCompress, compressorIndex)
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
			}
			return
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
		ctx.Error("Internal Server Error", StatusInternalServerError)
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
			startPos, endPos, err := ParseByteRange(byteRange, contentLength)
			if err != nil {
				_ = r.(io.Closer).Close()
				ctx.Logger().Printf("cannot parse byte range %q for path=%q: %v", byteRange, path, err)
				ctx.Error("Range Not Satisfiable", StatusRequestedRangeNotSatisfiable)
				return
			}

			if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil {
				_ = r.(io.Closer).Close()
				ctx.Logger().Printf("cannot seek byte range %q for path=%q: %v", byteRange, path, err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
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
			if err := rc.Close(); err != nil {
				ctx.Logger().Printf("cannot close file reader: %v", err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
				return
			}
		}
	}
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
}

type byteRangeUpdater interface {
	UpdateByteRange(startPos, endPos int) error
}

// ParseByteRange parses 'Range: bytes=...' header value.
//
// It follows https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35 .
func ParseByteRange(byteRange []byte, contentLength int) (startPos, endPos int, err error) {
	b := byteRange
	if !bytes.HasPrefix(b, strBytes) {
		return 0, 0, fmt.Errorf("unsupported range units: %q. Expecting %q", byteRange, strBytes)
	}

	b = b[len(strBytes):]
	if len(b) == 0 || b[0] != '=' {
		return 0, 0, fmt.Errorf("missing byte range in %q", byteRange)
	}
	b = b[1:]

	n := bytes.IndexByte(b, '-')
	if n < 0 {
		return 0, 0, fmt.Errorf("missing the end position of byte range in %q", byteRange)
	}
	//
	if n == 0 {
		v, err := ParseUint(b[n+1:])
		if err != nil {
			return 0, 0, err
		}
		startPos := contentLength - v
		if startPos < 0 {
			startPos = 0
		}
		return startPos, contentLength - 1, nil
	}

	if startPos, err = ParseUint(b[:n]); err != nil {
		return 0, 0, err
	}
	if startPos >= contentLength {
		return 0, 0, fmt.Errorf("the start position of byte range cannot exceed %d. byte range %q", contentLength-1, byteRange)
	}
	//
	b = b[n+1:]
	if len(b) == 0 {
		return startPos, contentLength - 1, nil
	}

	if endPos, err = ParseUint(b); err != nil {
		return 0, 0, err
	}
	if endPos >= contentLength {
		endPos = contentLength - 1
	}
	if endPos < startPos {
		return 0, 0, fmt.Errorf("the start position of byte range cannot exceed the end position. byte range %q", byteRange)
	}
	return startPos, endPos, nil
}

func (h *fsHandler) openIndexFile(ctx *RequestCtx, dirPath, reqPath string, mustCompress bool, compressorIndex int) (*fsFile, error) {
	for _, indexName := range h.indexNames {
		indexFilePath := indexName
		if dirPath != "" {
			indexFilePath = dirPath + "/" + indexName
		}

		ff, err := h.openFSFile(indexFilePath, reqPath, mustCompress, compressorIndex)
		if err == nil {
			return ff, nil
		}
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", indexFilePath)
			mustCompress = false
			return h.openFSFile(indexFilePath, reqPath, mustCompress, compressorIndex)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cannot open file %q: %w", indexFilePath, err)
		}
	}

	if !h.generateIndexPages {
		return nil, fmt.Errorf("cannot access directory without index page. Directory %q", dirPath)
	}

	return h.createDirIndex(ctx, dirPath, mustCompress, compressorIndex)
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

func (h *fsHandler) createDirIndex(ctx *RequestCtx, dirPath string, mustCompress bool, compressorIndex int) (ff *fsFile, err error) {
	buf := &pbytes.Buffer{}
	base := ctx.URI()
	// io/fs doesn't support ReadDir with empty path.
	if dirPath == "" {
		dirPath = "."
	}

	basePathEscaped := html.EscapeString(string(base.Path()))
	_, _ = fmt.Fprintf(buf, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	_, _ = fmt.Fprintf(buf, "<h1>%s</h1>", basePathEscaped)
	_, _ = fmt.Fprintf(buf, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		_, _ = fmt.Fprintf(buf, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	dirEntries, err := fs.ReadDir(h.filesystem, dirPath)
	if err != nil {
		return nil, err
	}

	fm := make(map[string]fs.FileInfo, len(dirEntries))
	filenames := make([]string, 0, len(dirEntries))
nestedContinue:
	for _, de := range dirEntries {
		name := de.Name()
		for _, cfs := range h.compressedFileSuffixes {
			if cfs == "" {
				continue
			}
			if strings.HasSuffix(name, cfs) {
				// Do not show compressed files on index page.
				continue nestedContinue
			}
		}
		fi, err := de.Info()
		if err != nil {
			ctx.Logger().Printf("cannot fetch information from dir entry %q: %v, skip", name, err)

			continue nestedContinue
		}

		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Strings(filenames)
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fi := fm[name]
		auxStr := "dir"
		className := "dir"
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
			className = "file"
		}
		_, _ = fmt.Fprintf(buf, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	_, _ = fmt.Fprintf(buf, "</ul></body></html>")

	if mustCompress {
		compressPool := h.order2pool[compressorIndex].Pool
		//
		buf2 := pbytes.NewBufferWithSize(buf.Len() / 2)
		w := compressPool.Get()
		w.Reset(buf2)
		_, err = copyZeroAlloc(w, buf)
		err2 := w.Close()
		compressPool.Put(w)
		buf.RecycleItems()
		buf = buf2
		if err == nil {
			err = err2
		}
	}
	if err != nil {
		err = errors.New("handle dir: " + dirPath + err.Error())
		return
	}
	dirIndex := buf.Bytes()
	//
	lastModified := time.Now()
	ff = h.smallFsFilePool.Get()
	ff.normalFile = false
	ff.dir = true
	ff.h = h
	ff.dirIndex = append(ff.dirIndex, dirIndex...)
	ff.contentType = http.MIMETextHTMLCharsetUTF8
	ff.contentLength = len(dirIndex)
	ff.compressed = mustCompress
	ff.lastModified = lastModified
	ff.lastModifiedStr = AppendHTTPDate(ff.lastModifiedStr, lastModified)
	ff.t = lastModified
	ff.pb[0] = buf
	return ff, nil
}

type FileLocks struct {
	xsync.MapOf[string, *sync.Mutex]
}

func (fl *FileLocks) Lock(path string) *sync.Mutex {
	lock, _ := fl.LoadOrStore(path, &sync.Mutex{})
	//lock.Lock()
	return lock
}

/*
func (fl *FileLocks) Unlock(path string) {
	lock, _ := fl.LoadOrStore(path, &sync.Mutex{})
	lock.Unlock()
}
*/

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024 // TODO let configurable
)

// here file path is original path without compressed suffix.
func (h *fsHandler) compressAndOpenFSFile(filePath string, reqPath string, compressorIndex int) (ff *fsFile, err error) {
	f1, err := h.filesystem.Open(filePath)
	if err != nil {
		return
	}

	fileInfo, err := f1.Stat()
	if err != nil {
		_ = f1.Close()
		err = fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
		return
	}

	if fileInfo.IsDir() {
		_ = f1.Close()
		err = errDirIndexRequired
		return
	}
	f, ok := f1.(FileSeeker)
	if !ok {
		err = OpenedFileNotSeekableErr
		return
	}
	if strings.HasSuffix(filePath, h.compressedFileSuffixes[h.order2kind[compressorIndex]]) ||
		fileInfo.Size() > fsMaxCompressibleFileSize || fileInfo.Size() < h.needCompressSize ||
		// TODO is unnecessary, only check suffix.
		!isFileCompressible(f, fsMinCompressRatio) {

		if h.cacheManager == nil {
			return h.newFSFile(f, fileInfo, false, filePath, compressorIndex)
		}

		var ok bool
		// create file cache item fsFile but not compress
		// path 1
		l := h.fileLock.Lock(filePath)
		l.Lock()
		ff, ok = h.cacheManager.GetFileFromCache(h.order2kind[compressorIndex], reqPath)
		if ok {
			l.Unlock()
			return
		}
		ff, err = h.newFSFile(f, fileInfo, false, filePath, compressorIndex)
		if err == nil {
			h.cacheManager.SetFileToCache(h.order2kind[compressorIndex], reqPath, ff)
		}
		l.Unlock()
	}

	// calculator compressed file path, but this function caller has calculated
	// TODO trans a param.
	// this method caller simple link filepath+suffix.
	// but here need call function??.
	compressedFilePath := h.filePathToCompressed(filePath)

	if _, ok := h.filesystem.(*osFS); !ok {
		// not osFS, use fileSystem self.
		// cant use file system.
		/// so fileSystem self should handle currency operation.
		//
		// path 2

		if h.cacheManager == nil {
			return h.newCompressedFSFileCache(f, fileInfo, compressedFilePath, compressorIndex)
		}

		l := h.fileLock.Lock(compressedFilePath)
		l.Lock()
		ff, ok = h.cacheManager.GetFileFromCache(h.order2kind[compressorIndex], reqPath)
		if ok {
			l.Unlock()
			return
		}
		ff, err = h.newCompressedFSFileCache(f, fileInfo, compressedFilePath, compressorIndex)
		if err == nil {
			h.cacheManager.SetFileToCache(h.order2kind[compressorIndex], reqPath, ff)
		}
		l.Unlock()
		return
	}
	// compressedFile's dir is not exists, create compressed file.
	if compressedFilePath != filePath {
		if err = os.MkdirAll(filepath.Dir(compressedFilePath), os.ModePerm); err != nil {
			return
		}
	}
	compressedFilePath += h.compressedFileSuffixes[h.order2kind[compressorIndex]]

	//absPath, err := filepath.Abs(compressedFilePath)
	/*
		if err != nil {
			_ = f.Close()
			err = fmt.Errorf("cannot determine absolute path for %q: %v", compressedFilePath, err)
			return
		}
	*/
	l := h.fileLock.Lock(compressedFilePath)
	l.Lock()
	if h.cacheManager == nil {
		ff, err = h.compressFileNoLock(f, fileInfo, filePath, compressedFilePath, reqPath, compressorIndex)
		l.Unlock()
		return
	}
	// read also need get this lock.
	ff, ok = h.cacheManager.GetFileFromCache(h.order2kind[compressorIndex], reqPath)
	if ok {
		l.Unlock()
		return
	}
	ff, err = h.compressFileNoLock(f, fileInfo, filePath, compressedFilePath, reqPath, compressorIndex)
	if err == nil {
		h.cacheManager.SetFileToCache(h.order2kind[compressorIndex], reqPath, ff)
	}
	l.Unlock()
	return ff, err
}

func (h *fsHandler) compressFileNoLock(
	f FileSeeker, fileInfo fs.FileInfo, filePath, compressedFilePath, reqPath string, compressorIndex int,
) (ff *fsFile, err error) {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err = os.Stat(compressedFilePath); err == nil {
		_ = f.Close()
		return h.newCompressedFSFile(compressedFilePath, reqPath, compressorIndex)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		_ = f.Close()
		if !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("cannot create temporary file %q: %w", tmpFilePath, err)
		}
		// TODO is necessary  reenter.
		return nil, errNoCreatePermission
	}
	var compressPool = h.order2pool[compressorIndex].Pool
	w := compressPool.Get()
	w.Reset(zf)
	_, err = copyZeroAlloc(w, f)
	err = w.Close()
	compressPool.Put(w)
	//
	_ = zf.Close()
	_ = f.Close()
	//
	if err != nil {
		return nil, fmt.Errorf("error when compressing file %q to %q: %w", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		return nil, fmt.Errorf("cannot change modification time to %v for tmp file %q: %v",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		return nil, fmt.Errorf("cannot move compressed file from %q to %q: %w", tmpFilePath, compressedFilePath, err)
	}
	//
	return h.newCompressedFSFile(compressedFilePath, reqPath, compressorIndex)
}

// newCompressedFSFileCache use memory cache compressed files.
// filepath is compressed file with suffix.
func (h *fsHandler) newCompressedFSFileCache(f FileSeeker, fileInfo fs.FileInfo, filePath string, compressorIndex int) (ff *fsFile, err error) {
	//
	buf := pbytes.NewBufferWithSize(int(fileInfo.Size()) / 2)
	var compressPool = h.order2pool[compressorIndex].Pool
	defer func() { _ = f.Close() }()
	//
	if compressPool != nil {
		w := compressPool.Get()
		w.Reset(buf)
		_, err = copyZeroAlloc(w, f)
		err = w.Close()
		compressPool.Put(w)
	}
	//
	if err != nil {
		buf.RecycleItems()
		_ = f.Close()
		return nil, fmt.Errorf("error when compressing file %q: %w", filePath, err)
	}

	seeker, ok := f.(io.Seeker)
	if !ok {
		buf.RecycleItems()
		_ = f.Close()
		return nil, errors.New("not implemented io.Seeker")
	}
	if _, err = seeker.Seek(0, io.SeekStart); err != nil {
		buf.RecycleItems()
		_ = f.Close()
		return nil, err
	}
	//
	ext := fileExtension(fileInfo.Name(), false, h.compressedFileSuffixes[h.order2kind[compressorIndex]])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, false, string(h.Orders[compressorIndex]))
		if err != nil {
			buf.RecycleItems()
			_ = f.Close()
			return nil, fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = ghttp.DetectContentType(data)
	}
	lastModified := fileInfo.ModTime()
	ff = h.smallFsFilePool.Get()
	ff.h = h
	ff.dir = false
	ff.normalFile = false
	ff.dirIndex = buf.Bytes()
	ff.contentType = contentType
	ff.contentLength = len(ff.dirIndex)
	ff.compressed = true
	ff.lastModified = lastModified
	ff.lastModifiedStr = AppendHTTPDate(ff.lastModifiedStr, lastModified)
	ff.t = time.Now()
	_ = f.Close()
	ff.pb[0] = buf
	return
}

const modifiedStale = time.Second * 2
const cacheCleanThreshold = 200

// filePath is compressed version. with compressed suffix.
// fileEncoding is gzip zstd br
func (h *fsHandler) newCompressedFSFile(filePath, reqPath string, compressorIndex int) (ff *fsFile, err error) {
	f1, err := h.filesystem.Open(filePath)
	if err != nil {
		err = fmt.Errorf("cannot open compressed file %q: %w", filePath, err)
		return
	}
	f, ok := f1.(FileSeeker)
	if !ok {
		err = OpenedFileNotSeekableErr
		return
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		err = fmt.Errorf("cannot obtain info for compressed file %q: %w", filePath, err)
		return
	}
	return h.newFSFile(f, fileInfo, true, filePath, compressorIndex)
}

func (h *fsHandler) openFSFile(filePath, reqPath string, mustCompress bool, compressorIndex int) (ff *fsFile, err error) {
	filePathOriginal := filePath
	if mustCompress {
		// TODO simple link is OK?
		filePath += h.compressedFileSuffixes[h.order2kind[compressorIndex]]
	}
	//
	f1, err := h.filesystem.Open(filePath)
	if err != nil {
		if mustCompress && errors.Is(err, fs.ErrNotExist) {
			// Path1 compressed file doesnt exits.
			return h.compressAndOpenFSFile(filePathOriginal, reqPath, compressorIndex)
		}

		// If the file is not found and the path is empty, let's return errDirIndexRequired error.
		if filePath == "" && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid)) {
			return nil, errDirIndexRequired
		}

		return nil, err
	}
	//
	fileInfo, err := f1.Stat()
	if err != nil {
		_ = f1.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}
	if fileInfo.IsDir() {
		_ = f1.Close()
		if mustCompress {
			return nil, fmt.Errorf("directory with unexpected suffix found: %q. Suffix: %q",
				filePath, h.compressedFileSuffixes[h.order2kind[compressorIndex]])
		}
		return nil, errDirIndexRequired
	}
	//
	f, ok := f1.(FileSeeker)
	if !ok {
		err = OpenedFileNotSeekableErr
		return
	}
	//
	if mustCompress {
		fileInfoOriginal, err := fs.Stat(h.filesystem, filePathOriginal)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cannot obtain info for original file %q: %w", filePathOriginal, err)
		}

		// Only re-create the compressed file if there was more than a second between the mod times.
		// On macOS the gzip seems to truncate the nanoseconds in the mod time causing the original file
		// to look newer than the gzipped file.
		if fileInfoOriginal.ModTime().Sub(fileInfo.ModTime()) >= modifiedStale {
			// The compressed file became stale. Re-create it.
			_ = f.Close()
			_ = os.Remove(filePath)
			return h.compressAndOpenFSFile(filePathOriginal, reqPath, compressorIndex)
		}
	}

	// here compress file exist and is not stale
	//
	var cacheKind = defaultCacheKind
	if mustCompress {
		cacheKind = h.order2kind[compressorIndex]
	}

	l := h.fileLock.Lock(filePath)
	l.Lock()
	ff, ok = h.cacheManager.GetFileFromCache(cacheKind, reqPath)
	if ok {
		l.Unlock()
		return
	}
	ff, err = h.newFSFile(f, fileInfo, false, filePath, compressorIndex)
	if err == nil {
		h.cacheManager.SetFileToCache(cacheKind, reqPath, ff)
	}
	l.Unlock()
	//return h.newFSFile(f, fileInfo, mustCompress, filePath, compressorIndex)
	return
}

// here file path is compressed filepath with suffix.
func (h *fsHandler) newFSFile(f FileSeeker, fileInfo fs.FileInfo, compressed bool, filePath string, compressorIndex int) (ff *fsFile, err error) {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		_ = f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed, h.compressedFileSuffixes[h.order2kind[compressorIndex]])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, compressed, string(h.Orders[compressorIndex]))
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = ghttp.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()

	if contentLength > maxSmallFileSize {
		ff = h.bigFsFilePool.Get()
		if !compressed {
			b := h.bigFileReaderPool.Get()
			b.f = f
			b.ff = ff
			b.r = f
			*(**bigFileReader)(ff.fileReader) = b
			ff.l++
		} else {
			_ = f.Close() // compressed file flush to disk
		}
		ff.f = nil
	} else {
		ff = h.smallFsFilePool.Get()
		ff.normalFile = true
		ff.f = f
	}
	//
	ff.h = h
	ff.filename = append(ff.filename, filePath...)
	ff.contentType = contentType
	ff.contentLength = contentLength
	ff.compressed = compressed
	ff.lastModified = lastModified
	ff.lastModifiedStr = AppendHTTPDate(ff.lastModifiedStr, lastModified)
	ff.t = time.Now()
	//
	return
}

func readFileHeader(f io.Reader, compressed bool, fileEncoding string) ([]byte, error) {
	r := f
	var (
		br  *brotli.Reader
		zr  *gzip.Reader
		zsr *zstd.Decoder
	)
	if compressed {
		var err error
		switch fileEncoding {
		case "br":
			if br, err = acquireBrotliReader(f); err != nil {
				return nil, err
			}
			r = br
		case "gzip":
			if zr, err = acquireGzipReader(f); err != nil {
				return nil, err
			}
			r = zr
		case "zstd":
			if zsr, err = acquireZstdReader(f); err != nil {
				return nil, err
			}
			r = zsr
		}
	}

	lr := &io.LimitedReader{
		R: r,
		N: 512,
	}
	data, err := io.ReadAll(lr)
	seeker, ok := f.(io.Seeker)
	if !ok {
		return nil, errors.New("must implement io.Seeker")
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	if br != nil {
		releaseBrotliReader(br)
	}

	if zr != nil {
		releaseGzipReader(zr)
	}

	if zsr != nil {
		releaseZstdReader(zsr)
	}

	return data, err
}

func stripLeadingSlashes(path []byte, stripSlashes int) []byte {
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			// developer sanity-check
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}
	return path
}

func stripTrailingSlashes(path []byte) []byte {
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func fileExtension(path string, compressed bool, compressedFileSuffix string) string {
	if compressed && strings.HasSuffix(path, compressedFileSuffix) {
		path = path[:len(path)-len(compressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

// FileLastModified returns last modified time for the file.
func FileLastModified(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return zeroTime, err
	}
	fileInfo, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return zeroTime, err
	}
	return fsModTime(fileInfo.ModTime()), nil
}

func fsModTime(t time.Time) time.Time {
	return t.In(time.UTC).Truncate(time.Second)
}

var filesLockMap = xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(200))

func getFileLock(absPath string) (filelock *sync.Mutex) {
	filelock, _ = filesLockMap.LoadOrStore(absPath, &sync.Mutex{})
	return
}

var _ fs.FS = (*osFS)(nil)

type osFS struct{}

func (o *osFS) Open(name string) (fs.File, error)     { return os.Open(name) }
func (o *osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

var ErrNotAllowedDirIndex = errors.New("want visit dir")
var ErrFileNotFound = errors.New("opened file not found")
