package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
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
		hasTrailingSlash := len(path) > 0 && (path[len(path)-1] == '/' || path[len(path)-1] == '\\')

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

var (
	rootFSOnce sync.Once
	rootFS     = &FS{
		Root:               "",
		AllowEmptyRoot:     true,
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
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
		CompressBrotli:     true,
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

	// AllowEmptyRoot controls what happens when Root is empty. When false (default) it will default to the
	// current working directory. An empty root is mostly useful when you want to use absolute paths
	// on windows that are on different filesystems. On linux setting your Root to "/" already allows you to use
	// absolute paths on any filesystem.
	AllowEmptyRoot bool

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

	// Uses brotli encoding and fallbacks to gzip in responses if set to true, uses gzip if set to false.
	//
	// This value has sense only if Compress is set.
	//
	// Brotli encoding is disabled by default.
	CompressBrotli bool

	// Path to the compressed root directory to serve files from. If this value
	// is empty, Root is used.
	CompressRoot string

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

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
	CompressedFileSuffix string

	// Suffixes list to add to compressedFileSuffix depending on encoding
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffixes is used by default.
	CompressedFileSuffixes map[string]string

	// If CleanStop is set, the channel can be closed to stop the cleanup handlers
	// for the FS RequestHandlers created with NewRequestHandler.
	// NEVER close this channel while the handler is still being used!
	CleanStop chan struct{}

	once sync.Once
	h    RequestHandler
}

// FSCompressedFileSuffix is the suffix FS adds to the original file names
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
const FSCompressedFileSuffix = ".fasthttp.gz"

// FSCompressedFileSuffixes is the suffixes FS adds to the original file names depending on encoding
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
var FSCompressedFileSuffixes = map[string]string{
	"gzip": ".fasthttp.gz",
	"br":   ".fasthttp.br",
}

// FSHandlerCacheDuration is the default expiration duration for inactive
// file handlers opened by FS.
const FSHandlerCacheDuration = 10 * time.Second

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
// Otherwise goroutine leak will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	f := &FS{
		Root:               root,
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		AcceptByteRange:    true,
	}
	if stripSlashes > 0 {
		f.PathRewrite = NewPathSlashesStripper(stripSlashes)
	}
	return f.NewRequestHandler()
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
	for len(root) > 0 && root[len(root)-1] == os.PathSeparator {
		root = root[:len(root)-1]
	}
	return root
}

func (fs *FS) initRequestHandler() {
	root := fs.normalizeRoot(fs.Root)

	compressRoot := fs.CompressRoot
	if compressRoot == "" {
		compressRoot = root
	} else {
		compressRoot = fs.normalizeRoot(compressRoot)
	}

	compressedFileSuffixes := fs.CompressedFileSuffixes
	if compressedFileSuffixes["br"] == "" || compressedFileSuffixes["gzip"] == "" ||
		compressedFileSuffixes["br"] == compressedFileSuffixes["gzip"] {
		// Copy global map
		compressedFileSuffixes = make(map[string]string, len(FSCompressedFileSuffixes))
		for k, v := range FSCompressedFileSuffixes {
			compressedFileSuffixes[k] = v
		}
	}

	if len(fs.CompressedFileSuffix) > 0 {
		compressedFileSuffixes["gzip"] = fs.CompressedFileSuffix
		compressedFileSuffixes["br"] = FSCompressedFileSuffixes["br"]
	}

	h := &fsHandler{
		filesystem:             fs.FS,
		root:                   root,
		indexNames:             fs.IndexNames,
		pathRewrite:            fs.PathRewrite,
		generateIndexPages:     fs.GenerateIndexPages,
		compress:               fs.Compress,
		compressBrotli:         fs.CompressBrotli,
		compressRoot:           compressRoot,
		pathNotFound:           fs.PathNotFound,
		acceptByteRange:        fs.AcceptByteRange,
		compressedFileSuffixes: compressedFileSuffixes,
	}

	h.cacheManager = newCacheManager(fs)

	if h.filesystem == nil {
		h.filesystem = &osFS{} // It provides os.Open and os.Stat
	}

	fs.h = h.handleRequest
}

