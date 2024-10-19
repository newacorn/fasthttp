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
)

// MaxSmallFileSize Files bigger than this size are sent with sendfile.
var MaxSmallFileSize int64 = 2 * 4096

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
		pb := pool.Get(128)
		buf := pb.B[:0]
		buf = append(buf, '/')
		buf = append(buf, host...)
		buf = append(buf, path...)
		ctx.URI().SetPathBytes(buf)
		pb.RecycleToPool00()

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
	// test only
	fh *fsHandler
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
	// If `fs.FS` is empty, either of the following two conditions will
	// convert `root` to an absolute path relative to the current directory:
	// 1.`fs.Root` is a relative path.
	// 2.If `fs.Root` is empty and `fs.AllowEmptyRoot` is `false`,
	//
	// `fs.Root` is set to the current directory plus `fs.Root`. Otherwise, `fs.Root` remains unchanged.
	//
	// However, regardless of the case, the `fs.normalizeRoot` function returns a `root` where `/`
	// is replaced with the system's path separator, and any trailing separators are removed (if present).
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
		fileLocks: [4]*FileLocks{{*xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(400))},
			{*xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(400))},
			{*xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(400))},
			{*xsync.NewMapOf[string, *sync.Mutex](xsync.WithPresize(400))},
		},
	}
	fs.fh = h
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
	//
	cm := newSyncMapCacheManager(fs)
	if cm != nil {
		h.cacheManager = cm
	}

	if h.filesystem == nil {
		h.filesystem = &osFS{} // It provides os.Open and os.Stat mehods.
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
			h.cacheKind2Pool[brotliCacheKind] = compress.Pool(int(level), o)
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
			h.cacheKind2Pool[gzipCacheKind] = compress.Pool(int(level), o)
		case compress.Zstd:
			h.order2kind[i] = zstdCacheKind
			if compressedFileSuffixes[zstdCacheKind-1] == "" {
				h.compressedFileSuffixes[zstdCacheKind] = DefaultCompressedFileSuffixes[zstdCacheKind-1]
			}
			//
			var level = h.Levels[i]
			if level == 0 {
				level = compress.ZstdDefaultLevel
			}
			h.cacheKind2Pool[zstdCacheKind] = compress.Pool(int(level), o)
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
	root string
	// List of indexed filenames in the directory.
	indexNames []string
	// Rewrite the request path to serve as a component of the file path.
	pathRewrite  PathRewriteFunc
	pathNotFound RequestHandler
	// Generate index content for the directory.
	generateIndexPages bool
	// Determine whether to compress the requested file.
	compress bool
	// Determine whether Range requests are supported.
	acceptByteRange bool
	// Directory for storing compressed file texts.
	compressRoot string
	// Mapping of `cacheKind` to corresponding compressed file extensions.
	compressedFileSuffixes [4]string
	// Cache container
	cacheManager cacheManager

	// Cache pool for `fsSmallFileReader` instances
	smallFileReaderPool *smallFileReaderPool
	// Cache pool for `bigFileReader` instances
	bigFileReaderPool *bigFileReaderPool
	// Cache pool for `fsFile` instances of large files
	bigFsFilePool *fsFilePool
	// Cache pool for `sFile` instances of small files
	smallFsFilePool *fsFilePool
	// Determine whether to enable compression based on the request path.
	needCompressFunc func(path []byte) bool
	// Compression starting point based on file size
	needCompressSize int64
	// Mapping of order to compression levels
	Levels [3]compress.Level
	// Compression type priority includes Brotli (br), Zstandard (zstd), and gzip.
	Orders [3]compress.Order
	// Mapping of `order` to `cacheKind`
	order2kind [3]CacheKind
	// Mapping of `cacheKind` to corresponding compression pools
	cacheKind2Pool [4]compress.Pooler
	// Mapping of `cacheKind` to lock containers
	fileLocks [4]*FileLocks
}

