package azure

import (
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
)

// Backend stores data on an azure endpoint.
type Backend struct {
	accountName string
	container   *storage.Container
	sem         *backend.Semaphore
	prefix      string
	backend.Layout
}

// make sure that *Backend implements backend.Backend
var _ restic.Backend = &Backend{}

func open(cfg Config) (*Backend, error) {
	debug.Log("open, config %#v", cfg)

	client, err := storage.NewBasicClient(cfg.AccountName, cfg.AccountKey)
	if err != nil {
		return nil, errors.Wrap(err, "NewBasicClient")
	}

	client.HTTPClient = &http.Client{Transport: backend.Transport()}

	service := client.GetBlobService()

	sem, err := backend.NewSemaphore(cfg.Connections)
	if err != nil {
		return nil, err
	}

	be := &Backend{
		container:   service.GetContainerReference(cfg.Container),
		accountName: cfg.AccountName,
		sem:         sem,
		prefix:      cfg.Prefix,
		Layout: &backend.DefaultLayout{
			Path: cfg.Prefix,
			Join: path.Join,
		},
	}

	return be, nil
}

// Open opens the Azure backend at specified container.
func Open(cfg Config) (restic.Backend, error) {
	return open(cfg)
}

// Create opens the Azure backend at specified container and creates the container if
// it does not exist yet.
func Create(cfg Config) (restic.Backend, error) {
	be, err := open(cfg)

	if err != nil {
		return nil, errors.Wrap(err, "open")
	}

	options := storage.CreateContainerOptions{
		Access: storage.ContainerAccessTypePrivate,
	}

	_, err = be.container.CreateIfNotExists(&options)
	if err != nil {
		return nil, errors.Wrap(err, "container.CreateIfNotExists")
	}

	return be, nil
}

// IsNotExist returns true if the error is caused by a not existing file.
func (be *Backend) IsNotExist(err error) bool {
	debug.Log("IsNotExist(%T, %#v)", err, err)
	return os.IsNotExist(err)
}

// Join combines path components with slashes.
func (be *Backend) Join(p ...string) string {
	return path.Join(p...)
}

type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (fi fileInfo) Name() string       { return fi.name }    // base name of the file
func (fi fileInfo) Size() int64        { return fi.size }    // length in bytes for regular files; system-dependent for others
func (fi fileInfo) Mode() os.FileMode  { return fi.mode }    // file mode bits
func (fi fileInfo) ModTime() time.Time { return fi.modTime } // modification time
func (fi fileInfo) IsDir() bool        { return fi.isDir }   // abbreviation for Mode().IsDir()
func (fi fileInfo) Sys() interface{}   { return nil }        // underlying data source (can return nil)

// ReadDir returns the entries for a directory.
func (be *Backend) ReadDir(dir string) (list []os.FileInfo, err error) {
	debug.Log("ReadDir(%v)", dir)

	// make sure dir ends with a slash
	if dir[len(dir)-1] != '/' {
		dir += "/"
	}

	obj, err := be.container.ListBlobs(storage.ListBlobsParameters{Prefix: dir, Delimiter: "/"})
	if err != nil {
		return nil, err
	}

	for _, item := range obj.BlobPrefixes {
		entry := fileInfo{
			name:  strings.TrimPrefix(item, dir),
			isDir: true,
			mode:  os.ModeDir | 0755,
		}
		if entry.name != "" {
			list = append(list, entry)
		}
	}

	for _, item := range obj.Blobs {
		entry := fileInfo{
			name:    strings.TrimPrefix(item.Name, dir),
			isDir:   false,
			mode:    0644,
			size:    item.Properties.ContentLength,
			modTime: time.Time(item.Properties.LastModified),
		}
		if entry.name != "" {
			list = append(list, entry)
		}
	}

	return list, nil
}

// Location returns this backend's location (the container name).
func (be *Backend) Location() string {
	return be.Join(be.container.Name, be.prefix)
}

// Path returns the path in the bucket that is used for this backend.
func (be *Backend) Path() string {
	return be.prefix
}

// preventCloser wraps an io.Reader to run a function instead of the original Close() function.
type preventCloser struct {
	io.Reader
	f func()
}

func (wr preventCloser) Close() error {
	wr.f()
	return nil
}