type fsHandler struct {
	filesystem fs.FS
	root       string
	// 一旦设置了indexNames，则便不会尊重 generateIndexPages 的值
	// 如果目录+indexName 没有找到文件就返回错误。
	indexNames             []string
	pathRewrite            PathRewriteFunc
	pathNotFound           RequestHandler
	generateIndexPages     bool
	compress               bool
	compressBrotli         bool
	compressRoot           string
	acceptByteRange        bool
	compressedFileSuffixes map[string]string

	cacheManager cacheManager

	smallFileReaderPool sync.Pool
}

const FileNameLength = 92 // 85
const ContentTypeLength = 30
const LastModifiedLength = 30

// fsFile 表示文件服务系统中的一个具体文件(非目录)的信息
// 可能是压缩文件版本或源文件版本，压缩文件是文件系统创建的(请求导致的创建)
type fsFile struct {
	// 文件服务系统的fsHandler表示形式由fasthttp.Fs转换而来
	h *fsHandler
	f fs.File
	// 完整路径名：目录+文件名
	// 85
	filename []byte // fs.FileInfo.Name() return filename, isn't filepath.
	// 1. 目录下的索引列表(此时 f 也是nil，但h=osFS 、compressed可能是true)
	// 或2.当 f =nil h != osFS compressed=true
	// 为f的压缩版本的内存存在形式，压缩后f赋值为nil newCompressedFSFileCache 方法处理的
	dirIndex []byte
	// 文件类型 30
	contentType []byte
	// 文件长度(无论f是否是压缩文件)
	// 或者是目录索引内容的长度
	contentLength int
	// 文件是否是压缩文件
	ok atomic.Bool
	// 此文件被读取了多少次
	readersCount atomic.Int32
	compressed   bool
	isBigFile    bool

	// 文件的修改日期
	// 如果文件是创建的压缩文件，这lastModified是压缩文件的创建时间
	//
	// 如果是目录且创建了索引文件，则是索引文件的创建日期。
	lastModified time.Time
	// Modified 的响应头的字节切片形式
	// Wed, 21 Oct 2015 07:28:00 GMT
	// 25 byte
	lastModifiedStr []byte

	// 此对象的创建时间
	t time.Time

	bigFPool *sync.Pool
	// bigFiles     []*bigFileReader
	// bigFilesLock sync.Mutex
	ready chan struct{}
	err   error
	cache *sync.Map
}

func closeFsReady(ff *fsFile, err bool, path string) {
	if err {
		if ff.f != nil {
			_ = ff.f.Close()
		}
		if ff.cache != nil {
			ff.cache.Delete(path)
		}
	}
	if ff.cache != nil {
		close(ff.ready)
	}
	_, ok := ff.h.cacheManager.(*inMemoryCacheManager)
	if ok {
		fsPool.Put(ff)
	}
}
func (fF *fsFile) delete() {
}

func (fF *fsFile) NewReader() (io.Reader, error) {
	if fF.isBigFile {
		if fF.cache == nil {
			return fF.bigFileReader()
		}
		br := fF.bigFPool.Get().(*bigFileReader)
		err := br.err
		if err != nil {
			fF.decReadersCount()
		}
		return br, err
	}
	return fF.smallFileReader()
}

func (fF *fsFile) smallFileReader() (io.Reader, error) {
	v := fF.h.smallFileReaderPool.Get()
	if v == nil {
		v = &fsSmallFileReader{}
	}
	r := v.(*fsSmallFileReader)
	r.ff = fF
	r.endPos = fF.contentLength
	if r.startPos > 0 {
		return nil, errors.New("bug: fsSmallFileReader with non-nil startPos found in the pool")
	}
	return r, nil
}

func (fF *fsFile) bigFileReader() (io.Reader, error) {
	if fF.f == nil {
		return nil, errors.New("bug: ff.f must be non-nil in bigFileReader")
	}

	f, err := fF.h.filesystem.Open(b2s(fF.filename))
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %w", err)
	}
	return &bigFileReader{
		f:  f,
		ff: fF,
		r:  f,
	}, nil
}

// Files bigger than this size are sent with sendfile.
const maxSmallFileSize = 2 * 4096