// NewBigFileReaderPool count is File copy limit
//
//goland:noinspection GoExportedFuncWithUnexportedType
func NewBigFileReaderPool(count int) *fsFilePool {
	return &fsFilePool{
		sync.Pool{
			New: func() interface{} {
				rs := make([]unsafe.Pointer, count)
				return &fsFile{
					bigfile:    true,
					normalFile: true,
					c:          uint32(DefaultBigFileReaderCount),
					fileReader: unsafe.Pointer(unsafe.SliceData(rs)),
				}
			},
		},
	}
}

var DefaultBigFileReaderCount = 400

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
	// Reset the associated file resources because they are no longer in use.
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

// FileSeeker The request file type must implement this interface.
// Excludes directory index content
type FileSeeker interface {
	fs.File
	io.Seeker
}

// fsFile is item with information for request file.
type fsFile struct {
	h *fsHandler
	// The `io.Seekable` version of the opened file
	f FileSeeker
	// fs.FileInfo.Name() return filename, isn't filepath.
	filename         []byte
	originalFileName []byte
	// Directory index content
	// When the filesystem is not of type osFS{}, it refers to the compressed version of the stored files.
	dirIndex      []byte
	contentType   string
	contentLength int64 // dont need reset

	lastModified    time.Time // dont need reset
	lastModifiedStr []byte    //   dont need reset

	t time.Time //  don't need to reset
	// The current number of readers for this file
	readersCount atomic.Int32 // working bigFileReader / fsSamllFileReader count //need reset.
	// The maximum limit for file copies
	c uint32
	// The current number of cached copies of the file
	l uint32 // need reset
	// This entry represents a compressed version of a file.
	compressed bool // dong need reset
	// Is it a large file?
	bigfile bool // don't need to reset
	// Is it a directory?
	dir bool // don't need to reset
	// Is it a large file or a small file?
	normalFile bool // don't need to reset
	//bigFiles     []*bigFileReader
	// Starting address of the file copies list. Used only for large files
	//
	// Use smallFileReader for small files and directory index content.
	fileReader unsafe.Pointer // need reset
	// Used only for large files
	fileReaderLock sync.Mutex
	// Cache directory index content and the compressed content of files under non-`osFS{}` type filesystems.
	pb [1]RecycleItemser
}

type RecycleItemser interface {
	RecycleItems()
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.readersCount.Add(-1)
		}
		return r, err
	}
	return ff.smallFileReader()
}

func (ff *fsFile) smallFileReader() (io.Reader, error) {
	//ff.readersCount.Add(1)
	r := ff.h.smallFileReaderPool.Get()
	r.ff = ff
	r.startPos = 0
	r.endPos = int(ff.contentLength)
	return r, nil
}

// TODO add a bool in fsHandler, type assert is small expensive
// only for big file and not dir.
func (ff *fsFile) isBig() bool {
	return ff.bigfile
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	var r *bigFileReader

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
		return nil, errors.New("fasthttp Fs Serve: opening new big file copy error occur: " + err.Error())
	}
	f1, ok := f.(FileSeeker)
	if !ok {
		err = FsOpenedFileNotSeekableErr
	}
	//
	r = ff.h.bigFileReaderPool.Get()
	r.r = f
	r.f = f1
	r.ff = ff
	return r, nil
}