// Save stores data in the backend at the handle.
func (be *Backend) Save(ctx context.Context, h restic.Handle, rd io.Reader) (err error) {
	if err := h.Valid(); err != nil {
		return err
	}

	objName := be.Filename(h)

	debug.Log("Save %v at %v", h, objName)

	// Check key does not already exist
	found, err := be.container.GetBlobReference(objName).Exists()
	if err != nil {
		return errors.Wrap(err, "GetBlobReference().Exists()")
	}
	if found {
		debug.Log("%v already exists", h)
		return errors.New("key already exists")
	}

	be.sem.GetToken()

	// wrap the reader so that net/http client cannot close the reader, return
	// the token instead.
	rd = preventCloser{
		Reader: rd,
		f: func() {
			debug.Log("Close()")
		},
	}

	debug.Log("InsertObject(%v, %v)", be.container.Name, objName)

	err = be.container.GetBlobReference(objName).CreateBlockBlobFromReader(rd, nil)

	be.sem.ReleaseToken()
	debug.Log("%v, err %#v", objName, err)

	return errors.Wrap(err, "CreateBlockBlobFromReader")
}

// wrapReader wraps an io.ReadCloser to run an additional function on Close.
type wrapReader struct {
	io.ReadCloser
	f func()
}

func (wr wrapReader) Close() error {
	err := wr.ReadCloser.Close()
	wr.f()
	return err
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (be *Backend) Load(ctx context.Context, h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v from %v", h, length, offset, be.Filename(h))
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	if length < 0 {
		return nil, errors.Errorf("invalid length %d", length)
	}

	objName := be.Filename(h)
	blob := be.container.GetBlobReference(objName)

	start := uint64(offset)
	var end uint64

	if length > 0 {
		end = uint64(offset + int64(length) - 1)
	} else {
		end = 0
	}

	be.sem.GetToken()

	rd, err := blob.GetRange(&storage.GetBlobRangeOptions{Range: &storage.BlobRange{Start: start, End: end}})
	if err != nil {
		be.sem.ReleaseToken()
		return nil, err
	}

	closeRd := wrapReader{
		ReadCloser: rd,
		f: func() {
			debug.Log("Close()")
			be.sem.ReleaseToken()
		},
	}

	return closeRd, err
}

// Stat returns information about a blob.
func (be *Backend) Stat(ctx context.Context, h restic.Handle) (bi restic.FileInfo, err error) {
	debug.Log("%v", h)

	objName := be.Filename(h)
	blob := be.container.GetBlobReference(objName)

	if err := blob.GetProperties(nil); err != nil {
		debug.Log("blob.GetProperties err %v", err)
		return restic.FileInfo{}, errors.Wrap(err, "blob.GetProperties")
	}

	return restic.FileInfo{Size: int64(blob.Properties.ContentLength)}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (be *Backend) Test(ctx context.Context, h restic.Handle) (bool, error) {
	objName := be.Filename(h)
	found, err := be.container.GetBlobReference(objName).Exists()
	if err != nil {
		return false, err
	}
	return found, nil
}

// Remove removes the blob with the given name and type.
func (be *Backend) Remove(ctx context.Context, h restic.Handle) error {
	objName := be.Filename(h)
	_, err := be.container.GetBlobReference(objName).DeleteIfExists(nil)
	debug.Log("Remove(%v) at %v -> err %v", h, objName, err)
	return errors.Wrap(err, "client.RemoveObject")
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this. If the channel done is closed, sending
// stops.
func (be *Backend) List(ctx context.Context, t restic.FileType) <-chan string {
	debug.Log("listing %v", t)
	ch := make(chan string)

	prefix := be.Dirname(restic.Handle{Type: t})

	// make sure prefix ends with a slash
	if prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}

	go func() {
		defer close(ch)

		obj, err := be.container.ListBlobs(storage.ListBlobsParameters{Prefix: prefix})
		if err != nil {
			return
		}

		for _, item := range obj.Blobs {
			m := strings.TrimPrefix(item.Name, prefix)
			if m == "" {
				continue
			}

			select {
			case ch <- path.Base(m):
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}

// Remove keys for a specified backend type.
func (be *Backend) removeKeys(ctx context.Context, t restic.FileType) error {
	for key := range be.List(ctx, restic.DataFile) {
		err := be.Remove(ctx, restic.Handle{Type: restic.DataFile, Name: key})
		if err != nil {
			return err
		}
	}

	return nil
}

// Delete removes all restic keys in the bucket. It will not remove the bucket itself.
func (be *Backend) Delete(ctx context.Context) error {
	alltypes := []restic.FileType{
		restic.DataFile,
		restic.KeyFile,
		restic.LockFile,
		restic.SnapshotFile,
		restic.IndexFile}

	for _, t := range alltypes {
		err := be.removeKeys(ctx, t)
		if err != nil {
			return nil
		}
	}

	return be.Remove(ctx, restic.Handle{Type: restic.ConfigFile})
}

// Close does nothing
func (be *Backend) Close() error { return nil }