func (fF *fsFile) isBig() bool {
	if _, ok := fF.h.filesystem.(*osFS); !ok { // fs.FS only uses bigFileReader, memory cache uses fsSmallFileReader
		// 不是系统文件系统上的文件 ff.f=nil
		// 则说明ff是一个内存压缩文件 newCompressedFSFileCache 函数负责
		// 处理这种情况。
		// 即请求的文件是非osFS上的文件，因为设置了压缩，又要对压缩进行缓存所以
		// 将压缩版本存储到内存中：将ff.f设置为nil，将压缩版本存储到f.dirIndex字段中
		return fF.f != nil
	}
	// ff是文件系统上的文件且不是目录且文件大小大于 maxSmallFileSize 8129字节
	// 则使用大文件发送方式
	return fF.contentLength > maxSmallFileSize && len(fF.dirIndex) == 0
}

func (fF *fsFile) Release() {
	if fF.f != nil {
		_ = fF.f.Close()

	}
}

func (fF *fsFile) decReadersCount() {
	fF.readersCount.Add(-1)
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f   fs.File
	ff  *fsFile
	r   io.Reader
	lr  io.LimitedReader
	err error
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int) error {
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

func (r *bigFileReader) Close() error {
	r.r = r.f
	seeker, ok := r.f.(io.Seeker)
	if !ok {
		_ = r.f.Close()
		return errors.New("must implement io.Seeker")
	}
	n, err := seeker.Seek(0, io.SeekStart)
	if err == nil {
		if n != 0 {
			_ = r.f.Close()
			err = errors.New("bug: File.Seek(0, io.SeekStart) returned (non-zero, nil)")
		}
		r.ff.bigFPool.Put(r)
	} else {
		_ = r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

type fsSmallFileReader struct {
	ff       *fsFile
	startPos int
	endPos   int
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.startPos = 0
	r.endPos = 0
	ff.h.smallFileReaderPool.Put(r)
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
	bufv := CopyBufPool.Get()
	buf := bufv.([]byte)
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
	CopyBufPool.Put(bufv)

	if err == io.EOF {
		err = nil
	}
	return int64(curPos - r.startPos), err
}

type cacheManager interface {
	WithLock(work func())
	GetFileFromCache(cacheKind CacheKind, path string) (*fsFile, bool)
	SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile
}

var (
	_ cacheManager = (*inMemoryCacheManager)(nil)
	_ cacheManager = (*noopCacheManager)(nil)
)

type CacheKind uint8

const (
	defaultCacheKind CacheKind = iota
	brotliCacheKind
	gzipCacheKind
)

type fsFilePool struct {
	sync.Pool
}

var fsPool = &fsFilePool{
	Pool: sync.Pool{
		New: func() any {
			fs := new(fsFile)
			fs.filename = make([]byte, 0, FileNameLength)
			fs.lastModifiedStr = make([]byte, 0, LastModifiedLength)
			fs.contentType = make([]byte, 0, ContentTypeLength)
			return fs
		},
	},
}

func (p *fsFilePool) release(ff *fsFile) {
	ff.err = nil
	ff.isBigFile = false
	ff.ready = nil
	ff.f = nil
	ff.contentType = ff.contentType[:0]
	ff.lastModifiedStr = ff.lastModifiedStr[:0]
	ff.contentLength = 0
	ff.compressed = false
	ff.dirIndex = nil
	if len(ff.filename) > FileNameLength {
		ff.filename = make([]byte, 0, FileNameLength)
	} else {
		ff.filename = ff.filename[:0]
	}
	ff.ok.Store(false)
	ff.t = time.Time{}
	ff.lastModified = time.Time{}
	ff.h = nil
	ff.bigFPool = nil
	ff.readersCount.Store(0)
	ff.cache = nil
	p.Pool.Put(ff)
}

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
		cache:         sync.Map{},
		cacheBrotli:   sync.Map{},
		cacheGzip:     sync.Map{},
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
	ff := fsPool.Get().(*fsFile)
	return ff, false
}

func (*noopCacheManager) SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile {
	return ff
}

type inMemoryCacheManager struct {
	cacheDuration time.Duration
	cache         sync.Map
	cacheBrotli   sync.Map
	cacheGzip     sync.Map
	cacheLock     sync.Mutex
}

func (cm *inMemoryCacheManager) GetCache(kind CacheKind) *sync.Map {
	if kind == brotliCacheKind {
		return &cm.cacheBrotli
	}
	if kind == gzipCacheKind {
		return &cm.cacheGzip
	}
	return &cm.cache
}
func (cm *inMemoryCacheManager) SetFileToCache(cacheKind CacheKind, path string, ff *fsFile) *fsFile {
	// TODO implement me
	panic("implement me")
}

func (cm *inMemoryCacheManager) WithLock(work func()) {
	cm.cacheLock.Lock()

	work()

	cm.cacheLock.Unlock()
}

func (cm *inMemoryCacheManager) DelFileFromCache(path string) {
	cm.cache.Delete(path)
	cm.cacheBrotli.Delete(path)
	cm.cacheGzip.Delete(path)
}
func (cm *inMemoryCacheManager) GetFileFromCache(cacheKind CacheKind, path string) (*fsFile, bool) {
	var fileCache *sync.Map

	if cacheKind == brotliCacheKind {
		fileCache = &cm.cacheBrotli
	} else if cacheKind == gzipCacheKind {
		fileCache = &cm.cacheGzip
	} else {
		fileCache = &cm.cache
	}
	r, ok := fileCache.Load(path)
	if ok {
		f := r.(*fsFile)
		if f.ok.Load() {
			return f, true
		} else {
			<-f.ready
			return f, true
		}
	}
	cm.cacheLock.Lock()
	r, ok = fileCache.Load(path)
	if ok {
		cm.cacheLock.Unlock()
		f := r.(*fsFile)
		if f.ok.Load() {
			return f, true
		} else {
			<-f.ready
			return f, true
		}
	}
	f := fsPool.Get().(*fsFile)
	f.cache = fileCache
	f.ready = make(chan struct{})
	p := make([]byte, len(path))
	copy(p, path)
	fileCache.Store(b2s(p), f)
	cm.cacheLock.Unlock()
	return f, false
}

// SetFileToCache cacheKind 缓存类型
// path 请求路径经过 rewrite字段处理过后返回的路径，且去掉了尾部的/
// ff path通过 pathToFilePath 后的文件路径的原始版本，或者压缩版本，或目录下的主页文件
//
// 返回的是参数ff或者通过path从缓存中检索到的ff(其它客户存入的，这种情况下参数ff关闭)。

func (cm *inMemoryCacheManager) cleanCache() {
	c := &cm.cacheBrotli
	f := func(key, value any) bool {
		v := value.(*fsFile)
		if v.err != nil {
			c.Delete(key)
		}
		return true
	}
	c.Range(f)
	c = &cm.cacheGzip
	c.Range(f)
	c = &cm.cache
	c.Range(f)
}
func (cm *inMemoryCacheManager) handleCleanCache(cleanStop chan struct{}) {

	clean := func() {
		cm.cleanCache()
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
	/*
		for {
				time.Sleep(cm.cacheDuration / 2)
				clean()
		}
	*/

}
func (h *fsHandler) pathToFilePath(path string) string {
	if _, ok := h.filesystem.(*osFS); !ok {
		if len(path) < 1 {
			return path
		}
		return path[1:]
	}
	return filepath.FromSlash(h.root + path)
}

func (h *fsHandler) filePathToCompressed(filePath string) string {
	if h.root == h.compressRoot {
		return filePath
	}
	if !strings.HasPrefix(filePath, h.root) {
		return filePath
	}
	return filepath.FromSlash(h.compressRoot + filePath[len(h.root):])
}

//goland:noinspection GoDirectComparisonOfErrors
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
	// pathStr := string(path)
	pathStr := b2s(path)
	// 如果请求支持压缩传输，且compress字段的值为true，即服务端开启了对
	// 文件服务的压缩工能，这mustCompress=true。
	mustCompress := false
	fileCacheKind := defaultCacheKind
	fileEncoding := ""
	byteRange := ctx.Request.Header.peek(strRange)
	if len(byteRange) == 0 && h.compress {
		if h.compressBrotli && ctx.Request.Header.HasAcceptEncodingBytes(strBr) {
			mustCompress = true
			fileCacheKind = brotliCacheKind
			fileEncoding = "br"
		} else if ctx.Request.Header.HasAcceptEncodingBytes(strGzip) {
			mustCompress = true
			fileCacheKind = gzipCacheKind
			fileEncoding = "gzip"
		}
	}
	if _, ok := h.filesystem.(*osFS); ok {

		p := make([]byte, 0, FileNameLength)
		p = append(p, h.root...)
		p = append(p, pathStr...)
		// p[len(h.root)+len(pathStr)] = 0
		// fp := filepath.FromSlash(h.root + pathStr)
		// h.root + pathStr+
		// s, err := os.Stat(fp)
		// err := error(nil)
		// err := syscall.Access2(p, syscall.F_OK)
		// exist := err == nil
		exist := true
		// exist := os.Stat2(p)
		// err := error(nil)
		if !exist {
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
			}
			m, ok := h.cacheManager.(*inMemoryCacheManager)
			if ok {
				c := m.GetCache(fileCacheKind)
				c.Delete(pathStr)
			}
			return
		}
		/*
			if s.IsDir() {
				if !hasTrailingSlash {
					ctx.RedirectBytes(append(path, '/'), StatusFound)
					return
				}
			}
		*/
	}

	// pathStr 有可能是文件或者目录，且没有/后缀
	ff, ok := h.cacheManager.GetFileFromCache(fileCacheKind, pathStr)
	if !ok {
		ff.h = h
	}
	if ok && ff.err == nil {
		goto ok
	}
	{
		// 缓存不存在
		// 构建相对于文件系统的路径
		filePath := h.pathToFilePath(pathStr)

		var err error
		// 返回的ff是filePath路径对应的文件表示形式
		// 或filePath路径对应的文件的压缩版本表示形式
		if ok {
			err = ff.err
		} else {
			err = ff.openFSFile(filePath, mustCompress, fileEncoding)
		}
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			// 如果没有权限在文件系统上创建filePath的压缩版，直接使用其原始版本
			if ok {
				err = ff.err
			} else {
				err = ff.openFSFile(filePath, mustCompress, fileEncoding)
			}
		}
		if err == errDirIndexRequired {
			// filePath 是个目录名
			if !hasTrailingSlash {
				// 请求path经过rewrite字段处理后不以/结尾
				// 则重定向到以/结尾的path
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				if !ok {
					closeFsReady(ff, true, pathStr)
				}
				return
			}
			// 尝试打开filePath目录下的主页文件
			if ok {
				err = ff.err
			} else {
				err = ff.openIndexFile(ctx, filePath, mustCompress, fileEncoding)
			}
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				if !ok {
					ff.err = err
				}
				if !ok {
					closeFsReady(ff, true, pathStr)
				}
				return
			}
		} else if err != nil {
			// 其它错误则作为文件不存在来对用户进行响应
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
			}
			if !ok {
				ff.err = err
			}

			if !ok {
				closeFsReady(ff, true, pathStr)
			}
			return
		}

		// pathStr是目录(没有/后缀)或文件
		// ff 可能是pathStr对应的文件原始版本/压缩版本或者目录下的index文件的原始版本/压缩版本
		// ff = h.cacheManager.SetFileToCache(fileCacheKind, pathStr, ff)
		if ff.isBig() {
			ff.isBigFile = true
			ff.bigFPool = &sync.Pool{
				New: func() any {
					var b = &bigFileReader{}
					var pa = pathStr
					if ff.f == nil {
						ff.err = errors.New("bug: ff.f must be non-nil in bigFileReader")
						ff.cache.Delete(pa)
						return b
					}
					f, err := ff.h.filesystem.Open(b2s(ff.filename))
					if err != nil {
						b.err = errors.New("cannot open already opened file: " + err.Error())
						return b
					}
					b.f = f
					b.ff = ff
					b.r = f
					return b
				},
			}
		}
		if ff.cache != nil {
			ff.ok.Store(true)
			close(ff.ready)
		}
	}