func (ff *fsFile) release() {
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
	if len(ff.originalFileName) > 0 {
		ff.originalFileName = ff.originalFileName[:0]
	}
	if len(ff.dirIndex) > 0 {
		// We don't cache `dirIndex` because it is used relatively infrequently.
		ff.dirIndex = nil
	}
	if ff.pb[0] != nil {
		// Recycle the memory used by `dirIndex`.
		ff.pb[0].RecycleItems()
	}
	if len(ff.lastModifiedStr) > 0 {
		ff.lastModifiedStr = ff.lastModifiedStr[:0]
	}
	var p *fsFilePool
	if ff.bigfile {
		p = ff.h.bigFsFilePool
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
		ff.h = nil
		p.Put(ff)
	} else {
		p = ff.h.smallFsFilePool
		ff.h = nil
		p.Put(ff)
	}
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  FileSeeker       // normally must is os.File
	ff *fsFile          // cache item.
	r  io.Reader        // read content always use this field.
	lr io.LimitedReader // for bytes range read.
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int64) error {
	if r.f == nil {
		return errors.New("fasthttp Fs serve: file must not be nil when updateByteRange")
	}
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		return errors.New("fasthttp Fs serve: file must implement io.Seeker")
	}
	if _, err := seeker.Seek(int64(startPos), io.SeekStart); err != nil {
		return err
	}
	r.r = &r.lr
	r.lr.R = r.f
	r.lr.N = endPos - startPos + 1
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
		// fast path. Use the `sendfile` system call
		return rf.ReadFrom(r.r)
	}
	// slow path
	return copyZeroAlloc(w, r.r)
}

// Close named Release  more properly, if error occur close linked FileSeeker
// , otherwise back to fsFile's bigFileReader slice container.
func (r *bigFileReader) Close() (err error) {
	r.r = r.f
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		_ = r.f.Close()
		r.ff.h.bigFileReaderPool.Put(r)
		r.ff.readersCount.Add(-1)
		return FsOpenedFileNotSeekableErr
	}
	//
	n, err := seeker.Seek(0, io.SeekStart)
	if err != nil || n != 0 {
		if n != 0 {
			err = FsSeekStartNotZeroErr
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

// Unlike large file reads, use non-seeking read methods for small files.
type fsSmallFileReader struct {
	ff *fsFile
	// Serve Range requests
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
		// Read without changing the file cursor
		ra, ok := ff.f.(io.ReaderAt)
		if !ok {
			return 0, errors.New("must implement io.ReaderAt")
		}
		n, err := ra.ReadAt(p, int64(r.startPos))
		r.startPos += n
		return n, err
	}
	// Read the directory index content or the compressed version of files under a non-`os.FS` filesystem.
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

	ca := &syncMapCacheManager{
		cacheDuration: cacheDuration,
		caches: [4]*xsync.MapOf[string, *fsFile]{
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(400)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
			xsync.NewMapOf[string, *fsFile](xsync.WithPresize(200)),
		},
		t: time.NewTicker(cacheDuration / 2),
	}
	go ca.handleCleanCache(fs.CleanStop)
	return ca
}

type syncMapCacheManager struct {
	caches        [4]*xsync.MapOf[string, *fsFile]
	cacheDuration time.Duration
	t             *time.Ticker
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
		// If a copy already exists in the cache, release the one you created.
		fF.release()
	}
	ff.readersCount.Add(1)
	return
}

func (s *syncMapCacheManager) cleanCache(pfs *pendingFiles, idlePfs *pendingFiles) {
	now := time.Now()
	for _, cache := range s.caches {
		cache.Range(func(path string, ff *fsFile) bool {
			fSys, ok := ff.h.filesystem.(*osFS)
			if !ok {
				// Handling of file cache entries for non-writable files.
				return true
			}
			// For writable filesystems, we check if the modification time has expired.
			if ff.dir {
				if now.Sub(ff.t) > s.cacheDuration {
					goto Delete
				}
				return true
			}
			{
				// writeable file system.
				fi, err := fSys.Stat(b2s(ff.originalFileName))
				if err == nil {
					if fi.ModTime().Sub(ff.lastModified) < modifiedStale {
						// cache is fresh.
						return true
						// original may delete.
					}
				}
				// cache file is stale.
			}

		Delete:
			c := idlePfs.shift()
			if c == nil {
				c = &fileChain{v: ff}
			} else {
				c.v = ff
			}
			//
			pfs.unShift(c)
			cache.Delete(path)
			return true
		})
	}
}

