package filesystem

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

const DefaultMaxReadBytes int64 = 10 << 20

type Options struct {
	MaxReadBytes int64
	TrashDir     string
}
type Service struct {
	access              *access.Manager
	maxReadBytes        int64
	trashDir            string
	beforeRead          func() error                                   // test hook; nil in normal services
	beforeReplace       func() error                                   // test hook after temp close/context check; nil in normal services
	beforeSwap          func() error                                   // test hook after final revision check; nil in normal services
	beforeRollback      func() error                                   // test hook before rollback swap; nil in normal services
	renameSwap          func(*os.File, string, string) error           // semantic exchange test hook
	beforeReadAt        func(string) error                             // test hook; nil in normal services
	afterReadAtStat     func(string) error                             // test hook; nil in normal services
	afterDisplacedRead  func() error                                   // test hook after a displaced inode is verified
	afterRollbackRead   func(*os.File, string) error                   // test hook after a rollback artifact is verified
	beforeCreatePublish func() error                                   // test hook after private create temp is durable
	writeFile           func(*os.File, []byte) error                   // test hook; nil uses writeAll
	syncFile            func(*os.File) error                           // test hook; nil uses Sync
	renameAt            func(*os.File, string, *os.File, string) error // no-replace rename test hook
	beforeTrashValidate func() error                                   // test hook before configured Trash revalidation; nil in normal services
	beforeTrashRename   func() error                                   // test hook after Trash descriptor pinning; nil in normal services
	beforeDeleteEntry   func(string) error                             // test hook; nil in normal services
	readDirectory       func(*os.File, int) ([]os.DirEntry, error)     // test hook; nil uses ReadDir
}
type TextFile struct {
	Root, Path, Content, Revision string
	Size                          int64
}
type BinaryKind string

const (
	BinaryImage    BinaryKind = "image"
	BinaryAudio    BinaryKind = "audio"
	BinaryResource BinaryKind = "resource"
)

type BinaryFile struct {
	Root, Path, MIMEType, Revision, URI string
	Kind                                BinaryKind
	Size                                int64
	Data                                []byte
}

func New(manager *access.Manager, options Options) *Service {
	max := options.MaxReadBytes
	if max <= 0 {
		max = DefaultMaxReadBytes
	}
	return &Service{access: manager, maxReadBytes: max, trashDir: options.TrashDir}
}

func (s *Service) ReadText(ctx context.Context, root, path string) (TextFile, error) {
	if err := ctx.Err(); err != nil {
		return TextFile{}, newError(root, path, config.OpRead, "filesystem_unavailable", err)
	}
	p, err := s.access.Resolve(root, path, config.OpRead, access.Existing)
	if err != nil {
		return TextFile{}, MapError(root, path, config.OpRead, err)
	}
	f, err := p.Open(os.O_RDONLY, 0)
	if err != nil {
		return TextFile{}, MapError(root, path, config.OpRead, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return TextFile{}, MapError(root, path, config.OpRead, err)
	}
	if !info.Mode().IsRegular() {
		return TextFile{}, newError(root, path, config.OpRead, "unsupported_file_type", nil)
	}
	if info.Size() > s.maxReadBytes {
		return TextFile{}, newError(root, path, config.OpRead, "response_too_large", nil)
	}
	if s.beforeRead != nil {
		if err := s.beforeRead(); err != nil {
			return TextFile{}, MapError(root, path, config.OpRead, err)
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, s.maxReadBytes+1))
	if err != nil {
		return TextFile{}, MapError(root, path, config.OpRead, err)
	}
	if int64(len(b)) > s.maxReadBytes {
		return TextFile{}, newError(root, path, config.OpRead, "response_too_large", nil)
	}
	if !utf8.Valid(b) {
		return TextFile{}, newError(root, path, config.OpRead, "invalid_utf8", nil)
	}
	return TextFile{Root: root, Path: path, Content: string(b), Revision: Revision(b), Size: int64(len(b))}, nil
}

func (s *Service) ReadBinary(ctx context.Context, root, path string) (BinaryFile, error) {
	if err := ctx.Err(); err != nil {
		return BinaryFile{}, newError(root, path, config.OpRead, "filesystem_unavailable", err)
	}
	p, err := s.access.Resolve(root, path, config.OpRead, access.Existing)
	if err != nil {
		return BinaryFile{}, MapError(root, path, config.OpRead, err)
	}
	f, err := p.Open(os.O_RDONLY, 0)
	if err != nil {
		return BinaryFile{}, MapError(root, path, config.OpRead, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return BinaryFile{}, MapError(root, path, config.OpRead, err)
	}
	if !info.Mode().IsRegular() {
		return BinaryFile{}, newError(root, path, config.OpRead, "unsupported_file_type", nil)
	}
	if info.Size() > s.maxReadBytes {
		return BinaryFile{}, newError(root, path, config.OpRead, "response_too_large", nil)
	}
	if s.beforeRead != nil {
		if err := s.beforeRead(); err != nil {
			return BinaryFile{}, MapError(root, path, config.OpRead, err)
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, s.maxReadBytes+1))
	if err != nil {
		return BinaryFile{}, MapError(root, path, config.OpRead, err)
	}
	if int64(len(b)) > s.maxReadBytes {
		return BinaryFile{}, newError(root, path, config.OpRead, "response_too_large", nil)
	}
	if err := ctx.Err(); err != nil {
		return BinaryFile{}, newError(root, path, config.OpRead, "filesystem_unavailable", err)
	}
	mimeType := http.DetectContentType(b)
	if mimeType == "application/octet-stream" {
		if ext := mime.TypeByExtension(filepath.Ext(path)); ext != "" {
			mimeType = ext
		}
	}
	kind := BinaryResource
	if len(mimeType) >= 6 && mimeType[:6] == "image/" {
		kind = BinaryImage
	} else if len(mimeType) >= 6 && mimeType[:6] == "audio/" {
		kind = BinaryAudio
	}
	return BinaryFile{Root: root, Path: path, MIMEType: mimeType, Revision: Revision(b), URI: resourceURI(root, path), Kind: kind, Size: int64(len(b)), Data: b}, nil
}

func resourceURI(root, path string) string {
	return "scopedfs://" + url.PathEscape(root) + "/" + url.PathEscape(path)
}