ok:
	ff.readersCount.Add(1)

	// 如果ff是pathStr对应的文件的压缩版本，下面的判断可能不正确
	// 因为ff.lastModified 是压缩文件的创建日期，而不是原始文件的修改日期
	if !ctx.IfModifiedSince(ff.lastModified) {
		// 没有变更，不发送文件
		ff.decReadersCount()
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
		if fileEncoding == "br" {
			hdr.SetContentEncodingBytes(strBr)
		} else if fileEncoding == "gzip" {
			hdr.SetContentEncodingBytes(strGzip)
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
		//
		/*
			fd := r.(*bigFileReader).r.(*os.File).Fd()
			ctx.Response.Header.SetContentLength(contentLength)
			_, err = ctx.GetConn().Write(ctx.Response.Header.Header())
			if err != nil {
				log.Fatal(err)
			}
			if err != nil {
				return
			}
			w, err := sendfile.SendFile(ctx.GetConn(), int(fd), 0, int64(contentLength))
			if err != nil {
				log.Fatal(err, w)
			}
			ctx.SendF = true
		*/

		// ctx.SetBodyStream(r, contentLength)
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
	// 没有错误的bigFile才放回缓存池(通过io.Closer接口,writeResponse会处理)，因为如果ff.err未nil
	// bigFile应该不会产生错误。
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentTypeBytes(ff.contentType)
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

// 如果设置了 h.indexNames 则将 dirPath+indexName 进行遍历组合，看是否存在，不存在则返回错误。
// 否则，使用用dirPath下的文件创建索引文件，将结果存储到 h.dirIndex中。并将f.f设置为nil.
func (fF *fsFile) openIndexFile(ctx *RequestCtx, dirPath string, mustCompress bool, fileEncoding string) error {
	for _, indexName := range fF.h.indexNames {
		indexFilePath := dirPath + "/" + indexName
		err := fF.openFSFile(indexFilePath, mustCompress, fileEncoding)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cannot open file %q: %w", indexFilePath, err)
		}
	}

	if !fF.h.generateIndexPages {
		return fmt.Errorf("cannot access directory without index page. Directory %q", dirPath)
	}

	return fF.createDirIndex(ctx, dirPath, mustCompress, fileEncoding)
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

func (fF *fsFile) createDirIndex(ctx *RequestCtx, dirPath string, mustCompress bool, fileEncoding string) error {
	w := &bytebufferpool.ByteBuffer{}

	base := ctx.URI()

	basePathEscaped := html.EscapeString(string(base.Path()))
	_, _ = fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	dirEntries, err := fs.ReadDir(fF.h.filesystem, dirPath)
	if err != nil {
		return err
	}

	fm := make(map[string]fs.FileInfo, len(dirEntries))
	filenames := make([]string, 0, len(dirEntries))
nestedContinue:
	for _, de := range dirEntries {
		name := de.Name()
		for _, cfs := range fF.h.compressedFileSuffixes {
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
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	_, _ = fmt.Fprintf(w, "</ul></body></html>")

	if mustCompress {
		var zbuf bytebufferpool.ByteBuffer
		if fileEncoding == "br" {
			zbuf.B = AppendBrotliBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		} else if fileEncoding == "gzip" {
			zbuf.B = AppendGzipBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		}
		w = &zbuf
	}

	dirIndex := w.B
	lastModified := time.Now()
	fF.dirIndex = dirIndex
	fF.contentType = []byte("text/html; charset=utf-8")
	fF.compressed = mustCompress
	fF.contentLength = len(dirIndex)
	fF.lastModified = lastModified
	fF.lastModifiedStr = append(fF.lastModifiedStr, AppendHTTPDate(nil, lastModified)...)
	fF.t = time.Now()
	return nil
}

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024
)

// compressAndOpenFSFile 以指定编码 fileEncoding 压缩 filePath ，构建压缩文件名为 filePath + 对应编码后缀
// 返回值：
// 1. filePath以压缩格式后缀结尾||filePath文件格式不适合压缩||文件大小超过 fsMaxCompressibleFileSize
// 则返回的 fsFile 是filePath的原始信息
// 2. 返回是filePath压缩版本的 fsFile
func (fF *fsFile) compressAndOpenFSFile(filePath string, fileEncoding string) error {
	f, err := fF.h.filesystem.Open(filePath)
	if err != nil {
		return err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		return errDirIndexRequired
	}

	if strings.HasSuffix(filePath, fF.h.compressedFileSuffixes[fileEncoding]) ||
		fileInfo.Size() > fsMaxCompressibleFileSize ||
		!isFileCompressible(f, fsMinCompressRatio) {
		// 要创建的文件已经是压缩文件了
		// 要压缩的文件大小超过  fsMaxCompressibleFileSize
		// 要压缩的文件格式不适合压缩
		// 则直接返回打开的filePath
		return fF.newFSFile(f, fileInfo, false, filePath, "")
	}

	// compressedFilePath 压缩文件构建后应该存在的目录
	compressedFilePath := fF.h.filePathToCompressed(filePath)

	if _, ok := fF.h.filesystem.(*osFS); !ok {
		// 文件服务系统不是系统磁盘文件系统
		// 将压缩版本存储到内存中
		return fF.newCompressedFSFileCache(f, fileInfo, compressedFilePath, fileEncoding)
	}

	if compressedFilePath != filePath {
		// 压缩文件与源文件不在同一个目录
		// 尝试递归创建压缩文件所在的目录
		if err := os.MkdirAll(filepath.Dir(compressedFilePath), os.ModePerm); err != nil {
			return err
		}
	}
	compressedFilePath += fF.h.compressedFileSuffixes[fileEncoding]

	// absPath 返回绝对路径
	absPath, err := filepath.Abs(compressedFilePath)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot determine absolute path for %q: %v", compressedFilePath, err)
	}

	// 获取文件锁，因为要对文件名 absPath 进行写操作
	flock := getFileLock(absPath)
	flock.Lock()
	// f 要压缩的文件的 fs.File 表示方式
	// fileInfo fs.FIle.Stat()的返回值
	// compressedFilePath filePath压缩后的文件名(全路径)
	// 要压缩的编码 br、gzip
	err = fF.compressFileNolock(f, fileInfo, filePath, compressedFilePath, fileEncoding)
	flock.Unlock()

	return err
}

// compressFileNolock f和fileInfo是filePath的fs.File和fs.Stat的表示形式
// compressedFilePath是压缩filePath后的存储路径(已带编码后缀)
// 目标编码格式：gzip、br
func (fF *fsFile) compressFileNolock(
	f fs.File, fileInfo fs.FileInfo, filePath, compressedFilePath, fileEncoding string,
) error {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err := os.Stat(compressedFilePath); err == nil {
		_ = f.Close()
		// 压缩文件已经存在
		return fF.newCompressedFSFile(compressedFilePath, fileEncoding)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		_ = f.Close()
		if !errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("cannot create temporary file %q: %w", tmpFilePath, err)
		}
		return errNoCreatePermission
	}
	if fileEncoding == "br" {
		zw := acquireStacklessBrotliWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessBrotliWriter(zw, CompressDefaultCompression)
	} else if fileEncoding == "gzip" {
		zw := acquireStacklessGzipWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	}
	_ = zf.Close()
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("error when compressing file %q to %q: %w", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		return fmt.Errorf("cannot change modification time to %v for tmp file %q: %v",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		return fmt.Errorf("cannot move compressed file from %q to %q: %w", tmpFilePath, compressedFilePath, err)
	}
	return fF.newCompressedFSFile(compressedFilePath, fileEncoding)
}

// newCompressedFSFileCache use memory cache compressed files.
func (fF *fsFile) newCompressedFSFileCache(f fs.File, fileInfo fs.FileInfo, filePath, fileEncoding string) error {
	var (
		w   = &bytebufferpool.ByteBuffer{}
		err error
	)

	if fileEncoding == "br" {
		zw := acquireStacklessBrotliWriter(w, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessBrotliWriter(zw, CompressDefaultCompression)
	} else if fileEncoding == "gzip" {
		zw := acquireStacklessGzipWriter(w, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	}
	defer func() { _ = f.Close() }()

	if err != nil {
		return fmt.Errorf("error when compressing file %q: %w", filePath, err)
	}

	seeker, ok := f.(io.Seeker)
	if !ok {
		return errors.New("not implemented io.Seeker")
	}
	if _, err = seeker.Seek(0, io.SeekStart); err != nil {
		return err
	}

	ext := fileExtension(fileInfo.Name(), false, fF.h.compressedFileSuffixes[fileEncoding])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, false, fileEncoding)
		if err != nil {
			return fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	dirIndex := w.B
	lastModified := fileInfo.ModTime()
	fF.dirIndex = dirIndex
	fF.contentType = append(fF.contentType, contentType...)
	fF.contentLength = len(dirIndex)
	fF.compressed = true
	fF.lastModified = lastModified
	fF.lastModifiedStr = append(fF.lastModifiedStr, AppendHTTPDate(nil, lastModified)...)
	fF.t = time.Now()
	return nil
}

func (fF *fsFile) newCompressedFSFile(filePath string, fileEncoding string) error {
	f, err := fF.h.filesystem.Open(filePath)
	if err != nil {
		return fmt.Errorf("cannot open compressed file %q: %w", filePath, err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot obtain info for compressed file %q: %w", filePath, err)
	}
	return fF.newFSFile(f, fileInfo, true, filePath, fileEncoding)
}

// 如果mustCompress = true，则会创建filePath对应的压缩版本
// 并返回其fsFile表示形式，否则直接返回filePath表示形式
func (fF *fsFile) openFSFile(filePath string, mustCompress bool, fileEncoding string) error {
	filePathOriginal := filePath
	if mustCompress {
		filePath += fF.h.compressedFileSuffixes[fileEncoding]
	}

	f, err := fF.h.filesystem.Open(filePath)
	if err != nil {
		if mustCompress && errors.Is(err, fs.ErrNotExist) {
			// 压缩文件不存在，创建压缩文件
			return fF.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}
		return err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		if mustCompress {
			// 重构的压缩文件是一个目录
			return fmt.Errorf("directory with unexpected suffix found: %q. Suffix: %q",
				filePath, fF.h.compressedFileSuffixes[fileEncoding])
		}
		return errDirIndexRequired
	}

	// 至此压缩文件 filePath 存在与文件系统中
	if mustCompress {
		fileInfoOriginal, err := fs.Stat(fF.h.filesystem, filePathOriginal)
		if err != nil {
			_ = f.Close()
			// 原始文件不存在，即使压缩文件存在也返回错误
			return fmt.Errorf("cannot obtain info for original file %q: %w", filePathOriginal, err)
		}

		// Only re-create the compressed file if there was more than a second between the mod times.
		// On macOS the gzip seems to truncate the nanoseconds in the mod time causing the original file
		// to look newer than the gzipped file.
		if fileInfoOriginal.ModTime().Sub(fileInfo.ModTime()) >= time.Second {
			// 压缩文件过期，关闭并移除文件系统，重新创建压缩文件。
			// The compressed file became stale. Re-create it.
			_ = f.Close()
			_ = os.Remove(filePath)
			return fF.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}
	}

	// 至此，压缩文件和对应的源文件都存在，且：1.压缩文件没有过期 2. 压缩文件和源文件够可以打开
	return fF.newFSFile(f, fileInfo, mustCompress, filePath, fileEncoding)
}

// filepath 文件路径名
// f filepath对应的fs.File
// fileInfo filepath对应的fs.FileInfo
// compressed filepath是否是压缩文件
// fileEncoding filepath是这个格式的压缩文件
func (fF *fsFile) newFSFile(f fs.File, fileInfo fs.FileInfo, compressed bool, filePath, fileEncoding string) error {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		_ = f.Close()
		return fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed, fF.h.compressedFileSuffixes[fileEncoding])
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		data, err := readFileHeader(f, compressed, fileEncoding)
		if err != nil {
			return fmt.Errorf("cannot read header of the file %q: %w", fileInfo.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()

	fF.f = f
	fF.filename = append(fF.filename, filePath...)
	fF.contentType = append(fF.contentType, contentType...)
	fF.contentLength = contentLength
	fF.compressed = compressed
	fF.lastModified = lastModified
	fF.t = time.Now()
	fF.lastModifiedStr = append(fF.lastModifiedStr, AppendHTTPDate(nil, lastModified)...)
	return nil
}

func readFileHeader(f io.Reader, compressed bool, fileEncoding string) ([]byte, error) {
	r := f
	var (
		br *brotli.Reader
		zr *gzip.Reader
	)
	if compressed {
		var err error
		if fileEncoding == "br" {
			if br, err = acquireBrotliReader(f); err != nil {
				return nil, err
			}
			r = br
		} else if fileEncoding == "gzip" {
			if zr, err = acquireGzipReader(f); err != nil {
				return nil, err
			}
			r = zr
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

var filesLockMap sync.Map

func getFileLock(absPath string) *sync.Mutex {
	v, _ := filesLockMap.LoadOrStore(absPath, &sync.Mutex{})
	filelock := v.(*sync.Mutex)
	return filelock
}

var _ fs.FS = (*osFS)(nil)

type osFS struct{}

func (o *osFS) Open(name string) (fs.File, error)     { return os.Open(name) }
func (o *osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