func (s *syncMapCacheManager) handlePendingCache(pfs *pendingFiles, idlePfs *pendingFiles) {
	prev := pfs.head
	if prev == nil {
		return
	}
	for {
		cur := prev.next
		if cur == nil {
			break
		}
		if cur.v.readersCount.Load() == 0 {
			prev.next = cur.next
			cur.v.release()
			cur.v = nil
			cur.next = nil
			idlePfs.unShift(cur)
		}
	}
	//
	if pfs.head.v.readersCount.Load() == 0 {
		pfs.head.v.release()
		pfs.head.next = nil
		pfs.head.v = nil
		idlePfs.unShift(pfs.head)
		pfs.head = nil
	}
}

type pendingFiles struct {
	head *fileChain
}

func (p *pendingFiles) unShift(chain *fileChain) {
	chain.next = p.head
	p.head = chain
}

func (p *pendingFiles) size() int {
	if p.head == nil {
		return 0
	}
	prv := p.head
	var i int
	for {
		if prv == nil {
			break
		}
		i++
		prv = prv.next
	}
	return i
}

func (p *pendingFiles) shift() *fileChain {
	if p.head == nil {
		return nil
	}
	chain := p.head
	p.head = chain.next
	chain.next = nil
	return chain
}

var fileChainPool = sync.Pool{}

type fileChain struct {
	v    *fsFile
	next *fileChain
}

var p1 *pendingFiles
var p2 *pendingFiles

func (s *syncMapCacheManager) handleCleanCache(stopChan chan struct{}) {
	var pfs = &pendingFiles{}
	var idlePfs = &pendingFiles{}
	p1, p2 = pfs, idlePfs

	if stopChan != nil {
		for {
			select {
			case <-s.t.C:
				s.cleanCache(pfs, idlePfs)
				s.handlePendingCache(pfs, idlePfs)
			case _, stillOpen := <-stopChan:
				// Ignore values send on the channel, only stop when it is closed.
				if !stillOpen {
					s.t.Stop()
					return
				}
			}
		}
	}
	//
	for {
		<-s.t.C
		s.cleanCache(pfs, idlePfs)
		s.handlePendingCache(pfs, idlePfs)
	}
}

var (
	_ cacheManager = (*inMemoryCacheManager)(nil)
	_ cacheManager = (*noopCacheManager)(nil)
	_ cacheManager = (*syncMapCacheManager)(nil)
)

type CacheKind uint8

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
		t:             time.NewTicker(cacheDuration / 2),
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
	t             *time.Ticker
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
		ff.release()
		ff = ff1
	}

	return ff
}

func (cm *inMemoryCacheManager) handleCleanCache(stopChan chan struct{}) {
	var pendingFiles []*fsFile

	clean := func() {
		pendingFiles = cm.cleanCache(pendingFiles)
	}

	if stopChan != nil {
		for {
			select {
			case <-cm.t.C:
				clean()
			case _, stillOpen := <-stopChan:
				// Ignore values send on the channel, only stop when it is closed.
				if !stillOpen {
					cm.t.Stop()
					return
				}
			}
		}
	}
	//
	for {
		<-cm.t.C
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
		ff.release()
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
	//
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
	hasTrailingSlash := len(path) > 0 && path[len(path)-1] == '/'
	//
	var (
		mustCompress  = h.compress
		fileCacheKind CacheKind
		fileEncoding  string
	)
	originalPathStr := string(path)
	pathStr := originalPathStr
	if hasTrailingSlash {
		pathStr = originalPathStr[:len(originalPathStr)-1]
	}
	//pathStr := string(path)
	byteRange := ctx.Request.Header.peek(strRange)
	if mustCompress && len(byteRange) == 0 {
		if h.needCompressFunc != nil {
			mustCompress = h.needCompressFunc(path)
		}
	} else {
		mustCompress = false
	}
	//
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
		if fileEncoding == "" {
			mustCompress = false
		}
	}
	//
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

		var err error
		ff, err = h.openFSFile(filePath, originalPathStr, hasTrailingSlash, mustCompress, fileCacheKind)
		if err != nil {
			if err == errDirIndexRequired {
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				return
			}
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			//
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
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
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %v", path, err)
		ctx.Error("Internal Server Error", StatusInternalServerError)
		return
	}
	//
	hdr := &ctx.Response.Header
	if ff.compressed {
		hdr.SetContentEncodingBytes(s2b(fileEncoding))
	}
	//
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
	//
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
	//
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
}

type byteRangeUpdater interface {
	UpdateByteRange(startPos, endPos int64) error
}

// ParseByteRange parses 'Range: bytes=...' header value.
//
// It follows https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35 .
func ParseByteRange(byteRange []byte, contentLength int64) (startPos, endPos int64, err error) {
	b := byteRange
	if !bytes.HasPrefix(b, strBytes) {
		err = fmt.Errorf("unsupported range units: %q. Expecting %q", byteRange, strBytes)
		return
	}

	b = b[len(strBytes):]
	if len(b) == 0 || b[0] != '=' {
		err = fmt.Errorf("missing byte range in %q", byteRange)
		return
	}
	b = b[1:]

	n := bytes.IndexByte(b, '-')
	if n < 0 {
		err = fmt.Errorf("missing the end position of byte range in %q", byteRange)
		return
	}
	//
	var v int64
	if n == 0 {
		v, err = ParseUint(b[n+1:])
		if err != nil {
			return
		}
		startPos = contentLength - v
		if startPos < 0 {
			startPos = 0
		}
		endPos = contentLength - 1
		return
	}

	if startPos, err = ParseUint(b[:n]); err != nil {
		return
	}
	if startPos >= contentLength {
		err = fmt.Errorf("the start position of byte range cannot exceed %d. byte range %q", contentLength-1, byteRange)
		return
	}
	//
	b = b[n+1:]
	if len(b) == 0 {
		endPos = contentLength - 1
		return
	}

	if endPos, err = ParseUint(b); err != nil {
		return
	}
	if endPos >= contentLength {
		endPos = contentLength - 1
	}
	if endPos < startPos {
		err = fmt.Errorf("the start position of byte range cannot exceed the end position. byte range %q", byteRange)
		return
	}
	return
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

// dirPath is a dir string with trailer slash.
func (h *fsHandler) createDirIndex(dirPath, reqPath string, mustCompress bool, cacheKind CacheKind) (ff *fsFile, err error) {
	buf := &pbytes.Buffer{}
	base := reqPath
	// io/fs doesn't support ReadDir with empty path.
	if dirPath == "" {
		dirPath = "."
	}

	basePathEscaped := html.EscapeString(base)
	_, _ = fmt.Fprintf(buf, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	_, _ = fmt.Fprintf(buf, "<h1>%s</h1>", basePathEscaped)
	_, _ = fmt.Fprintf(buf, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		parentURI.Update(base + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		_, _ = fmt.Fprintf(buf, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	dirEntries, err := fs.ReadDir(h.filesystem, dirPath)
	if err != nil {
		return
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
		fi, err1 := de.Info()
		if err1 != nil {
			// cannot fetch information from dir entry
			// Ignore this type of error.
			continue nestedContinue
		}

		fm[name] = fi
		filenames = append(filenames, name)
	}
	//
	var u URI
	u.Update(base + "/")

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
		compressPool := h.cacheKind2Pool[cacheKind]
		//
		buf2 := pbytes.NewBufferWithSize((buf.Len() * 3) >> 2)
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
		err = errors.New("Fs serve: handle dir: " + dirPath + err.Error())
		return
	}
	dirIndex := buf.Bytes()
	//
	lastModified := time.Now()
	ff = h.smallFsFilePool.Get()
	ff.normalFile = false
	ff.dir = true
	ff.h = h
	ff.dirIndex = dirIndex
	ff.contentType = http.MIMETextHTMLCharsetUTF8
	ff.contentLength = int64(len(dirIndex))
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

func (fl *FileLocks) Locker(path string) *sync.Mutex {
	lock, _ := fl.LoadOrStore(path, &sync.Mutex{})
	return lock
}

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024 // TODO let configurable
)

// here file path is original path without compressed suffix.
func (h *fsHandler) compressAndOpenFSFile(f FileSeeker, fileInfo fs.FileInfo, compressedFilePath string, inPlaceCompress bool, cacheKind CacheKind) (ff *fsFile, err error) {
	if _, ok := h.filesystem.(*osFS); !ok {
		// not osFS, use fileSystem self.
		return h.newCompressedFSFileCache(f, fileInfo, compressedFilePath, cacheKind)
	}
	// compressedFile's dir is not exists, create compressed file.
	if !inPlaceCompress {
		err = os.MkdirAll(filepath.Dir(compressedFilePath), os.ModePerm)
		if err != nil {
			err = FsWhenCompressErr{
				Path:  compressedFilePath,
				error: err,
			}
			return
		}
	}
	//
	compressedFilePath += h.compressedFileSuffixes[cacheKind]

	var compressedFileInfo fs.FileInfo
	// TODO can reuse compressedFileInfo ?
	compressedFileInfo, err = os.Stat(compressedFilePath)
	if err == nil {
		// check compressed file is stale.
		if fileInfo.ModTime().Sub(compressedFileInfo.ModTime()) < modifiedStale {
			_ = f.Close()
			// is fresh open it.
			return h.newCompressedFSFile(compressedFilePath, cacheKind)
		}
		// is stale delete it
		_ = os.Remove(compressedFilePath)
	}
	//
	ff, err = h.compressFileNoLock(f, fileInfo, compressedFilePath, cacheKind)
	return
}

type FsWhenCompressErr struct {
	error
	Path string
}

func (we FsWhenCompressErr) Unwrap() error {
	return we.error
}
func (we FsWhenCompressErr) Error() string {
	return "fasthttp Fs serve: compress file: " + we.Path + "occur: " + we.error.Error()
}

func (h *fsHandler) compressFileNoLock(
	f FileSeeker, fileInfo fs.FileInfo, compressedFilePath string, cacheKind CacheKind,
) (ff *fsFile, err error) {
	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	//
	// Atomic rename
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, fs.ErrPermission) {
			err = errNoCreatePermission
			return
		}
		err = FsWhenCompressErr{error: err, Path: tmpFilePath}
		return
	}
	//
	var compressPool = h.cacheKind2Pool[cacheKind]
	w := compressPool.Get()
	w.Reset(zf)
	_, err = copyZeroAlloc(w, f)
	err = w.Close()
	compressPool.Put(w)
	//
	_ = zf.Close() // flush
	_ = f.Close()
	//
	if err != nil {
		err = FsWhenCompressErr{Path: compressedFilePath, error: err}
		return
	}
	//
	err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime())
	if err != nil {
		err = FsWhenCompressErr{Path: tmpFilePath, error: err}
		return
	}
	//
	err = os.Rename(tmpFilePath, compressedFilePath)
	if err != nil {
		err = FsWhenCompressErr{Path: tmpFilePath, error: err}
		return
	}
	//
	return h.newCompressedFSFile(compressedFilePath, cacheKind)
}

// newCompressedFSFileCache use memory cache compressed files.
// filepath is compressed file with suffix.
func (h *fsHandler) newCompressedFSFileCache(f FileSeeker, fileInfo fs.FileInfo, filePath string, cacheKind CacheKind) (ff *fsFile, err error) {
	buf := pbytes.NewBufferWithSize((int(fileInfo.Size()) * 3) >> 2)
	var compressPool = h.cacheKind2Pool[cacheKind]
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
		err = FsWhenCompressErr{
			Path:  filePath,
			error: err,
		}
		return
	}

	if _, err = f.Seek(0, io.SeekStart); err != nil {
		buf.RecycleItems()
		_ = f.Close()
		return nil, err
	}
	//
	lastModified := fileInfo.ModTime()
	ff = h.smallFsFilePool.Get()
	ff.h = h
	ff.dir = false
	ff.normalFile = false
	ff.dirIndex = buf.Bytes()
	ff.contentLength = int64(len(ff.dirIndex))
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
func (h *fsHandler) newCompressedFSFile(compressedFilePath string, cacheKind CacheKind) (ff *fsFile, err error) {
	f1, err := h.filesystem.Open(compressedFilePath)
	if err != nil {
		err = FsWhenCompressErr{
			Path:  compressedFilePath,
			error: err,
		}
		return
	}
	//
	f, ok := f1.(FileSeeker)
	if !ok {
		err = FsOpenedFileNotSeekableErr
		return
	}
	//
	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		err = FsWhenCompressErr{
			Path:  compressedFilePath,
			error: err,
		}
		return
	}
	return h.newFSFile(f, fileInfo, true, compressedFilePath, cacheKind)
}

// filePath is a path without trailer slash.
func (h *fsHandler) openFSFile(filePath, reqPath string, endSlash, mustCompress bool, cacheKind CacheKind) (ff *fsFile, err error) {
	originalPathStr := reqPath
	if endSlash {
		reqPath = reqPath[:len(reqPath)-1]
	}
	var f1 fs.File
	if filePath == "" {
		f1, err = h.filesystem.Open(".")
	} else {
		f1, err = h.filesystem.Open(filePath)
	}
	//
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = FsFileNotFoundErr
		} else if errors.Is(err, fs.ErrPermission) {
			err = FsPermissionDeniedErr
		}
		return
	}
	fileInfo, err := f1.Stat()
	if err != nil {
		_ = f1.Close()
		return
	}
	//
	var ok bool
	var isDir = fileInfo.IsDir()

	if isDir {
		// Directory information is no longer needed and can be safely closed.
		_ = f1.Close()
		if endSlash {
			// Check if there are index-type files in the directory.
			for _, indexName := range h.indexNames {
				filePath1 := filePath + "/" + indexName
				f1, err = h.filesystem.Open(filePath1)
				if err == nil {
					filePath = filePath1
					ok = true
					isDir = false
					break
				}
				if !errors.Is(err, fs.ErrNotExist) {
					return
				}
				// Reset this error, and then check if the directory can be indexed.
				err = nil
			}
			//
			if ok {
				// At this point, fileInfo must represent a file type rather than a directory.
				fileInfo, err = f1.Stat()
				if err != nil {
					return
				}
			}
			// Check if the directory can be indexed.
			if !ok && !h.generateIndexPages {
				err = FsNotAllowedIndexDirErr
				return
			}
		} else {
			// Directory paths without a trailing slash need
			// to be redirected to the version with a trailing slash.
			err = errDirIndexRequired
			return
		}
	}
	var f FileSeeker
	// At this point, f1 must represent either a file
	// or a directory ending with a trailing slash.
	//
	// If it is a file type, it must implement `io.Seeker`.
	var ct string
	if !isDir {
		f, ok = f1.(FileSeeker)
		if !ok {
			err = FsOpenedFileNotSeekableErr
			return
		}
		//
		ct, err = contentType(f, fileInfo.Name())
		if err != nil {
			return
		}
		//
		if mustCompress {
			if fileInfo.Size() > fsMaxCompressibleFileSize || fileInfo.Size() < h.needCompressSize {
				mustCompress = false
			}
			if mustCompress {
				mustCompress = compress.CheckMimeOk(s2b(ct))
			}
		}
	}
	// Is the directory for storing compressed files the same as the source file's directory?
	var inPlaceCompress bool
	if mustCompress {
		originalPath := filePath
		filePath = h.filePathToCompressed(filePath)
		if filePath == originalPath {
			inPlaceCompress = true
		}
	}
	if h.cacheManager == nil {
		// File caching functionality is not enabled.
		if isDir {
			return h.createDirIndex(filePath, reqPath, mustCompress, cacheKind)
		}
		if mustCompress {
			// Avoid performing compression operations on the same file simultaneously.
			l := h.fileLocks[cacheKind].Locker(filePath)
			l.Lock()
			ff, err = h.compressAndOpenFSFile(f, fileInfo, filePath, inPlaceCompress, cacheKind)
			ff.contentType = ct
			l.Unlock()
			return
		}
		ff, err = h.newFSFile(f, fileInfo, false, filePath, cacheKind)
		ff.contentType = ct
		return
	}

	// After unlocking, the file cache version must be stored in the cache unless an error occurs.
	l := h.fileLocks[cacheKind].Locker(filePath)
	l.Lock()
	ff, ok = h.cacheManager.GetFileFromCache(cacheKind, originalPathStr)
	if ok {
		l.Unlock()
		return
	}
	// Create a cache entry and store it in the corresponding container.
	if isDir {
		ff, err = h.createDirIndex(filePath, reqPath, mustCompress, cacheKind)
	} else if mustCompress {
		ff, err = h.compressAndOpenFSFile(f, fileInfo, filePath, inPlaceCompress, cacheKind)
		ff.contentType = ct
	} else {
		ff, err = h.newFSFile(f, fileInfo, false, filePath, cacheKind)
		ff.contentType = ct
	}
	// TODO need cache error?
	if err == nil {
		if !isDir {
			ff.originalFileName = append(ff.originalFileName[:0], filePath...)
		}
		ff = h.cacheManager.SetFileToCache(cacheKind, originalPathStr, ff)
	}
	l.Unlock()
	return
}

// Detect the file type.
func contentType(f FileSeeker, fp string) (contentType string, err error) {
	ext := filepath.Ext(fp)
	if len(ext) != 0 {
		// Quick path
		contentType = http.TypeByExtension(ext, "")
		if len(contentType) != 0 {
			return
		}
	}
	// Slow path
	pb := pool.Get(512)
	pb.B = pb.B[:cap(pb.B)]
	var n int
	for n < 512 && err == nil {
		var nn int
		nn, err = f.Read(pb.B[n:])
		n += nn
	}
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		pb.RecycleToPool00()
		return
	}
	//
	if n > 0 {
		contentType = ghttp.DetectContentType(pb.B[:n])
	} else {
		contentType = http.MIMEOctetStream
	}
	pb.RecycleToPool00()
	_, err = f.Seek(0, io.SeekStart)
	return
}

// Create an entry representing the file.
func (h *fsHandler) newFSFile(f FileSeeker, fileInfo fs.FileInfo, compressed bool, filePath string, cacheKind CacheKind) (ff *fsFile, err error) {
	n := fileInfo.Size()
	contentLength := n
	// On 32-bit platforms, only files smaller than 2GB can be handled.
	if n != contentLength {
		_ = f.Close()
		err = FsCantHandleBigFileErr
		return
	}

	lastModified := fileInfo.ModTime()

	if contentLength > MaxSmallFileSize {
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
	ff.h = h
	ff.filename = append(ff.filename, filePath...)
	ff.contentLength = contentLength
	ff.compressed = compressed
	ff.lastModified = lastModified
	ff.lastModifiedStr = AppendHTTPDate(ff.lastModifiedStr, lastModified)
	ff.t = time.Now()
	return
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

var _ fs.FS = (*osFS)(nil)

type osFS struct{}

func (o *osFS) Open(name string) (fs.File, error)     { return os.Open(name) }
func (o *osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
